export interface RepoInfo {
  root: string;
  key: string;
  head?: string;
  running: boolean;
  queued: boolean;
}

export interface InfoResponse {
  mode: 'single' | 'multi';
  repos: RepoInfo[];
  default_repo: string;
}

export interface PackageStat {
  path: string;
  files: number;
  functions: number;
}

export interface OverviewResponse {
  repo: string;
  source: string;
  extracted_at?: string;
  is_codebase: boolean;
  is_api: boolean;
  total_nodes: number;
  total_edges: number;
  node_counts: Record<string, number>;
  edge_counts: Record<string, number>;
  top_packages: PackageStat[];
  routes_count: number;
  models_count: number;
}

export type GraphView =
  | 'architecture'
  | 'calls'
  | 'routes'
  | 'models'
  | 'neighbors'
  | 'impact'
  | 'diff'
  | 'join'
  | 'context';

export type LayoutName =
  | 'dagre-lr'
  | 'dagre-tb'
  | 'fcose'
  | 'concentric'
  | 'grid';

export interface RepoStatus {
  running: boolean;
  queued: boolean;
  progress_done?: number;
  progress_total?: number;
  last_at?: string;
  last_duration?: string;
  last_summary?: string;
  last_embedded?: number;
  last_note?: string;
  last_error?: string;
}

export interface GraphNode {
  id: string;
  type: string;
  name: string;
  label: string;
  file?: string;
  package?: string;
  line_start?: number;
  line_end?: number;
  degree: number;
  distance?: number;
  diff_status?: 'added' | 'modified' | 'removed' | 'caller';
  external?: boolean;
  attrs?: Record<string, string>;
}

export interface GrepHit {
  path: string;
  line: number;
  text: string;
  file_node_id?: string;
  node_id?: string;
  node_label?: string;
}

export interface GrepResponse {
  repo: string;
  total: number;
  shown: number;
  files_scanned: number;
  truncated: boolean;
  hits: GrepHit[];
}

export interface DiffChange {
  kind: string;
  type: string;
  old_id?: string;
  new_id?: string;
  old_name?: string;
  new_name?: string;
  old_file?: string;
  new_file?: string;
  callers?: { id: string; name: string; file?: string; label: string }[];
}

export interface DiffResponse {
  repo: string;
  ref: string;
  files_touched: number;
  total_changes: number;
  changes: DiffChange[];
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface DocDriftResponse {
  repo: string;
  text: string;
  docs: { id: string; path: string }[] | null;
}

export interface JoinStep {
  from_table: string;
  to_table: string;
  from_column?: string;
  to_column?: string;
}

export interface JoinResponse {
  repo: string;
  found: boolean;
  steps?: JoinStep[];
  hint?: string;
  text: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface ContextResponse {
  repo: string;
  method: string;
  text: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface AnnotateRequest {
  id: string;
  entity_note?: string;
  default_filter?: string;
  route_method?: string;
  route_path?: string;
  route_framework?: string;
  model_table?: string;
  clear?: boolean;
}

export interface GraphEdge {
  id: string;
  from: string;
  to: string;
  type: string;
  attrs?: Record<string, string>;
}

export interface GraphResponse {
  repo: string;
  view: GraphView;
  focus?: string;
  package?: string;
  total_matching: number;
  truncated: boolean;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface NeighborSummary {
  id: string;
  type: string;
  name: string;
  label: string;
  file?: string;
  edge_type: string;
  attrs?: Record<string, string>;
}

export interface SourcePreview {
  file: string;
  line_start: number;
  line_end: number;
  language?: string;
  content: string;
  truncated?: boolean;
}

export interface CommitEntry {
  sha: string;
  date: string;
  author: string;
  subject: string;
  issues?: string[];
}

export interface NodeDetailResponse {
  repo: string;
  id: string;
  type: string;
  name: string;
  label: string;
  file?: string;
  package?: string;
  attrs?: Record<string, string>;
  incoming: NeighborSummary[] | null;
  outgoing: NeighborSummary[] | null;
  source?: SourcePreview | null;
  history?: CommitEntry[] | null;
}

export interface TestRef {
  id: string;
  name: string;
  file: string;
  line?: string;
}

export interface AffectedTestsResponse {
  repo: string;
  target: string;
  test_files: string[];
  test_functions: TestRef[];
  commands: string[];
  text: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface HotspotEntry {
  node_id: string;
  path: string;
  commits: number;
  degree: number;
  score: number;
}

export interface HotspotsResponse {
  repo: string;
  hotspots: HotspotEntry[];
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface NodeDiffResponse {
  repo: string;
  file: string;
  ref: string;
  diff: string;
}

export interface SavedBookmark {
  id: string;
  name: string;
  view: GraphView;
  pkg: string;
  layout: LayoutName;
  groupByPackage: boolean;
}

export interface SearchHit {
  id: string;
  type: string;
  name: string;
  label: string;
  file?: string;
  score: number;
}

export interface SearchResponse {
  repo: string;
  method: string;
  hits: SearchHit[];
}

export interface TreeNode {
  name: string;
  path: string;
  kind: 'dir' | 'file';
  node_id?: string;
  functions?: number;
  children?: TreeNode[];
}

export interface TreeResponse {
  repo: string;
  children: TreeNode[] | null;
}

export const NODE_COLORS: Record<string, string> = {
  file: '#3b82f6',
  package: '#6366f1',
  project: '#818cf8',
  component: '#8b5cf6',
  function: '#10b981',
  definition: '#f59e0b',
  route: '#a855f7',
  endpoint: '#a855f7',
  model: '#06b6d4',
  table: '#06b6d4',
  schema: '#06b6d4',
  view: '#14b8a6',
  state: '#f43f5e',
  column: '#64748b',
  property: '#64748b',
};

export const EDGE_COLORS: Record<string, string> = {
  CALLS: '#34d399',
  IMPORTS: '#60a5fa',
  DEPENDS_ON: '#818cf8',
  HAS_METHOD: '#fbbf24',
  HANDLED_BY: '#c084fc',
  USES_MIDDLEWARE: '#e879f9',
  USES_MODEL: '#22d3ee',
  CALLS_ENDPOINT: '#f472b6',
  CO_CHANGED: '#fb923c',
  REFERENCES: '#2dd4bf',
  FOREIGN_KEY: '#38bdf8',
  READS: '#fda4af',
  WRITES: '#fb7185',
};

export function nodeColor(type: string): string {
  return NODE_COLORS[type] ?? '#94a3b8';
}

export function edgeColor(type: string): string {
  return EDGE_COLORS[type] ?? '#64748b';
}
