import React, { useEffect, useRef, useState } from 'react';
import cytoscape, { type Core, type ElementDefinition } from 'cytoscape';
import dagre from 'cytoscape-dagre';
// @ts-expect-error cytoscape-fcose has no bundled types
import fcose from 'cytoscape-fcose';
import { ContextMenu, type ContextMenuItem } from './ContextMenu';
import {
  edgeColor,
  nodeColor,
  type GraphEdge,
  type GraphNode,
  type GraphView,
  type LayoutName,
} from './types';

cytoscape.use(dagre);
cytoscape.use(fcose);

interface GraphCanvasProps {
  nodes: GraphNode[];
  edges: GraphEdge[];
  view: GraphView;
  layout: LayoutName;
  groupByPackage: boolean;
  selectedPkg: string;
  selectedId: string | null;
  loading: boolean;
  truncated: boolean;
  totalMatching: number;
  onSelectNode: (id: string | null) => void;
  onExpandNode: (id: string) => void;
  onFocusImpact: (id: string) => void;
  onFocusCalls: (id: string) => void;
  onOpenPackage: (pkg: string) => void;
  onChangeLayout: (l: LayoutName) => void;
  onToggleGroup: () => void;
}

function nodeShape(type: string): string {
  switch (type) {
    case 'route':
    case 'endpoint':
      return 'round-hexagon';
    case 'model':
    case 'table':
    case 'schema':
    case 'view':
      return 'barrel';
    case 'definition':
      return 'cut-rectangle';
    default:
      return 'round-rectangle';
  }
}

function impactColor(distance?: number): string | null {
  if (distance === undefined) return null;
  if (distance === 0) return '#ef4444';
  if (distance === 1) return '#f97316';
  if (distance === 2) return '#eab308';
  return '#fde047';
}

function diffColor(status?: string): string | null {
  switch (status) {
    case 'added':
      return '#22c55e';
    case 'modified':
      return '#f59e0b';
    case 'removed':
      return '#ef4444';
    case 'caller':
      return '#38bdf8';
    default:
      return null;
  }
}

function layoutOptions(name: LayoutName, nodeCount: number): cytoscape.LayoutOptions {
  switch (name) {
    case 'dagre-lr':
      return {
        name: 'dagre',
        rankDir: 'LR',
        align: 'UL',
        nodeSep: 26,
        edgeSep: 14,
        rankSep: 85,
        padding: 36,
        nodeDimensionsIncludeLabels: true,
        animate: false,
      } as cytoscape.LayoutOptions;
    case 'dagre-tb':
      return {
        name: 'dagre',
        rankDir: 'TB',
        nodeSep: 30,
        edgeSep: 14,
        rankSep: 78,
        padding: 36,
        nodeDimensionsIncludeLabels: true,
        animate: false,
      } as cytoscape.LayoutOptions;
    case 'concentric':
      return {
        name: 'concentric',
        padding: 36,
        minNodeSpacing: 42,
        avoidOverlap: true,
        nodeDimensionsIncludeLabels: true,
        animate: false,
      } as cytoscape.LayoutOptions;
    case 'grid':
      return {
        name: 'grid',
        padding: 36,
        avoidOverlap: true,
        avoidOverlapPadding: 18,
        nodeDimensionsIncludeLabels: true,
        animate: false,
      } as cytoscape.LayoutOptions;
    default: // 'fcose'
      return {
        name: 'fcose',
        quality: nodeCount > 150 ? 'default' : 'proof',
        randomize: true,
        animate: false,
        padding: 36,
        nodeDimensionsIncludeLabels: true,
        nodeSeparation: 65,
        nodeRepulsion: () => 12000,
        idealEdgeLength: () => 115,
        edgeElasticity: () => 0.4,
        packComponents: true,
        tile: true,
        tilingPaddingVertical: 22,
        tilingPaddingHorizontal: 22,
      } as cytoscape.LayoutOptions;
  }
}

