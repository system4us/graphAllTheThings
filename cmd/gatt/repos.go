package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/lock"
	"graphallthethings/internal/mcpserver"
	"graphallthethings/internal/store"
)

var (
	errBadRepo        = errors.New("repo must be an absolute directory path")
	errRepoNotMounted = errors.New("repo path not found inside server")
)

type repoMeta struct {
	Root      string    `json:"root"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

type repoEntry struct {
	key        string
	root       string
	graphPath  string
	coalescer  *coalescer
	mcp        *mcpserver.Server
	mcpHandler http.Handler
}

type repoStatus struct {
	Root     string      `json:"root"`
	Key      string      `json:"key"`
	Head     string      `json:"head,omitempty"`
	Running  bool        `json:"running"`
	Queued   bool        `json:"queued"`
	Last     *syncStatus `json:"last,omitempty"`
	Progress *progress   `json:"progress,omitempty"`
}

type progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

func (e *repoEntry) status() repoStatus {
	running, queued, last := e.coalescer.Status()
	st := repoStatus{
		Root:    e.root,
		Key:     e.key,
		Head:    shortSHA(gitHeadSHA(e.root)),
		Running: running,
		Queued:  queued,
		Last:    last,
	}
	if running && e.coalescer.pTotal.Load() > 0 {
		st.Progress = &progress{Done: e.coalescer.pDone.Load(), Total: e.coalescer.pTotal.Load()}
	}
	return st
}

// repoKey derives a deterministic, filesystem-safe directory name for a repo
// path: <sanitized-basename>-<first-8-hex-of-sha256>.
func repoKey(absPath string) string {
	clean := filepath.Clean(absPath)
	base := filepath.Base(clean)
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "._")
	if name == "" {
		name = "repo"
	}
	sum := sha256.Sum256([]byte(clean))
	return fmt.Sprintf("%s-%x", name, sum[:4])
}

// repoPool manages multiple repositories in a central `gatt serve --data-dir`
// instance. Each mounted repository gets its own SQLite graph, vector index,
// background sync coalescer, and MCP Streamable HTTP handler.
type repoPool struct {
	ctx      context.Context
	dataDir  string
	baseSync syncConfig
	// indexSem serializes background sync/embed passes across repos so scanning
	// dozens of repositories doesn't stampede the CPU-bound ONNX worker.
	indexSem chan struct{}

	mu       sync.Mutex
	repos    map[string]*repoEntry
	inFlight map[string]chan struct{}
}

func newRepoPool(ctx context.Context, dataDir string, base syncConfig) *repoPool {
	return &repoPool{
		ctx:      ctx,
		dataDir:  dataDir,
		baseSync: base,
		indexSem: make(chan struct{}, 1),
		repos:    map[string]*repoEntry{},
		inFlight: map[string]chan struct{}{},
	}
}

// containerRepoPath translates a host path into the POSIX path mounted inside
// the Linux Docker container. POSIX paths (/home/...) pass through unchanged;
// Windows drive paths (C:\Users\foo or C:/Users/foo) map to /host/c/Users/foo
// so `-v C:\Users\foo:/host/c/Users/foo:ro`, `gatt install --http`, and
// `gatt notify` agree on the same container path.
func containerRepoPath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		drive := strings.ToLower(string(p[0]))
		rest := strings.ReplaceAll(p[2:], "\\", "/")
		rest = strings.TrimLeft(rest, "/")
		if rest == "" {
			return "/host/" + drive
		}
		return "/host/" + drive + "/" + strings.TrimRight(rest, "/")
	}
	return p
}

// Get returns the repoEntry for rawPath, initializing and extracting its graph
// on first access. Concurrent callers for the same path wait on one init.
func (p *repoPool) Get(ctx context.Context, rawPath string) (*repoEntry, error) {
	rawPath = containerRepoPath(rawPath)
	if rawPath == "" {
		return nil, fmt.Errorf("%w: missing repo parameter (?repo=/abs/path or X-Gatt-Repo header)", errBadRepo)
	}
	clean := filepath.Clean(rawPath)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("%w: %q", errBadRepo, rawPath)
	}
	fi, err := os.Stat(clean)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q (mount the repository at the same absolute path inside the container)", errRepoNotMounted, clean)
	}

	for {
		p.mu.Lock()
		if e, ok := p.repos[clean]; ok {
			p.mu.Unlock()
			return e, nil
		}
		if ch, ok := p.inFlight[clean]; ok {
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		p.inFlight[clean] = ch
		p.mu.Unlock()

		entry, err := p.initRepo(ctx, clean)

		p.mu.Lock()
		if err == nil {
			p.repos[clean] = entry
		}
		delete(p.inFlight, clean)
		close(ch)
		p.mu.Unlock()

		return entry, err
	}
}

func (p *repoPool) initRepo(ctx context.Context, cleanRoot string) (*repoEntry, error) {
	key := repoKey(cleanRoot)
	repoDir := filepath.Join(p.dataDir, "repos", key)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return nil, fmt.Errorf("create repo data dir: %w", err)
	}

	metaPath := filepath.Join(repoDir, "meta.json")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		metaBytes, _ := json.MarshalIndent(repoMeta{
			Root:      cleanRoot,
			Key:       key,
			CreatedAt: time.Now().UTC(),
		}, "", "  ")
		_ = os.WriteFile(metaPath, metaBytes, 0o644)
	}

	graphPath := filepath.Join(repoDir, "graph.db")
	if _, err := os.Stat(graphPath); os.IsNotExist(err) {
		if err := extractInitialCodebase(ctx, cleanRoot, graphPath); err != nil {
			return nil, fmt.Errorf("initial extract %s: %w", cleanRoot, err)
		}
	}

	coll := p.baseSync.coll
	if p.baseSync.qdURL != "" {
		coll = coll + "-" + key
	}

	sc := p.baseSync
	sc.graphPath = graphPath
	sc.coll = coll

	entry := &repoEntry{
		key:       key,
		root:      cleanRoot,
		graphPath: graphPath,
	}

	mcpCfg := mcpserver.Config{
		GraphPath:  graphPath,
		SourceKind: "codebase",
		Source:     cleanRoot,
		OnAutoRefresh: func() {
			if entry.coalescer != nil {
				entry.coalescer.Trigger()
			}
		},
	}
	if !sc.noEmbed {
		vs := openStore(graphPath, sc.qdURL, coll)
		model := sc.embModel
		if model == "" {
			model = resolveModel("", vs)
		}
		mcpCfg.OpenStore = func() store.VectorStore { return openStore(graphPath, sc.qdURL, coll) }
		mcpCfg.Embedder = embed.New(sc.embURL, model)
		mcpCfg.EmbModel = model
	}

	srv, err := mcpserver.New(mcpCfg)
	if err != nil {
		return nil, fmt.Errorf("mcp init %s: %w", cleanRoot, err)
	}
	entry.mcp = srv
	entry.mcpHandler = srv.HTTPHandler(nil)

	entry.coalescer = newCoalescer(func() syncStatus {
		select {
		case p.indexSem <- struct{}{}:
			defer func() { <-p.indexSem }()
		case <-p.ctx.Done():
			return syncStatus{At: time.Now(), Error: p.ctx.Err().Error()}
		}
		st := syncOnce(p.ctx, sc)
		if st.Error == "" && (st.Summary != "" || st.Embedded > 0) {
			_ = entry.mcp.Reload()
		}
		return st
	})
	sc.progress = entry.coalescer.SetProgress

	// Kick off an initial catch-up pass (refresh + embeddings) in the background.
	entry.coalescer.Trigger()
	return entry, nil
}

func extractInitialCodebase(ctx context.Context, root, graphPath string) error {
	release, err := lock.Acquire(graphPath+".lock", 2*time.Minute)
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Stat(graphPath); err == nil {
		return nil
	}
	g, err := codebase.New(root).Extract(ctx)
	if err != nil {
		return err
	}
	g.ExtractedAt = time.Now()
	return g.Save(graphPath)
}

// LoadExisting pre-loads previously registered repos found under dataDir/repos
// (whose mounted root still exists) plus any explicit paths from extraRepos and
// any git repositories discovered under scanDirs (up to scanDepth levels deep).
func (p *repoPool) LoadExisting(ctx context.Context, extraRepos, scanDirs []string, scanDepth int) {
	seen := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(p.dataDir, "repos", "*", "meta.json"))
	for _, mp := range matches {
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var m repoMeta
		if err := json.Unmarshal(data, &m); err != nil || m.Root == "" {
			continue
		}
		if fi, err := os.Stat(m.Root); err != nil || !fi.IsDir() {
			continue
		}
		seen[filepath.Clean(m.Root)] = true
		if _, err := p.Get(ctx, m.Root); err != nil {
			fmt.Fprintf(os.Stderr, "gatt serve: preload %s skipped: %v\n", m.Root, err)
		}
	}
	for _, r := range extraRepos {
		if ctx.Err() != nil {
			return
		}
		r = containerRepoPath(r)
		if r == "" || seen[filepath.Clean(r)] {
			continue
		}
		seen[filepath.Clean(r)] = true
		if _, err := p.Get(ctx, r); err != nil {
			fmt.Fprintf(os.Stderr, "gatt serve: preload %s skipped: %v\n", r, err)
		}
	}
	p.ScanNew(ctx, scanDirs, scanDepth)
}

// ScanNew discovers git repositories under scanDirs (up to scanDepth levels
// deep) and registers any that are not yet loaded in the pool.
func (p *repoPool) ScanNew(ctx context.Context, scanDirs []string, scanDepth int) {
	if len(scanDirs) == 0 {
		return
	}
	found := discoverGitRepos(scanDirs, scanDepth)
	var added int
	for _, root := range found {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		_, exists := p.repos[root]
		p.mu.Unlock()
		if exists {
			continue
		}
		if _, err := p.Get(ctx, root); err != nil {
			fmt.Fprintf(os.Stderr, "gatt serve: scan %s skipped: %v\n", root, err)
			continue
		}
		added++
	}
	if added > 0 {
		fmt.Fprintf(os.Stderr, "gatt serve: discovered %d git repo(s) under %s (indexing sequentially in background)\n",
			added, strings.Join(scanDirs, ", "))
	}
}

// discoverGitRepos walks each directory in dirs up to maxDepth levels deep and
// returns every directory containing a .git entry (repo or worktree), without
// recursing inside discovered repositories.
func discoverGitRepos(dirs []string, maxDepth int) []string {
	if maxDepth <= 0 {
		maxDepth = 2
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range dirs {
		root := containerRepoPath(raw)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		fi, err := os.Stat(root)
		if err != nil || !fi.IsDir() {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			isRoot := path == root
			if !isRoot && graph.SkipDir(d.Name(), false) {
				return filepath.SkipDir
			}
			// Check if this directory is a git repo or worktree (.git dir or file).
			if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
				if !seen[path] {
					seen[path] = true
					out = append(out, path)
				}
				return filepath.SkipDir
			}
			if !isRoot {
				rel, err := filepath.Rel(root, path)
				if err == nil {
					depth := strings.Count(filepath.ToSlash(rel), "/") + 1
					if depth >= maxDepth {
						return filepath.SkipDir
					}
				}
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

func (p *repoPool) List() []repoStatus {
	p.mu.Lock()
	entries := make([]*repoEntry, 0, len(p.repos))
	for _, e := range p.repos {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	out := make([]repoStatus, len(entries))
	for i, e := range entries {
		out[i] = e.status()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out
}

func (p *repoPool) RefreshAll() map[string]string {
	p.mu.Lock()
	entries := make([]*repoEntry, 0, len(p.repos))
	for _, e := range p.repos {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	states := make(map[string]string, len(entries))
	for _, e := range entries {
		states[e.root] = e.coalescer.Trigger()
	}
	return states
}

func (p *repoPool) Roots() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.repos))
	for root := range p.repos {
		out = append(out, root)
	}
	return out
}

func (p *repoPool) Trigger(root string) {
	p.mu.Lock()
	e := p.repos[root]
	p.mu.Unlock()
	if e != nil && e.coalescer != nil {
		e.coalescer.Trigger()
	}
}

// Resolve finds a repoEntry by absolute path (auto-extracting if mounted),
// by unique repository basename (e.g. "ducksecrets"), or — when query is empty
// and exactly one repo is loaded — that single repo.
func (p *repoPool) Resolve(ctx context.Context, query string) (*repoEntry, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		p.mu.Lock()
		n := len(p.repos)
		var only *repoEntry
		if n == 1 {
			for _, e := range p.repos {
				only = e
			}
		}
		p.mu.Unlock()
		if only != nil {
			return only, nil
		}
		return nil, fmt.Errorf("repo is required when %d repositories are registered (pass repo=<abs-path-or-name> or call list_repos)", n)
	}

	mapped := containerRepoPath(q)
	if filepath.IsAbs(mapped) {
		return p.Get(ctx, mapped)
	}

	// Match by basename (case-insensitive) among loaded repos.
	lower := strings.ToLower(q)
	p.mu.Lock()
	var matches []*repoEntry
	for _, e := range p.repos {
		if strings.ToLower(filepath.Base(e.root)) == lower || strings.ToLower(e.key) == lower {
			matches = append(matches, e)
		}
	}
	p.mu.Unlock()
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		var paths []string
		for _, m := range matches {
			paths = append(paths, m.root)
		}
		sort.Strings(paths)
		return nil, fmt.Errorf("ambiguous repo name %q matches %s; pass the full path", q, strings.Join(paths, ", "))
	}
	return nil, fmt.Errorf("repo %q not found; pass the full mounted path or call list_repos", q)
}

// requestRepo extracts the target repo path from ?repo=... or X-Gatt-Repo.
func requestRepo(r *http.Request) string {
	if q := strings.TrimSpace(r.URL.Query().Get("repo")); q != "" {
		return q
	}
	return strings.TrimSpace(r.Header.Get("X-Gatt-Repo"))
}
