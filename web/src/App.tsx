import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api } from './api';
import { GraphCanvas } from './GraphCanvas';
import { Inspector } from './Inspector';
import { Sidebar } from './Sidebar';
import {
  nodeColor,
  type GraphEdge,
  type GraphNode,
  type GraphResponse,
  type GraphView,
  type InfoResponse,
  type LayoutName,
  type NodeDetailResponse,
  type OverviewResponse,
  type RepoStatus,
  type SavedBookmark,
  type SearchHit,
  type TreeNode,
} from './types';

function shortRepoName(root: string): string {
  const clean = root.replace(/\/+$/, '');
  const idx = clean.lastIndexOf('/');
  return idx >= 0 ? clean.slice(idx + 1) : clean;
}

export default function App() {
  const [info, setInfo] = useState<InfoResponse | null>(null);
  const [repo, setRepo] = useState<string>('');
  const [overview, setOverview] = useState<OverviewResponse | null>(null);
  const [tree, setTree] = useState<TreeNode[]>([]);
  const [graph, setGraph] = useState<GraphResponse | null>(null);
  const [graphLoading, setGraphLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // View / query controls
  const [view, setView] = useState<GraphView>('architecture');
  const [focusId, setFocusId] = useState<string>('');
  const [selectedPkg, setSelectedPkg] = useState<string>('');
  const [limit, setLimit] = useState<number>(45);
  const [depth, setDepth] = useState<number>(2);
  const [layout, setLayout] = useState<LayoutName>('dagre-lr');
  const [groupByPackage, setGroupByPackage] = useState<boolean>(true);
  const [includeExternal, setIncludeExternal] = useState<boolean>(false);
  const [selectedNodeTypes, setSelectedNodeTypes] = useState<string[]>([]);

  // Inspector
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [nodeDetail, setNodeDetail] = useState<NodeDetailResponse | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);

  // Global search
  const [searchQuery, setSearchQuery] = useState('');
  const [searchHits, setSearchHits] = useState<SearchHit[]>([]);
  const [searchMethod, setSearchMethod] = useState('');
  const [searchOpen, setSearchOpen] = useState(false);
  const searchInputRef = useRef<HTMLInputElement | null>(null);

  // Refresh & live status state
  const [refreshing, setRefreshing] = useState(false);
  const [repoStatus, setRepoStatus] = useState<RepoStatus | null>(null);
  const [overlayLabel, setOverlayLabel] = useState<string>('');
  const [nodeRefreshTick, setNodeRefreshTick] = useState(0);
  const [bookmarks, setBookmarks] = useState<SavedBookmark[]>(() => {
    try {
      const raw = localStorage.getItem('gatt.bookmarks.v1');
      return raw ? (JSON.parse(raw) as SavedBookmark[]) : [];
    } catch {
      return [];
    }
  });

  const saveBookmarks = (next: SavedBookmark[]) => {
    setBookmarks(next);
    try {
      localStorage.setItem('gatt.bookmarks.v1', JSON.stringify(next));
    } catch {
      // ignore storage quota errors
    }
  };

  // Initial load: server info & repos
  useEffect(() => {
    api
      .info()
      .then((res) => {
        setInfo(res);
        if (res.default_repo) setRepo(res.default_repo);
      })
      .catch((err) => setError(err.message));
  }, []);

  // Keyboard shortcuts: / focuses search, Escape closes inspector/search
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === '/' && document.activeElement?.tagName !== 'INPUT') {
        e.preventDefault();
        searchInputRef.current?.focus();
      } else if (e.key === 'Escape') {
        setSearchOpen(false);
        setSelectedId(null);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  // Load overview & tree when repo changes
  useEffect(() => {
    if (!info) return;
    setError(null);
    setSelectedId(null);
    setNodeDetail(null);
    setFocusId('');
    setSelectedPkg('');

    Promise.all([api.overview(repo || undefined), api.tree(repo || undefined)])
      .then(([ov, tr]) => {
        setOverview(ov);
        setTree(tr.children ?? []);
        if (!ov.is_codebase) {
          setView('models');
        }
      })
      .catch((err) => setError(err.message));
  }, [info, repo]);

  // Poll live /api/status (faster while syncing/embedding, slower when idle)
  useEffect(() => {
    if (!info) return;
    let cancelled = false;
    let timer: number | undefined;
    const poll = () => {
      api
        .status(repo || undefined)
        .then((st) => {
          if (cancelled) return;
          setRepoStatus(st);
          timer = window.setTimeout(poll, st.running || st.queued ? 1500 : 10000);
        })
        .catch(() => {
          if (!cancelled) timer = window.setTimeout(poll, 12000);
        });
    };
    poll();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [info, repo]);

  // Fetch subgraph whenever view/filters/repo change
  useEffect(() => {
    if (!info) return;
    if (view === 'diff' || view === 'join' || view === 'context') return;
    if ((view === 'neighbors' || view === 'impact') && !focusId) return;

    let cancelled = false;
    setGraphLoading(true);
    setError(null);

    api
      .graph({
        repo: repo || undefined,
        view,
        focus: focusId || undefined,
        pkg: selectedPkg || undefined,
        depth,
        limit,
        types: selectedNodeTypes,
        includeExternal,
      })
      .then((res) => {
        if (!cancelled) setGraph(res);
      })
      .catch((err) => {
        if (!cancelled) setError(err.message);
      })
      .finally(() => {
        if (!cancelled) setGraphLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, [info, repo, view, focusId, selectedPkg, depth, limit, selectedNodeTypes, includeExternal]);

  // Fetch node details when selectedId changes
  useEffect(() => {
    if (!selectedId) {
      setNodeDetail(null);
      return;
    }
    let cancelled = false;
    setDetailLoading(true);
    api
      .node(selectedId, repo || undefined)
      .then((res) => {
        if (!cancelled) setNodeDetail(res);
      })
      .catch(() => {
        if (!cancelled) setNodeDetail(null);
      })
      .finally(() => {
        if (!cancelled) setDetailLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedId, repo, nodeRefreshTick]);

  // Debounced search
  useEffect(() => {
    const q = searchQuery.trim();
    if (!q) {
      setSearchHits([]);
      setSearchMethod('');
      return;
    }
    const t = setTimeout(() => {
      api
        .search(q, repo || undefined)
        .then((res) => {
          setSearchHits(res.hits ?? []);
          setSearchMethod(res.method);
          setSearchOpen(true);
        })
        .catch(() => setSearchHits([]));
    }, 180);
    return () => clearTimeout(t);
  }, [searchQuery, repo]);

  const switchMainView = (next: GraphView) => {
    setFocusId('');
    setOverlayLabel('');
    setView(next);
  };

  const handleShowOverlay = useCallback(
    (nextView: GraphView, nodes: GraphNode[], edges: GraphEdge[], label?: string) => {
      setFocusId('');
      setOverlayLabel(label || nextView);
      setView(nextView);
      setGraph({
        repo: repo || '',
        view: nextView,
        total_matching: nodes.length,
        truncated: false,
        nodes,
        edges,
      });
    },
    [repo],
  );

  const handleFocusNeighbors = useCallback((id: string) => {
    setSelectedId(id);
    setFocusId(id);
    setView('neighbors');
  }, []);

  const handleFocusImpact = useCallback((id: string) => {
    setSelectedId(id);
    setFocusId(id);
    setView('impact');
  }, []);

  const handleFocusCalls = useCallback((id: string) => {
    setSelectedId(id);
    setFocusId(id);
    setView('calls');
  }, []);

  const handleFindTests = useCallback(
    async (id: string) => {
      try {
        const res = await api.tests(id, repo || undefined, 3);
        if (res.nodes.length > 0) {
          handleShowOverlay('diff', res.nodes, res.edges, `Tests (${res.test_files?.length ?? 0})`);
        } else {
          setError(`No affected tests found for ${id}`);
        }
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : 'affected_tests failed');
      }
    },
    [repo, handleShowOverlay],
  );

  const handleNavigateNode = useCallback(
    (id: string) => {
      setSelectedId(id);
      const inCurrent = graph?.nodes.some((n) => n.id === id);
      if (!inCurrent) {
        setFocusId(id);
        setView('neighbors');
      }
    },
    [graph],
  );

  const handleRefresh = async () => {
    setRefreshing(true);
    try {
      await api.refresh(repo || undefined);
      setTimeout(async () => {
        try {
          const [ov, tr] = await Promise.all([
            api.overview(repo || undefined),
            api.tree(repo || undefined),
          ]);
          setOverview(ov);
          setTree(tr.children ?? []);
        } finally {
          setRefreshing(false);
        }
      }, 800);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'refresh failed');
      setRefreshing(false);
    }
  };

  const handleToggleNodeType = (t: string) => {
    setSelectedNodeTypes((prev) =>
      prev.includes(t) ? prev.filter((x) => x !== t) : [...prev, t],
    );
  };

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="topbar-left">
          <div className="brand">
            <span className="brand-dot" />
            <span className="brand-name">gatt</span>
          </div>

          {info && info.repos.length > 0 && (
            <select
              className="repo-select"
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              title={repo}
            >
              {info.repos.map((r) => (
                <option key={r.root} value={r.root}>
                  {shortRepoName(r.root)}
                  {r.head ? ` (${r.head})` : ''}
                </option>
              ))}
            </select>
          )}

          <nav className="view-tabs">
            <button
              type="button"
              className={view === 'architecture' ? 'active' : ''}
              onClick={() => switchMainView('architecture')}
            >
              Architecture
            </button>
            <button
              type="button"
              className={view === 'calls' ? 'active' : ''}
              onClick={() => switchMainView('calls')}
            >
              Call Graph
            </button>
            <button
              type="button"
              className={view === 'routes' ? 'active' : ''}
              onClick={() => switchMainView('routes')}
            >
              Routes
              {overview && overview.routes_count > 0 && (
                <span className="tab-count">{overview.routes_count}</span>
              )}
            </button>
            <button
              type="button"
              className={view === 'models' ? 'active' : ''}
              onClick={() => switchMainView('models')}
            >
              Models
              {overview && overview.models_count > 0 && (
                <span className="tab-count">{overview.models_count}</span>
              )}
            </button>
            {(view === 'neighbors' ||
              view === 'impact' ||
              view === 'diff' ||
              view === 'join' ||
              view === 'context') && (
              <span className={`mode-pill ${view}`}>
                {overlayLabel || (view === 'impact' ? 'Impact' : 'Neighbors')}
                <button
                  type="button"
                  onClick={() => switchMainView('architecture')}
                  title="Return to Architecture"
                >
                  ×
                </button>
              </span>
            )}
          </nav>
        </div>

        <div className="topbar-right">
          <div className="search-wrap">
            <input
              ref={searchInputRef}
              type="search"
              placeholder="Search nodes, functions, files… (/)"
              value={searchQuery}
              onFocus={() => searchHits.length > 0 && setSearchOpen(true)}
              onChange={(e) => setSearchQuery(e.target.value)}
            />
            {searchOpen && searchHits.length > 0 && (
              <div className="search-dropdown">
                <div className="search-meta">
                  {searchHits.length} results · {searchMethod}
                </div>
                {searchHits.map((hit) => (
                  <button
                    key={hit.id}
                    type="button"
                    className="search-hit"
                    onClick={() => {
                      setSearchOpen(false);
                      handleFocusNeighbors(hit.id);
                    }}
                  >
                    <span className="dot" style={{ background: nodeColor(hit.type) }} />
                    <div className="hit-info">
                      <div className="hit-name">{hit.label}</div>
                      {hit.file && <div className="hit-file">{hit.file}</div>}
                    </div>
                    <span className="hit-type">{hit.type}</span>
                  </button>
                ))}
              </div>
            )}
          </div>

          {overview && (
            <div className="stats-pill" title={overview.source}>
              <span>{overview.total_nodes.toLocaleString()} nodes</span>
              <span>·</span>
              <span>{overview.total_edges.toLocaleString()} edges</span>
            </div>
          )}

          {repoStatus && (
            <div
              className={`live-pill ${repoStatus.running ? 'running' : repoStatus.last_error ? 'err' : 'ok'}`}
              title={
                repoStatus.last_error ||
                [repoStatus.last_summary, repoStatus.last_duration, repoStatus.last_note]
                  .filter(Boolean)
                  .join(' · ') ||
                'Idle'
              }
            >
              <span className="live-dot" />
              {repoStatus.running ? (
                repoStatus.progress_total ? (
                  <span>
                    Indexing {repoStatus.progress_done ?? 0}/{repoStatus.progress_total}
                  </span>
                ) : (
                  <span>Indexing…</span>
                )
              ) : repoStatus.last_error ? (
                <span>Sync err</span>
              ) : (
                <span>Live</span>
              )}
            </div>
          )}

          <button
            type="button"
            className="refresh-btn"
            onClick={handleRefresh}
            disabled={refreshing}
            title="Re-extract changed files"
          >
            {refreshing ? 'Syncing…' : '↻ Sync'}
          </button>
        </div>
      </header>

      {error && (
        <div className="error-banner">
          <span>{error}</span>
          <button type="button" onClick={() => setError(null)}>
            ×
          </button>
        </div>
      )}

      <div className="workspace">
        <Sidebar
          repo={repo || undefined}
          overview={overview}
          tree={tree}
          selectedPkg={selectedPkg}
          limit={limit}
          depth={depth}
          layout={layout}
          includeExternal={includeExternal}
          selectedNodeTypes={selectedNodeTypes}
          bookmarks={bookmarks}
          onSelectPkg={(pkg) => {
            setSelectedPkg(pkg);
            if (
              view === 'neighbors' ||
              view === 'impact' ||
              view === 'diff' ||
              view === 'join' ||
              view === 'context'
            ) {
              setFocusId('');
              setOverlayLabel('');
              setView('architecture');
            }
          }}
          onSelectFileNode={(id) => handleFocusNeighbors(id)}
          onFocusImpact={handleFocusImpact}
          onChangeLimit={setLimit}
          onChangeDepth={setDepth}
          onChangeLayout={setLayout}
          onToggleExternal={setIncludeExternal}
          onToggleNodeType={handleToggleNodeType}
          onClearFilters={() => setSelectedNodeTypes([])}
          onSaveBookmark={(name) => {
            saveBookmarks([
              ...bookmarks,
              {
                id: String(Date.now()),
                name,
                view,
                pkg: selectedPkg,
                layout,
                groupByPackage,
              },
            ]);
          }}
          onApplyBookmark={(b) => {
            setSelectedPkg(b.pkg);
            setLayout(b.layout);
            setGroupByPackage(b.groupByPackage);
            setFocusId('');
            setOverlayLabel('');
            setView(b.view);
          }}
          onDeleteBookmark={(id) => saveBookmarks(bookmarks.filter((x) => x.id !== id))}
          onShowOverlay={handleShowOverlay}
        />

        <main className="stage" onClick={() => searchOpen && setSearchOpen(false)}>
          <GraphCanvas
            nodes={graph?.nodes ?? []}
            edges={graph?.edges ?? []}
            view={view}
            layout={layout}
            groupByPackage={groupByPackage}
            selectedPkg={selectedPkg}
            selectedId={selectedId}
            loading={graphLoading}
            truncated={graph?.truncated ?? false}
            totalMatching={graph?.total_matching ?? 0}
            onSelectNode={setSelectedId}
            onExpandNode={handleFocusNeighbors}
            onFocusImpact={handleFocusImpact}
            onFocusCalls={handleFocusCalls}
            onFindTests={handleFindTests}
            onOpenPackage={(pkg) => {
              setSelectedPkg(pkg);
              setSelectedId(null);
              setFocusId('');
              setOverlayLabel('');
              setView('architecture');
            }}
            onChangeLayout={setLayout}
            onToggleGroup={() => setGroupByPackage((v) => !v)}
          />
        </main>

        <Inspector
          repo={repo || undefined}
          detail={nodeDetail}
          loading={detailLoading}
          onClose={() => setSelectedId(null)}
          onNavigateNode={handleNavigateNode}
          onFocusNeighbors={handleFocusNeighbors}
          onFocusImpact={handleFocusImpact}
          onFocusCalls={handleFocusCalls}
          onFindTests={handleFindTests}
          onOpenPackage={(pkg) => {
            setSelectedPkg(pkg);
            setSelectedId(null);
            setFocusId('');
            setOverlayLabel('');
            setView('architecture');
          }}
          onAnnotated={() => setNodeRefreshTick((n) => n + 1)}
        />
      </div>
    </div>
  );
}