export const GraphCanvas: React.FC<GraphCanvasProps> = ({
  nodes,
  edges,
  view,
  layout,
  groupByPackage,
  selectedPkg,
  selectedId,
  loading,
  truncated,
  totalMatching,
  onSelectNode,
  onExpandNode,
  onFocusImpact,
  onFocusCalls,
  onOpenPackage,
  onChangeLayout,
  onToggleGroup,
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const cyRef = useRef<Core | null>(null);
  const selectedIdRef = useRef<string | null>(selectedId);
  const nodesRef = useRef<GraphNode[]>(nodes);
  const layoutRef = useRef<LayoutName>(layout);
  const selectedPkgRef = useRef<string>(selectedPkg);
  const didRightDragRef = useRef<boolean>(false);

  const [showLegend, setShowLegend] = useState(false);
  const [zoomPct, setZoomPct] = useState(100);
  const [isPanning, setIsPanning] = useState(false);
  const [ctxMenu, setCtxMenu] = useState<{
    x: number;
    y: number;
    title?: string;
    items: ContextMenuItem[];
  } | null>(null);

  const onSelectRef = useRef(onSelectNode);
  const onExpandRef = useRef(onExpandNode);
  const onImpactRef = useRef(onFocusImpact);
  const onCallsRef = useRef(onFocusCalls);
  const onOpenPkgRef = useRef(onOpenPackage);
  const onLayoutRef = useRef(onChangeLayout);
  const onToggleGroupRef = useRef(onToggleGroup);
  onSelectRef.current = onSelectNode;
  onExpandRef.current = onExpandNode;
  onImpactRef.current = onFocusImpact;
  onCallsRef.current = onFocusCalls;
  onOpenPkgRef.current = onOpenPackage;
  onLayoutRef.current = onChangeLayout;
  onToggleGroupRef.current = onToggleGroup;
  selectedIdRef.current = selectedId;
  nodesRef.current = nodes;
  layoutRef.current = layout;
  selectedPkgRef.current = selectedPkg;

  // Initialize Cytoscape once.
  useEffect(() => {
    if (!containerRef.current) return;
    const cy = cytoscape({
      container: containerRef.current,
      minZoom: 0.12,
      maxZoom: 3.5,
      userPanningEnabled: false,
      userZoomingEnabled: false,
      boxSelectionEnabled: false,
      textureOnViewport: false,
      hideEdgesOnViewport: false,
      hideLabelsOnViewport: false,
      pixelRatio: Math.max(window.devicePixelRatio || 1, 2),
      style: [
        // Compound package/folder group boxes
        {
          selector: 'node[?isGroup]',
          style: {
            label: 'data(label)',
            'text-valign': 'top',
            'text-halign': 'left',
            'text-margin-x': 8,
            'text-margin-y': 16,
            'font-size': 11,
            'font-weight': 'bold',
            'font-family': 'ui-monospace, SFMono-Regular, Menlo, monospace',
            color: '#94a3b8',
            'background-color': '#0f172a',
            'background-opacity': 0.55,
            'border-width': 1.2,
            'border-style': 'dashed',
            'border-color': '#334155',
            shape: 'round-rectangle',
            padding: '22px',
          } as never,
        },
        // Regular code entity pill/card nodes (auto-sized to label so text never overlaps)
        {
          selector: 'node[!isGroup]',
          style: {
            label: 'data(label)',
            shape: 'data(shape)' as never,
            width: 'label',
            height: 36,
            padding: '12px',
            'background-color': '#111b2e',
            'border-width': 2,
            'border-color': 'data(color)',
            color: '#f1f5f9',
            'font-size': 13,
            'font-family': 'ui-monospace, SFMono-Regular, Menlo, monospace',
            'text-valign': 'center',
            'text-halign': 'center',
            'min-zoomed-font-size': 0,
            'overlay-opacity': 0,
          } as never,
        },
        {
          selector: 'node[!isGroup][?tinted]',
          style: {
            'background-color': 'data(color)',
            color: '#0b1120',
            'font-weight': 'bold',
          },
        },
        {
          selector: 'node.selected',
          style: {
            'border-width': 3,
            'border-color': '#ffffff',
            'background-color': '#1e293b',
            color: '#ffffff',
            'font-weight': 'bold',
            'z-index': 30,
          },
        },
        {
          selector: 'node.hovered',
          style: {
            'border-width': 2.5,
            'border-color': '#e2e8f0',
            'z-index': 25,
          },
        },
        {
          selector: 'node.dimmed',
          style: {
            opacity: 0.16,
          },
        },
        {
          selector: 'edge',
          style: {
            width: 1.3,
            'line-color': 'data(color)',
            'target-arrow-color': 'data(color)',
            'target-arrow-shape': 'triangle',
            'arrow-scale': 0.75,
            'curve-style': 'bezier',
            opacity: 0.28,
          },
        },
        {
          selector: 'edge[edgeType = "CO_CHANGED"]',
          style: {
            'line-style': 'dashed',
            'target-arrow-shape': 'none',
            opacity: 0.22,
          },
        },
        {
          selector: 'edge.highlighted',
          style: {
            width: 2.4,
            opacity: 0.98,
            label: 'data(edgeType)',
            'font-size': 9,
            color: '#e2e8f0',
            'text-background-color': '#0b1120',
            'text-background-opacity': 0.9,
            'text-background-padding': '2px',
            'text-background-shape': 'roundrectangle',
            'z-index': 20,
          },
        },
        {
          selector: 'edge.dimmed',
          style: {
            opacity: 0.05,
          },
        },
      ],
    });

    cy.on('tap', 'node[!isGroup]', (evt) => {
      onSelectRef.current(evt.target.id());
    });
    cy.on('dbltap', 'node[!isGroup]', (evt) => {
      const id = evt.target.id();
      if (id.startsWith('dir:')) {
        onOpenPkgRef.current(id.slice(4));
      } else {
        onExpandRef.current(id);
      }
    });
    cy.on('zoom', () => {
      setZoomPct(Math.round(cy.zoom() * 100));
    });
    cy.on('tap', (evt) => {
      setCtxMenu(null);
      if (evt.target === cy) {
        onSelectRef.current(null);
      }
    });

    // Right-click (without drag) opens custom context menu for node, folder, or canvas.
    cy.on('cxttap', (evt) => {
      if (didRightDragRef.current) return;
      const oe = evt.originalEvent as MouseEvent | undefined;
      const x = oe?.clientX ?? 120;
      const y = oe?.clientY ?? 120;
      const copy = (txt: string) => navigator.clipboard?.writeText(txt).catch(() => {});

      if (evt.target !== cy && evt.target.isNode && evt.target.isNode()) {
        const el = evt.target;
        if (el.data('isGroup')) {
          const pkg = String(el.data('label') || '');
          setCtxMenu({
            x,
            y,
            title: `📁 ${pkg}`,
            items: [
              { label: 'Filter Graph to Folder', onClick: () => onOpenPkgRef.current(pkg) },
              { label: 'Copy Folder Path', onClick: () => copy(pkg) },
            ],
          });
          return;
        }
        const id = el.id();
        const nodeObj = nodesRef.current.find((n) => n.id === id);
        if (id.startsWith('dir:')) {
          const pkg = id.slice(4);
          setCtxMenu({
            x,
            y,
            title: `📁 ${pkg}`,
            items: [
              { label: 'Open Folder in Graph', onClick: () => onOpenPkgRef.current(pkg) },
              { label: 'Inspect Contents', onClick: () => onSelectRef.current(id) },
              { separator: true, label: '' },
              { label: 'Copy Folder Path', onClick: () => copy(pkg) },
            ],
          });
          return;
        }

        const items: ContextMenuItem[] = [
          { label: 'Inspect Code & Details', onClick: () => onSelectRef.current(id) },
          { label: 'Expand Neighbors', shortcut: '2×Click', onClick: () => onExpandRef.current(id) },
          { label: 'Blast / Impact Radius', danger: true, onClick: () => onImpactRef.current(id) },
        ];
        if (nodeObj && (nodeObj.type === 'function' || nodeObj.type === 'definition')) {
          items.push({ label: 'Call Graph', onClick: () => onCallsRef.current(id) });
        }
        if (nodeObj?.package) {
          items.push({
            label: `Open Folder (${nodeObj.package})`,
            onClick: () => onOpenPkgRef.current(nodeObj.package!),
          });
        }
        items.push({ separator: true, label: '' });
        if (nodeObj?.file) {
          items.push({ label: 'Copy File Path', onClick: () => copy(nodeObj.file!) });
        }
        items.push({ label: 'Copy Node ID', onClick: () => copy(id) });
        items.push({ separator: true, label: '' });
        items.push({
          label: 'Hide Node from Canvas',
          onClick: () => {
            el.remove();
            if (selectedIdRef.current === id) onSelectRef.current(null);
          },
        });
        setCtxMenu({ x, y, title: nodeObj?.label || id, items });
        return;
      }

      // Empty canvas context menu
      const canvasItems: ContextMenuItem[] = [
        {
          label: 'Reset Zoom (100%)',
          onClick: () => {
            cy.zoom(1);
            cy.center();
          },
        },
        { label: 'Fit Whole Graph', onClick: () => cy.fit(undefined, 40) },
        {
          label: 'Re-run Layout',
          onClick: () => {
            if (nodesRef.current.length > 0) {
              cy.layout(layoutOptions(layoutRef.current, nodesRef.current.length)).run();
              cy.fit(undefined, 40);
            }
          },
        },
        { separator: true, label: '' },
        { label: 'Layout: Left → Right', onClick: () => onLayoutRef.current('dagre-lr') },
        { label: 'Layout: Top → Bottom', onClick: () => onLayoutRef.current('dagre-tb') },
        { label: 'Layout: Clustered (fCoSE)', onClick: () => onLayoutRef.current('fcose') },
        { label: 'Toggle Folder Grouping', onClick: () => onToggleGroupRef.current() },
        { separator: true, label: '' },
        {
          label: 'Export PNG',
          onClick: () => {
            const png = cy.png({ bg: '#0b1120', full: true, scale: 2 });
            const a = document.createElement('a');
            a.href = png;
            a.download = 'gatt-graph.png';
            a.click();
          },
        },
      ];
      if (selectedPkgRef.current) {
        canvasItems.unshift(
          { label: '← Return to All Packages', onClick: () => onOpenPkgRef.current('') },
          { separator: true, label: '' },
        );
      }
      setCtxMenu({ x, y, title: 'Canvas', items: canvasItems });
    });

    // Right-button (and middle-button) drag-to-pan on the canvas container.
    const el = containerRef.current;
    let dragging = false;
    let startX = 0;
    let startY = 0;
    let lastX = 0;
    let lastY = 0;

    const onMouseDown = (e: MouseEvent) => {
      if (e.button === 2 || e.button === 1) {
        dragging = true;
        didRightDragRef.current = false;
        startX = lastX = e.clientX;
        startY = lastY = e.clientY;
        if (e.button === 1) e.preventDefault();
      }
    };
    const onMouseMove = (e: MouseEvent) => {
      if (!dragging) return;
      const dx = e.clientX - lastX;
      const dy = e.clientY - lastY;
      lastX = e.clientX;
      lastY = e.clientY;
      if (!didRightDragRef.current && Math.hypot(e.clientX - startX, e.clientY - startY) > 4) {
        didRightDragRef.current = true;
        setCtxMenu(null);
        setIsPanning(true);
      }
      if (didRightDragRef.current) {
        cy.panBy({ x: dx, y: dy });
      }
    };
    const onMouseUp = () => {
      if (!dragging) return;
      dragging = false;
      setIsPanning(false);
    };
    const onContextMenu = (e: MouseEvent) => {
      e.preventDefault();
    };
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      // Normalize trackpad pinch / smooth scroll vs discrete mouse wheel ticks.
      const delta = e.deltaMode === 1 ? e.deltaY * 16 : e.deltaY;
      const clamped = Math.max(-120, Math.min(120, delta));
      const factor = Math.pow(1.0018, -clamped);
      const nextZoom = Math.min(3.5, Math.max(0.12, cy.zoom() * factor));
      cy.zoom({
        level: nextZoom,
        renderedPosition: {
          x: e.clientX - rect.left,
          y: e.clientY - rect.top,
        },
      });
    };

    el.addEventListener('mousedown', onMouseDown);
    el.addEventListener('contextmenu', onContextMenu);
    el.addEventListener('wheel', onWheel, { passive: false });
    window.addEventListener('mousemove', onMouseMove);
    window.addEventListener('mouseup', onMouseUp);

    // Hover spotlight when nothing is locked-selected
    cy.on('mouseover', 'node[!isGroup]', (evt) => {
      if (selectedIdRef.current) return;
      const target = evt.target;
      const hood = target.closedNeighborhood();
      cy.batch(() => {
        cy.elements().not(hood).not('node[?isGroup]').addClass('dimmed');
        target.addClass('hovered');
        hood.edges().addClass('highlighted');
      });
    });
    cy.on('mouseout', 'node[!isGroup]', () => {
      if (selectedIdRef.current) return;
      cy.batch(() => {
        cy.elements().removeClass('dimmed hovered highlighted');
      });
    });

    cyRef.current = cy;
    return () => {
      el.removeEventListener('mousedown', onMouseDown);
      el.removeEventListener('contextmenu', onContextMenu);
      el.removeEventListener('wheel', onWheel);
      window.removeEventListener('mousemove', onMouseMove);
      window.removeEventListener('mouseup', onMouseUp);
      cy.destroy();
      cyRef.current = null;
    };
  }, []);

  // Update graph elements & run layout when data, layout, or grouping changes.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;

    const elements: ElementDefinition[] = [];
    const useGroups =
      groupByPackage && view !== 'join' && view !== 'impact' && nodes.length > 1;

    if (useGroups) {
      const pkgs = new Set<string>();
      for (const n of nodes) {
        if (n.package) pkgs.add(n.package);
      }
      // Only create compound boxes when there are >= 2 distinct packages
      if (pkgs.size >= 2) {
        for (const p of pkgs) {
          elements.push({
            data: {
              id: `pkg:${p}`,
              label: p,
              isGroup: true,
            },
          });
        }
      }
    }

    const groupSet = new Set(
      elements.filter((el) => el.data.isGroup).map((el) => el.data.id as string),
    );

    for (const n of nodes) {
      const imp = view === 'impact' ? impactColor(n.distance) : null;
      const diff = view === 'diff' ? diffColor(n.diff_status) : null;
      const pkgGroupId = n.package ? `pkg:${n.package}` : undefined;
      const parent = pkgGroupId && groupSet.has(pkgGroupId) ? pkgGroupId : undefined;
      elements.push({
        data: {
          id: n.id,
          parent,
          label: n.label.length > 30 ? n.label.slice(0, 28) + '…' : n.label,
          fullLabel: n.label,
          nodeType: n.type,
          color: diff ?? imp ?? nodeColor(n.type),
          shape: nodeShape(n.type),
          tinted: Boolean(diff || imp),
        },
      });
    }

    for (const e of edges) {
      elements.push({
        data: {
          id: e.id,
          source: e.from,
          target: e.to,
          edgeType: e.type,
          color: view === 'join' ? '#38bdf8' : edgeColor(e.type),
        },
        classes: view === 'join' ? 'highlighted' : '',
      });
    }

    cy.batch(() => {
      cy.elements().remove();
      cy.add(elements);
    });

    if (nodes.length > 0) {
      const run = cy.layout(layoutOptions(layout, nodes.length));
      run.run();
      cy.fit(undefined, 40);
      // Keep initial zoom readable instead of shrinking large graphs into unreadable specks.
      const z = cy.zoom();
      if (z < 0.72) {
        const best = [...nodes].sort((a, b) => (b.degree || 0) - (a.degree || 0))[0];
        const el = best ? cy.getElementById(best.id) : null;
        cy.zoom(0.88);
        if (el && !el.empty()) {
          cy.center(el);
        } else {
          cy.center();
        }
      } else if (z > 1.25) {
        cy.zoom(1.1);
        cy.center();
      }
      setZoomPct(Math.round(cy.zoom() * 100));
    }
  }, [nodes, edges, view, layout, groupByPackage]);

  // Highlight selected node + 1-hop neighborhood.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.batch(() => {
      cy.elements().removeClass('selected dimmed hovered highlighted');
      if (view === 'join') {
        cy.edges().addClass('highlighted');
      }
      if (!selectedId) return;
      const target = cy.getElementById(selectedId);
      if (target.empty()) return;
      const hood = target.closedNeighborhood();
      cy.elements().not(hood).not('node[?isGroup]').addClass('dimmed');
      target.addClass('selected');
      hood.edges().addClass('highlighted');
    });
  }, [selectedId, nodes, view]);

  const handleFit = () => cyRef.current?.fit(undefined, 40);
  const handleZoom = (factor: number) => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.zoom({
      level: cy.zoom() * factor,
      renderedPosition: { x: cy.width() / 2, y: cy.height() / 2 },
    });
  };
  const handleRelayout = () => {
    const cy = cyRef.current;
    if (!cy || nodes.length === 0) return;
    cy.layout(layoutOptions(layout, nodes.length)).run();
    cy.fit(undefined, 40);
  };
  const handleExportPNG = () => {
    const cy = cyRef.current;
    if (!cy) return;
    const png = cy.png({ bg: '#0b1120', full: true, scale: 2 });
    const a = document.createElement('a');
    a.href = png;
    a.download = `gatt-${view}.png`;
    a.click();
  };

  return (
    <div className={`canvas-wrap ${isPanning ? 'panning' : ''}`}>
      <div ref={containerRef} className="cy-container" />

      {ctxMenu && (
        <ContextMenu
          x={ctxMenu.x}
          y={ctxMenu.y}
          title={ctxMenu.title}
          items={ctxMenu.items}
          onClose={() => setCtxMenu(null)}
        />
      )}

      {loading && (
        <div className="canvas-overlay">
          <div className="spinner" />
          <span>Loading graph…</span>
        </div>
      )}

      {!loading && nodes.length === 0 && (
        <div className="canvas-overlay empty">
          <p className="empty-title">No nodes in this view</p>
          <p className="empty-sub">
            Try switching views (Architecture, Call Graph, Routes, Models) or clearing filters.
          </p>
        </div>
      )}

      {selectedPkg && (
        <div className="canvas-breadcrumb">
          <button type="button" onClick={() => onOpenPackage('')}>
            All Packages
          </button>
          <span>›</span>
          <strong>{selectedPkg}</strong>
        </div>
      )}

      {truncated && (
        <div className="truncated-banner">
          Showing <strong>{nodes.length}</strong> of <strong>{totalMatching}</strong> matching nodes.
          Click a folder in Explorer or search to drill down.
        </div>
      )}

      {view === 'impact' && (
        <div className="impact-legend">
          <span className="pill-dot" style={{ background: '#ef4444' }} /> Target
          <span className="pill-dot" style={{ background: '#f97316' }} /> Direct (1 hop)
          <span className="pill-dot" style={{ background: '#eab308' }} /> Transitive (2+ hops)
        </div>
      )}

      {view === 'diff' && (
        <div className="impact-legend">
          <span className="pill-dot" style={{ background: '#22c55e' }} /> Added
          <span className="pill-dot" style={{ background: '#f59e0b' }} /> Modified / Renamed
          <span className="pill-dot" style={{ background: '#ef4444' }} /> Removed
          <span className="pill-dot" style={{ background: '#38bdf8' }} /> Caller
        </div>
      )}

      <div className="canvas-toolbar">
        <button type="button" onClick={() => handleZoom(1.25)} title="Zoom in">
          +
        </button>
        <button type="button" onClick={() => handleZoom(0.8)} title="Zoom out">
          −
        </button>
        <button
          type="button"
          onClick={() => {
            const cy = cyRef.current;
            if (!cy) return;
            cy.zoom(1);
            cy.center();
          }}
          title="Reset to 100% readable zoom"
        >
          {zoomPct}%
        </button>
        <button type="button" onClick={handleFit} title="Fit whole graph to view">
          Fit
        </button>
        <span className="toolbar-sep" />
        <button
          type="button"
          className={layout === 'dagre-lr' ? 'active' : ''}
          onClick={() => onChangeLayout('dagre-lr')}
          title="Hierarchical Left → Right"
        >
          L→R
        </button>
        <button
          type="button"
          className={layout === 'dagre-tb' ? 'active' : ''}
          onClick={() => onChangeLayout('dagre-tb')}
          title="Hierarchical Top → Bottom"
        >
          T→B
        </button>
        <button
          type="button"
          className={layout === 'fcose' ? 'active' : ''}
          onClick={() => onChangeLayout('fcose')}
          title="Clustered Force (fCoSE)"
        >
          Cluster
        </button>
        <button
          type="button"
          className={groupByPackage ? 'active' : ''}
          onClick={onToggleGroup}
          title="Group nodes by folder / package"
        >
          Folders
        </button>
        <span className="toolbar-sep" />
        <button type="button" onClick={handleRelayout} title="Re-run layout">
          ↻
        </button>
        <button type="button" onClick={handleExportPNG} title="Export PNG">
          PNG
        </button>
        <button
          type="button"
          className={showLegend ? 'active' : ''}
          onClick={() => setShowLegend((v) => !v)}
          title="Toggle legend"
        >
          Legend
        </button>
      </div>

      {showLegend && (
        <div className="legend-popover">
          <div className="legend-title">Node Types</div>
          <div className="legend-grid">
            {[
              ['file', 'File'],
              ['package', 'Package'],
              ['function', 'Function'],
              ['definition', 'Type / Class'],
              ['route', 'Route / API'],
              ['model', 'Model / Table'],
              ['state', 'Shared State'],
            ].map(([type, label]) => (
              <div key={type} className="legend-item">
                <span className="dot" style={{ background: nodeColor(type) }} />
                <span>{label}</span>
              </div>
            ))}
          </div>
          <div className="legend-hint">
            Right-drag to pan · Right-click for context menu · Click to inspect · Double-click to
            expand / open folder.
          </div>
        </div>
      )}
    </div>
  );
};
