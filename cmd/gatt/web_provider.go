package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"graphallthethings/internal/engine"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/httpapi"
	"graphallthethings/internal/mcpserver"
)

// singleRepoProvider backs the web UI in single-repo `gatt serve` mode.
// When mcpSrv is non-nil it reuses the live MCP engine; otherwise it lazily
// loads and caches the graph from disk (reloading whenever the file mtime changes).
type singleRepoProvider struct {
	graphPath string
	coalescer *coalescer
	mcpSrv    *mcpserver.Server

	mu      sync.Mutex
	cached  *engine.Engine
	modTime time.Time
}

func (p *singleRepoProvider) Mode() string { return "single" }

func (p *singleRepoProvider) ListRepos() []httpapi.RepoInfo {
	running, queued, _ := p.coalescer.Status()
	root := p.graphPath
	if p.mcpSrv != nil {
		if e, _, err := p.mcpSrv.Engine(context.Background(), false); err == nil && e != nil && e.G != nil {
			root = strings.TrimPrefix(e.G.Source, "codebase:")
		}
	} else if src, _, err := graph.LoadCodebaseState(p.graphPath); err == nil && src != "" {
		root = strings.TrimPrefix(src, "codebase:")
	}
	return []httpapi.RepoInfo{{
		Root:    root,
		Key:     "default",
		Running: running,
		Queued:  queued,
	}}
}

func (p *singleRepoProvider) EngineFor(ctx context.Context, _ string) (*engine.Engine, string, error) {
	if p.mcpSrv != nil {
		e, _, err := p.mcpSrv.Engine(ctx, false)
		if err != nil {
			return nil, "", err
		}
		root := p.graphPath
		if e != nil && e.G != nil {
			root = strings.TrimPrefix(e.G.Source, "codebase:")
		}
		return e, root, nil
	}
	if p.graphPath == "" {
		return nil, "", fmt.Errorf("no graph configured")
	}
	fi, err := os.Stat(p.graphPath)
	if err != nil {
		return nil, "", fmt.Errorf("graph not found at %s (run `gatt extract codebase .` first)", p.graphPath)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && !fi.ModTime().After(p.modTime) {
		root := strings.TrimPrefix(p.cached.G.Source, "codebase:")
		return p.cached, root, nil
	}
	g, err := graph.Load(p.graphPath)
	if err != nil {
		return nil, "", err
	}
	p.cached = engine.New(g, nil, nil)
	p.modTime = fi.ModTime()
	return p.cached, strings.TrimPrefix(g.Source, "codebase:"), nil
}

func (p *singleRepoProvider) TriggerRefresh(_ context.Context, _ string) (string, error) {
	if p.coalescer == nil {
		return "", fmt.Errorf("refresh unavailable")
	}
	return p.coalescer.Trigger(), nil
}

func (p *singleRepoProvider) GraphPathFor(_ context.Context, _ string) (string, error) {
	if p.graphPath == "" {
		return "", fmt.Errorf("no graph configured")
	}
	return p.graphPath, nil
}

func (p *singleRepoProvider) ReloadRepo(_ context.Context, _ string) error {
	if p.mcpSrv != nil {
		return p.mcpSrv.Reload()
	}
	p.mu.Lock()
	p.cached = nil
	p.modTime = time.Time{}
	p.mu.Unlock()
	return nil
}

func (p *singleRepoProvider) StatusFor(_ context.Context, _ string) (httpapi.RepoStatus, error) {
	if p.coalescer == nil {
		return httpapi.RepoStatus{}, nil
	}
	running, queued, last := p.coalescer.Status()
	out := httpapi.RepoStatus{
		Running:      running,
		Queued:       queued,
		ProgressDone: p.coalescer.pDone.Load(),
		ProgressTot:  p.coalescer.pTotal.Load(),
	}
	if last != nil {
		if !last.At.IsZero() {
			out.LastAt = last.At.UTC().Format(time.RFC3339)
		}
		out.LastDuration = last.Duration
		out.LastSummary = last.Summary
		out.LastEmbedded = last.Embedded
		out.LastNote = last.Note
		out.LastError = last.Error
	}
	return out, nil
}

// multiRepoProvider backs the web UI in multi-repo `gatt serve --data-dir` mode.
type multiRepoProvider struct {
	pool *repoPool
}

func (p *multiRepoProvider) Mode() string { return "multi" }

func (p *multiRepoProvider) ListRepos() []httpapi.RepoInfo {
	list := p.pool.List()
	out := make([]httpapi.RepoInfo, 0, len(list))
	for _, r := range list {
		out = append(out, httpapi.RepoInfo{
			Root:    r.Root,
			Key:     r.Key,
			Head:    r.Head,
			Running: r.Running,
			Queued:  r.Queued,
		})
	}
	return out
}

func (p *multiRepoProvider) EngineFor(ctx context.Context, repo string) (*engine.Engine, string, error) {
	if repo == "" {
		list := p.pool.List()
		if len(list) == 0 {
			return nil, "", fmt.Errorf("no repositories indexed yet")
		}
		repo = list[0].Root
	}
	entry, err := p.pool.Get(ctx, repo)
	if err != nil {
		return nil, "", err
	}
	e, _, err := entry.mcp.Engine(ctx, false)
	if err != nil {
		return nil, "", err
	}
	return e, entry.root, nil
}

func (p *multiRepoProvider) TriggerRefresh(ctx context.Context, repo string) (string, error) {
	if repo != "" {
		entry, err := p.pool.Get(ctx, repo)
		if err != nil {
			return "", err
		}
		return entry.coalescer.Trigger(), nil
	}
	p.pool.RefreshAll()
	return "started", nil
}

func (p *multiRepoProvider) resolveEntry(ctx context.Context, repo string) (*repoEntry, error) {
	if repo == "" {
		list := p.pool.List()
		if len(list) == 0 {
			return nil, fmt.Errorf("no repositories indexed yet")
		}
		repo = list[0].Root
	}
	return p.pool.Get(ctx, repo)
}

func (p *multiRepoProvider) GraphPathFor(ctx context.Context, repo string) (string, error) {
	entry, err := p.resolveEntry(ctx, repo)
	if err != nil {
		return "", err
	}
	return entry.graphPath, nil
}

func (p *multiRepoProvider) ReloadRepo(ctx context.Context, repo string) error {
	entry, err := p.resolveEntry(ctx, repo)
	if err != nil {
		return err
	}
	return entry.mcp.Reload()
}

func (p *multiRepoProvider) StatusFor(ctx context.Context, repo string) (httpapi.RepoStatus, error) {
	entry, err := p.resolveEntry(ctx, repo)
	if err != nil {
		return httpapi.RepoStatus{}, err
	}
	st := entry.status()
	out := httpapi.RepoStatus{
		Running: st.Running,
		Queued:  st.Queued,
	}
	if st.Progress != nil {
		out.ProgressDone = st.Progress.Done
		out.ProgressTot = st.Progress.Total
	}
	if st.Last != nil {
		if !st.Last.At.IsZero() {
			out.LastAt = st.Last.At.UTC().Format(time.RFC3339)
		}
		out.LastDuration = st.Last.Duration
		out.LastSummary = st.Last.Summary
		out.LastEmbedded = st.Last.Embedded
		out.LastNote = st.Last.Note
		out.LastError = st.Last.Error
	}
	return out, nil
}
