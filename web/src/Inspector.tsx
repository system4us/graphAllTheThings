import React, { useEffect, useState } from 'react';
import { api } from './api';
import { edgeColor, nodeColor, type NodeDetailResponse } from './types';

interface InspectorProps {
  repo?: string;
  detail: NodeDetailResponse | null;
  loading: boolean;
  onClose: () => void;
  onNavigateNode: (id: string) => void;
  onFocusNeighbors: (id: string) => void;
  onFocusImpact: (id: string) => void;
  onFocusCalls: (id: string) => void;
  onFindTests: (id: string) => void;
  onOpenPackage: (pkg: string) => void;
  onAnnotated: () => void;
}

export const Inspector: React.FC<InspectorProps> = ({
  repo,
  detail,
  loading,
  onClose,
  onNavigateNode,
  onFocusNeighbors,
  onFocusImpact,
  onFocusCalls,
  onFindTests,
  onOpenPackage,
  onAnnotated,
}) => {
  const [annOpen, setAnnOpen] = useState(false);
  const [entityNote, setEntityNote] = useState('');
  const [defaultFilter, setDefaultFilter] = useState('');
  const [routeMethod, setRouteMethod] = useState('');
  const [routePath, setRoutePath] = useState('');
  const [modelTable, setModelTable] = useState('');
  const [saving, setSaving] = useState(false);
  const [annMsg, setAnnMsg] = useState<string | null>(null);
  const [diffOpen, setDiffOpen] = useState(false);
  const [diffText, setDiffText] = useState<string | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);

  useEffect(() => {
    setEntityNote(detail?.attrs?.entity_note ?? '');
    setDefaultFilter(detail?.attrs?.default_filter ?? '');
    setRouteMethod(detail?.attrs?.route_method ?? '');
    setRoutePath(detail?.attrs?.route_path ?? '');
    setModelTable(detail?.attrs?.model_table ?? '');
    setAnnMsg(null);
    setDiffOpen(false);
    setDiffText(null);
  }, [detail]);

  const toggleGitDiff = async () => {
    if (!detail?.file) return;
    if (diffOpen) {
      setDiffOpen(false);
      return;
    }
    setDiffOpen(true);
    setDiffLoading(true);
    try {
      const res = await api.nodeDiff(detail.file, 'HEAD', repo);
      setDiffText(res.diff || 'No uncommitted changes vs HEAD.');
    } catch (err: unknown) {
      setDiffText(err instanceof Error ? err.message : 'Failed to load git diff');
    } finally {
      setDiffLoading(false);
    }
  };

  if (!detail && !loading) return null;

  const saveAnnotation = async (clear = false) => {
    if (!detail) return;
    setSaving(true);
    setAnnMsg(null);
    try {
      await api.annotate(
        {
          id: detail.id,
          entity_note: entityNote.trim(),
          default_filter: defaultFilter.trim(),
          route_method: routeMethod.trim(),
          route_path: routePath.trim(),
          model_table: modelTable.trim(),
          clear,
        },
        repo,
      );
      setAnnMsg(clear ? 'Annotations cleared' : 'Saved to annotations.json');
      onAnnotated();
    } catch (err: unknown) {
      setAnnMsg(err instanceof Error ? err.message : 'Failed to save');
    } finally {
      setSaving(false);
    }
  };

  const incoming = detail?.incoming ?? [];
  const outgoing = detail?.outgoing ?? [];
  const attrEntries = detail?.attrs
    ? Object.entries(detail.attrs).filter(([k, v]) => k !== 'body' && v !== '')
    : [];

  return (
    <aside className="inspector">
      <div className="inspector-header">
        {detail ? (
          <div className="inspector-title-wrap">
            <span className="type-badge" style={{ background: nodeColor(detail.type) }}>
              {detail.type}
            </span>
            <h3 title={detail.name}>{detail.label}</h3>
          </div>
        ) : (
          <h3>Loading…</h3>
        )}
        <button type="button" className="icon-btn" onClick={onClose} title="Close (Esc)">
          ×
        </button>
      </div>

      {loading && <div className="inspector-loading">Fetching node details…</div>}

      {detail && !loading && (
        <div className="inspector-body">
          {detail.file && (
            <div className="inspector-path" title={detail.file}>
              {detail.file}
              {detail.attrs?.line_start ? `:${detail.attrs.line_start}` : ''}
              {detail.attrs?.line_end && detail.attrs.line_end !== detail.attrs.line_start
                ? `–${detail.attrs.line_end}`
                : ''}
            </div>
          )}

          <div className="inspector-actions">
            {detail.id.startsWith('dir:') ? (
              <button
                type="button"
                className="primary-btn"
                onClick={() => onOpenPackage(detail.id.slice(4))}
              >
                Open Folder in Graph →
              </button>
            ) : (
              <>
                <button type="button" onClick={() => onFocusNeighbors(detail.id)}>
                  Expand Neighbors
                </button>
                <button
                  type="button"
                  className="danger-btn"
                  onClick={() => onFocusImpact(detail.id)}
                >
                  Blast / Impact
                </button>
                {(detail.type === 'function' || detail.type === 'definition') && (
                  <button type="button" onClick={() => onFocusCalls(detail.id)}>
                    Call Graph
                  </button>
                )}
                <button type="button" onClick={() => onFindTests(detail.id)}>
                  Affected Tests
                </button>
                {detail.file && (
                  <button
                    type="button"
                    className={diffOpen ? 'active-btn' : ''}
                    onClick={toggleGitDiff}
                  >
                    ± Git Diff
                  </button>
                )}
                <button
                  type="button"
                  className={annOpen ? 'active-btn' : ''}
                  onClick={() => setAnnOpen((v) => !v)}
                >
                  ✎ Annotate
                </button>
              </>
            )}
          </div>

          {diffOpen && (
            <section className="inspector-section">
              <div className="section-title">Git Diff vs HEAD</div>
              {diffLoading ? (
                <div className="muted-note">Loading diff…</div>
              ) : (
                <div className="code-box diff-box">
                  <pre>
                    {(diffText ?? '').split('\n').map((line, idx) => {
                      const cls = line.startsWith('+') && !line.startsWith('+++')
                        ? 'diff-add'
                        : line.startsWith('-') && !line.startsWith('---')
                          ? 'diff-del'
                          : line.startsWith('@@')
                            ? 'diff-hunk'
                            : '';
                      return (
                        <div key={idx} className={`code-line ${cls}`}>
                          <span className="line-txt">{line}</span>
                        </div>
                      );
                    })}
                  </pre>
                </div>
              )}
            </section>
          )}

          {annOpen && (
            <section className="annotate-box">
              <div className="section-title">Curate Business Knowledge</div>
              <textarea
                rows={2}
                placeholder="Entity note (persisted in annotations.json)…"
                value={entityNote}
                onChange={(e) => setEntityNote(e.target.value)}
              />
              <input
                type="text"
                placeholder="Default filter (e.g. deleted_at IS NULL)"
                value={defaultFilter}
                onChange={(e) => setDefaultFilter(e.target.value)}
              />
              <div className="tool-row">
                <input
                  type="text"
                  placeholder="Route method (GET)"
                  value={routeMethod}
                  onChange={(e) => setRouteMethod(e.target.value)}
                />
                <input
                  type="text"
                  placeholder="Route path (/api/…)"
                  value={routePath}
                  onChange={(e) => setRoutePath(e.target.value)}
                />
              </div>
              <input
                type="text"
                placeholder="ORM model table name"
                value={modelTable}
                onChange={(e) => setModelTable(e.target.value)}
              />
              <div className="tool-row between">
                <button
                  type="button"
                  className="secondary-btn"
                  disabled={saving}
                  onClick={() => saveAnnotation(true)}
                >
                  Clear
                </button>
                <button
                  type="button"
                  className="primary-btn"
                  disabled={saving}
                  onClick={() => saveAnnotation(false)}
                >
                  {saving ? 'Saving…' : 'Save Annotation'}
                </button>
              </div>
              {annMsg && <div className="ann-status">{annMsg}</div>}
            </section>
          )}

          {detail.source && (
            <section className="inspector-section">
              <div className="section-title">
                Source Code
                <span className="section-meta">
                  L{detail.source.line_start}–{detail.source.line_end}
                  {detail.source.truncated ? ' (truncated)' : ''}
                </span>
              </div>
              <div className="code-box">
                <pre>
                  {detail.source.content.split('\n').map((line, idx) => (
                    <div key={idx} className="code-line">
                      <span className="line-no">{detail.source!.line_start + idx}</span>
                      <span className="line-txt">{line}</span>
                    </div>
                  ))}
                </pre>
              </div>
            </section>
          )}

          <section className="inspector-section">
            <div className="section-title">
              Incoming <span className="section-meta">{incoming.length}</span>
            </div>
            {incoming.length === 0 ? (
              <div className="muted-note">No incoming edges.</div>
            ) : (
              <div className="neighbor-list">
                {incoming.slice(0, 60).map((n, i) => (
                  <button
                    key={`${n.id}-${n.edge_type}-${i}`}
                    type="button"
                    className="neighbor-item"
                    onClick={() => onNavigateNode(n.id)}
                  >
                    <span className="dot" style={{ background: nodeColor(n.type) }} />
                    <span className="neighbor-name" title={n.name}>
                      {n.label}
                    </span>
                    <span className="edge-pill" style={{ borderColor: edgeColor(n.edge_type) }}>
                      {n.edge_type}
                    </span>
                  </button>
                ))}
              </div>
            )}
          </section>

          <section className="inspector-section">
            <div className="section-title">
              Outgoing <span className="section-meta">{outgoing.length}</span>
            </div>
            {outgoing.length === 0 ? (
              <div className="muted-note">No outgoing edges.</div>
            ) : (
              <div className="neighbor-list">
                {outgoing.slice(0, 60).map((n, i) => (
                  <button
                    key={`${n.id}-${n.edge_type}-${i}`}
                    type="button"
                    className="neighbor-item"
                    onClick={() => onNavigateNode(n.id)}
                  >
                    <span className="dot" style={{ background: nodeColor(n.type) }} />
                    <span className="neighbor-name" title={n.name}>
                      {n.label}
                    </span>
                    <span className="edge-pill" style={{ borderColor: edgeColor(n.edge_type) }}>
                      {n.edge_type}
                    </span>
                  </button>
                ))}
              </div>
            )}
          </section>

          {detail.history && detail.history.length > 0 && (
            <section className="inspector-section">
              <div className="section-title">
                Git History <span className="section-meta">{detail.history.length} commits</span>
              </div>
              <div className="history-list">
                {detail.history.map((c) => (
                  <div key={c.sha} className="history-item">
                    <div className="history-head">
                      <code>{c.sha}</code>
                      <span>{c.date}</span>
                      <span className="history-author">{c.author}</span>
                    </div>
                    <div className="history-subj">{c.subject}</div>
                    {c.issues && c.issues.length > 0 && (
                      <div className="history-issues">
                        {c.issues.map((iss) => (
                          <span key={iss} className="issue-chip">
                            {iss}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </section>
          )}

          {attrEntries.length > 0 && (
            <section className="inspector-section">
              <div className="section-title">Attributes</div>
              <dl className="attr-list">
                {attrEntries.map(([k, v]) => (
                  <div key={k} className="attr-row">
                    <dt>{k}</dt>
                    <dd>{v}</dd>
                  </div>
                ))}
              </dl>
            </section>
          )}
        </div>
      )}
    </aside>
  );
};
