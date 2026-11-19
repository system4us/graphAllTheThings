import React, { useState } from 'react';
import { ContextMenu, type ContextMenuItem } from './ContextMenu';
import { ToolsPanel, type ToolTab } from './ToolsPanel';
import {
  nodeColor,
  type GraphEdge,
  type GraphNode,
  type GraphView,
  type LayoutName,
  type OverviewResponse,
  type SavedBookmark,
  type TreeNode,
} from './types';

type SidebarTab = 'explorer' | 'packages' | 'filters' | ToolTab;

interface SidebarProps {
  repo?: string;
  overview: OverviewResponse | null;
  tree: TreeNode[];
  selectedPkg: string;
  limit: number;
  depth: number;
  layout: LayoutName;
  includeExternal: boolean;
  selectedNodeTypes: string[];
  bookmarks: SavedBookmark[];
  onSelectPkg: (pkg: string) => void;
  onSelectFileNode: (nodeId: string) => void;
  onFocusImpact: (nodeId: string) => void;
  onChangeLimit: (n: number) => void;
  onChangeDepth: (n: number) => void;
  onChangeLayout: (l: LayoutName) => void;
  onToggleExternal: (v: boolean) => void;
  onToggleNodeType: (t: string) => void;
  onClearFilters: () => void;
  onSaveBookmark: (name: string) => void;
  onApplyBookmark: (b: SavedBookmark) => void;
  onDeleteBookmark: (id: string) => void;
  onShowOverlay: (view: GraphView, nodes: GraphNode[], edges: GraphEdge[], label?: string) => void;
}

const TreeItem: React.FC<{
  node: TreeNode;
  level: number;
  selectedPkg: string;
  onSelectPkg: (pkg: string) => void;
  onSelectFileNode: (id: string) => void;
  onContextMenu: (e: React.MouseEvent, node: TreeNode) => void;
}> = ({ node, level, selectedPkg, onSelectPkg, onSelectFileNode, onContextMenu }) => {
  const [open, setOpen] = useState(level < 1);
  if (node.kind === 'dir') {
    const active = selectedPkg === node.path;
    return (
      <div>
        <div
          className={`tree-row dir ${active ? 'active' : ''}`}
          style={{ paddingLeft: 10 + level * 14 }}
          onContextMenu={(e) => onContextMenu(e, node)}
        >
          <button
            type="button"
            className="tree-chevron"
            onClick={() => setOpen((v) => !v)}
            aria-label="Toggle folder"
          >
            {open ? '▾' : '▸'}
          </button>
          <button
            type="button"
            className="tree-label"
            onClick={() => onSelectPkg(active ? '' : node.path)}
            title={`Filter graph to ${node.path}`}
          >
            {node.name}/
          </button>
        </div>
        {open &&
          node.children?.map((child) => (
            <TreeItem
              key={child.path}
              node={child}
              level={level + 1}
              selectedPkg={selectedPkg}
              onSelectPkg={onSelectPkg}
              onSelectFileNode={onSelectFileNode}
              onContextMenu={onContextMenu}
            />
          ))}
      </div>
    );
  }
  return (
    <button
      type="button"
      className="tree-row file"
      style={{ paddingLeft: 26 + level * 14 }}
      onClick={() => node.node_id && onSelectFileNode(node.node_id)}
      onContextMenu={(e) => onContextMenu(e, node)}
      title={node.path}
    >
      <span className="tree-file-name">{node.name}</span>
      {node.functions ? <span className="tree-badge">{node.functions}</span> : null}
    </button>
  );
};

