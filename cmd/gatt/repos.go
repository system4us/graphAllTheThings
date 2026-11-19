package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/embed"
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
// instance. Each mounted repository gets its own SQLite graph, local vector
// index, background sync coalescer, and MCP Streamable HTTP handler.
type repoPool struct {
	ctx      context.Context
	dataDir  string
	baseSync syncConfig

	mu       sync.Mutex
	repos    map[string]*repoEntry
	inFlight map[string]chan struct{}
}

func newRepoPool(ctx context.Context, dataDir string, base syncConfig) *repoPool {
	return &repoPool{
		ctx:      ctx,
		dataDir:  dataDir,
		baseSync: base,
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
// (whose mounted root still exists) plus any explicit paths from extraRepos.
func (p *repoPool) LoadExisting(ctx context.Context, extraRepos []string) {
	seen := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(p.dataDir, "repos", "*", "meta.json"))
	for _, mp := range matches {
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
		r = strings.TrimSpace(r)
		if r == "" || seen[filepath.Clean(r)] {
			continue
		}
		if _, err := p.Get(ctx, r); err != nil {
			fmt.Fprintf(os.Stderr, "gatt serve: preload %s skipped: %v\n", r, err)
		}
	}
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

// requestRepo extracts the target repo path from ?repo=... or X-Gatt-Repo.
func requestRepo(r *http.Request) string {
	if q := strings.TrimSpace(r.URL.Query().Get("repo")); q != "" {
		return q
	}
	return strings.TrimSpace(r.Header.Get("X-Gatt-Repo"))
}
