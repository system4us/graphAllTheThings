import type {
  AnnotateRequest,
  ContextResponse,
  DiffResponse,
  DocDriftResponse,
  GraphResponse,
  GraphView,
  GrepResponse,
  InfoResponse,
  JoinResponse,
  NodeDetailResponse,
  OverviewResponse,
  RepoStatus,
  SearchResponse,
  TreeResponse,
} from './types';

async function getJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch {
      // ignore non-json error body
    }
    throw new Error(msg);
  }
  return res.json() as Promise<T>;
}

function buildParams(params: Record<string, string | number | boolean | undefined>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '' && v !== false) {
      sp.set(k, String(v));
    }
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

export const api = {
  info: () => getJSON<InfoResponse>('/api/info'),

  overview: (repo?: string) =>
    getJSON<OverviewResponse>(`/api/overview${buildParams({ repo })}`),

  graph: (opts: {
    repo?: string;
    view: GraphView;
    focus?: string;
    pkg?: string;
    depth?: number;
    limit?: number;
    types?: string[];
    edgeTypes?: string[];
    includeExternal?: boolean;
  }) =>
    getJSON<GraphResponse>(
      `/api/graph${buildParams({
        repo: opts.repo,
        view: opts.view,
        focus: opts.focus,
        package: opts.pkg,
        depth: opts.depth,
        limit: opts.limit,
        types: opts.types?.length ? opts.types.join(',') : undefined,
        edge_types: opts.edgeTypes?.length ? opts.edgeTypes.join(',') : undefined,
        include_external: opts.includeExternal ? 'true' : undefined,
      })}`,
    ),

  node: (id: string, repo?: string) =>
    getJSON<NodeDetailResponse>(`/api/node${buildParams({ repo, id })}`),

  search: (q: string, repo?: string, type?: string, limit = 25) =>
    getJSON<SearchResponse>(`/api/search${buildParams({ repo, q, type, limit })}`),

  tree: (repo?: string) => getJSON<TreeResponse>(`/api/tree${buildParams({ repo })}`),

  refresh: (repo?: string) =>
    getJSON<{ state: string }>(`/api/refresh${buildParams({ repo })}`, {
      method: 'POST',
    }),

  status: (repo?: string) => getJSON<RepoStatus>(`/api/status${buildParams({ repo })}`),

  grep: (opts: {
    repo?: string;
    q: string;
    regex?: boolean;
    include?: string;
    exclude?: string;
    limit?: number;
  }) =>
    getJSON<GrepResponse>(
      `/api/grep${buildParams({
        repo: opts.repo,
        q: opts.q,
        regex: opts.regex ? 'true' : undefined,
        include: opts.include,
        exclude: opts.exclude,
        limit: opts.limit,
      })}`,
    ),

  diff: (ref: string, repo?: string, limit = 60) =>
    getJSON<DiffResponse>(`/api/diff${buildParams({ repo, ref, limit })}`),

  docDrift: (repo?: string, limit = 50) =>
    getJSON<DocDriftResponse>(`/api/doc-drift${buildParams({ repo, limit })}`),

  annotate: (body: AnnotateRequest, repo?: string) =>
    getJSON<{ id: string; annotations: Record<string, string> }>(
      `/api/annotate${buildParams({ repo })}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    ),

  join: (from: string, to: string, repo?: string) =>
    getJSON<JoinResponse>(`/api/join${buildParams({ repo, from, to })}`),

  context: (q: string, repo?: string, limit = 8) =>
    getJSON<ContextResponse>(`/api/context${buildParams({ repo, q, limit })}`),
};