export const Sidebar: React.FC<SidebarProps> = ({
  repo,
  overview,
  tree,
  selectedPkg,
  limit,
  depth,
  layout,
  includeExternal,
  selectedNodeTypes,
  bookmarks,
  onSelectPkg,
  onSelectFileNode,
  onFocusImpact,
  onChangeLimit,
  onChangeDepth,
  onChangeLayout,
  onToggleExternal,
  onToggleNodeType,
  onClearFilters,
  onSaveBookmark,
  onApplyBookmark,
  onDeleteBookmark,
  onShowOverlay,
}) => {
  const [tab, setTab] = useState<SidebarTab>('explorer');
  const [bmName, setBmName] = useState('');
  const [ctxMenu, setCtxMenu] = useState<{
    x: number;
    y: number;
    title: string;
    items: ContextMenuItem[];
  } | null>(null);

  const openTreeMenu = (e: React.MouseEvent, node: TreeNode) => {
    e.preventDefault();
    const copy = (txt: string) => navigator.clipboard?.writeText(txt).catch(() => {});
    if (node.kind === 'dir') {
      const active = selectedPkg === node.path;
      setCtxMenu({
        x: e.clientX,
        y: e.clientY,
        title: `📁 ${node.path}`,
        items: [
          {
            label: active ? 'Clear Folder Filter' : 'Filter Graph to Folder',
            onClick: () => onSelectPkg(active ? '' : node.path),
          },
          { label: 'Copy Folder Path', onClick: () => copy(node.path) },
        ],
      });
    } else {
      const items: ContextMenuItem[] = [];
      if (node.node_id) {
        items.push(
          { label: 'Focus in Graph', onClick: () => onSelectFileNode(node.node_id!) },
          { label: 'Blast / Impact Radius', danger: true, onClick: () => onFocusImpact(node.node_id!) },
          { separator: true, label: '' },
        );
      }
      items.push({ label: 'Copy File Path', onClick: () => copy(node.path) });
      setCtxMenu({ x: e.clientX, y: e.clientY, title: node.name, items });
    }
  };

  const nodeTypeEntries = overview
    ? Object.entries(overview.node_counts).sort((a, b) => b[1] - a[1])
    : [];

  const isToolTab = (t: SidebarTab): t is ToolTab =>
    t === 'grep' ||
    t === 'diff' ||
    t === 'ask' ||
    t === 'tests' ||
    t === 'hotspots' ||
    t === 'drift' ||
    t === 'join';

  return (
    <aside className="sidebar">
      <div className="sidebar-tabs-grid">
        {(
          [
            ['explorer', 'Explorer'],
            ['packages', 'Packages'],
            ['filters', 'Filters'],
            ['ask', 'Ask'],
            ['grep', 'Grep'],
            ['diff', 'Diff'],
            ['tests', 'Tests'],
            ['hotspots', 'Hotspots'],
            ['drift', 'Drift'],
            ['join', 'Join'],
          ] as [SidebarTab, string][]
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            className={tab === id ? 'active' : ''}
            onClick={() => setTab(id)}
          >
            {label}
          </button>
        ))}
      </div>

      {selectedPkg && (
        <div className="active-filter-chip">
          <span title={selectedPkg}>📁 {selectedPkg}</span>
          <button type="button" onClick={() => onSelectPkg('')} title="Clear package filter">
            ×
          </button>
        </div>
      )}

      <div className="sidebar-body">
        {isToolTab(tab) && (
          <ToolsPanel
            tab={tab}
            repo={repo}
            onJumpNode={onSelectFileNode}
            onShowOverlay={onShowOverlay}
          />
        )}

        {tab === 'explorer' && (
          <div className="tree-list">
            {tree.length === 0 ? (
              <div className="muted-note">No file tree available for this graph.</div>
            ) : (
              tree.map((node) => (
                <TreeItem
                  key={node.path}
                  node={node}
                  level={0}
                  selectedPkg={selectedPkg}
                  onSelectPkg={onSelectPkg}
                  onSelectFileNode={onSelectFileNode}
                  onContextMenu={openTreeMenu}
                />
              ))
            )}
          </div>
        )}

        {tab === 'packages' && (
          <div className="pkg-list">
            {!overview?.top_packages?.length ? (
              <div className="muted-note">No packages indexed.</div>
            ) : (
              overview.top_packages.map((p) => {
                const active = selectedPkg === p.path;
                return (
                  <button
                    key={p.path}
                    type="button"
                    className={`pkg-item ${active ? 'active' : ''}`}
                    onClick={() => onSelectPkg(active ? '' : p.path)}
                  >
                    <span className="pkg-path" title={p.path}>
                      {p.path}
                    </span>
                    <span className="pkg-counts">
                      {p.files}f · {p.functions}fn
                    </span>
                  </button>
                );
              })
            )}
          </div>
        )}

        {tab === 'filters' && (
          <div className="filters-pane">
            <div className="filter-group">
              <label>Saved Views / Bookmarks</label>
              <form
                className="tool-row"
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!bmName.trim()) return;
                  onSaveBookmark(bmName.trim());
                  setBmName('');
                }}
              >
                <input
                  type="text"
                  placeholder="Save current view as…"
                  value={bmName}
                  onChange={(e) => setBmName(e.target.value)}
                  className="bm-input"
                />
                <button type="submit" className="secondary-btn">
                  Save
                </button>
              </form>
              {bookmarks.length > 0 && (
                <div className="bm-list">
                  {bookmarks.map((b) => (
                    <div key={b.id} className="bm-item">
                      <button
                        type="button"
                        className="bm-load"
                        onClick={() => onApplyBookmark(b)}
                        title={`${b.view}${b.pkg ? ' · ' + b.pkg : ''}`}
                      >
                        ★ {b.name}
                      </button>
                      <button
                        type="button"
                        className="icon-btn"
                        onClick={() => onDeleteBookmark(b.id)}
                        title="Delete bookmark"
                      >
                        ×
                      </button>
                    </div>
                  ))}
                </div>
              )}
            </div>

            <div className="filter-group">
              <label>Layout</label>
              <select
                value={layout}
                onChange={(e) => onChangeLayout(e.target.value as LayoutName)}
              >
                <option value="dagre-lr">Hierarchical Left → Right (Dagre)</option>
                <option value="dagre-tb">Hierarchical Top → Bottom (Dagre)</option>
                <option value="fcose">Clustered Force (fCoSE)</option>
                <option value="concentric">Concentric</option>
                <option value="grid">Grid</option>
              </select>
            </div>

            <div className="filter-group">
              <label>Max Nodes: {limit}</label>
              <input
                type="range"
                min={30}
                max={400}
                step={10}
                value={limit}
                onChange={(e) => onChangeLimit(Number(e.target.value))}
              />
            </div>

            <div className="filter-group">
              <label>Traversal Depth (Impact / Neighbors): {depth}</label>
              <input
                type="range"
                min={1}
                max={4}
                step={1}
                value={depth}
                onChange={(e) => onChangeDepth(Number(e.target.value))}
              />
            </div>

            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={includeExternal}
                onChange={(e) => onToggleExternal(e.target.checked)}
              />
              <span>Include external / stdlib stubs</span>
            </label>

            {nodeTypeEntries.length > 0 && (
              <div className="filter-group">
                <div className="filter-header">
                  <label>Node Types</label>
                  {selectedNodeTypes.length > 0 && (
                    <button type="button" className="link-btn" onClick={onClearFilters}>
                      Reset
                    </button>
                  )}
                </div>
                <div className="type-checks">
                  {nodeTypeEntries.map(([t, count]) => (
                    <label key={t} className="checkbox-row">
                      <input
                        type="checkbox"
                        checked={selectedNodeTypes.includes(t)}
                        onChange={() => onToggleNodeType(t)}
                      />
                      <span className="dot" style={{ background: nodeColor(t) }} />
                      <span className="type-name">{t}</span>
                      <span className="type-count">{count}</span>
                    </label>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}
      </div>
      {ctxMenu && (
        <ContextMenu
          x={ctxMenu.x}
          y={ctxMenu.y}
          title={ctxMenu.title}
          items={ctxMenu.items}
          onClose={() => setCtxMenu(null)}
        />
      )}
    </aside>
  );
};
