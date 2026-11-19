import React, { useState } from 'react';
import { api } from './api';
import type {
  AffectedTestsResponse,
  ContextResponse,
  DiffResponse,
  DocDriftResponse,
  GraphEdge,
  GraphNode,
  GraphView,
  GrepResponse,
  HotspotsResponse,
  JoinResponse,
} from './types';

export type ToolTab = 'grep' | 'diff' | 'ask' | 'tests' | 'hotspots' | 'drift' | 'join';

interface ToolsPanelProps {
  tab: ToolTab;
  repo?: string;
  onJumpNode: (id: string) => void;
  onShowOverlay: (view: GraphView, nodes: GraphNode[], edges: GraphEdge[], label?: string) => void;
}

export const ToolsPanel: React.FC<ToolsPanelProps> = ({
  tab,
  repo,
  onJumpNode,
  onShowOverlay,
}) => {
  // Grep state
  const [grepQ, setGrepQ] = useState('');
  const [grepRegex, setGrepRegex] = useState(false);
  const [grepInclude, setGrepInclude] = useState('');
  const [grepExclude, setGrepExclude] = useState('');
  const [grepRes, setGrepRes] = useState<GrepResponse | null>(null);
  const [grepLoading, setGrepLoading] = useState(false);
  const [grepErr, setGrepErr] = useState<string | null>(null);

  // Diff state
  const [diffRef, setDiffRef] = useState('HEAD');
  const [diffRes, setDiffRes] = useState<DiffResponse | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);
  const [diffErr, setDiffErr] = useState<string | null>(null);

  // Ask / Context state
  const [askQ, setAskQ] = useState('');
  const [askBudget, setAskBudget] = useState<number>(0);
  const [askRes, setAskRes] = useState<ContextResponse | null>(null);
  const [askLoading, setAskLoading] = useState(false);
  const [askErr, setAskErr] = useState<string | null>(null);

  // Tests state
  const [testTarget, setTestTarget] = useState('');
  const [testRes, setTestRes] = useState<AffectedTestsResponse | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [testErr, setTestErr] = useState<string | null>(null);
  const [copiedCmd, setCopiedCmd] = useState<string | null>(null);

  // Hotspots state
  const [hotRes, setHotRes] = useState<HotspotsResponse | null>(null);
  const [hotLoading, setHotLoading] = useState(false);
  const [hotErr, setHotErr] = useState<string | null>(null);

  // Doc drift state
  const [driftRes, setDriftRes] = useState<DocDriftResponse | null>(null);
  const [driftLoading, setDriftLoading] = useState(false);
  const [driftErr, setDriftErr] = useState<string | null>(null);

  // Join path state
  const [joinFrom, setJoinFrom] = useState('');
  const [joinTo, setJoinTo] = useState('');
  const [joinRes, setJoinRes] = useState<JoinResponse | null>(null);
  const [joinLoading, setJoinLoading] = useState(false);
  const [joinErr, setJoinErr] = useState<string | null>(null);

  const runGrep = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!grepQ.trim()) return;
    setGrepLoading(true);
    setGrepErr(null);
    try {
      const res = await api.grep({
        repo,
        q: grepQ,
        regex: grepRegex,
        include: grepInclude.trim() || undefined,
        exclude: grepExclude.trim() || undefined,
        limit: 120,
      });
      setGrepRes(res);
    } catch (err: unknown) {
      setGrepErr(err instanceof Error ? err.message : 'grep failed');
    } finally {
      setGrepLoading(false);
    }
  };

  const runDiff = async (e: React.FormEvent) => {
    e.preventDefault();
    setDiffLoading(true);
    setDiffErr(null);
    try {
      const res = await api.diff(diffRef.trim() || 'HEAD', repo);
      setDiffRes(res);
      if (res.nodes.length > 0) {
        onShowOverlay('diff', res.nodes, res.edges, `Diff vs ${res.ref}`);
      }
    } catch (err: unknown) {
      setDiffErr(err instanceof Error ? err.message : 'diff failed');
    } finally {
      setDiffLoading(false);
    }
  };

  const runAsk = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!askQ.trim()) return;
    setAskLoading(true);
    setAskErr(null);
    try {
      const res = await api.context(askQ.trim(), repo, 8, askBudget || undefined);
      setAskRes(res);
      if (res.nodes.length > 0) {
        onShowOverlay('context', res.nodes, res.edges, 'Context');
      }
    } catch (err: unknown) {
      setAskErr(err instanceof Error ? err.message : 'context query failed');
    } finally {
      setAskLoading(false);
    }
  };

  const runTests = async (e: React.FormEvent) => {
    e.preventDefault();
    setTestLoading(true);
    setTestErr(null);
    try {
      const res = await api.tests(testTarget.trim() || undefined, repo, 3);
      setTestRes(res);
      if (res.nodes.length > 0) {
        onShowOverlay('diff', res.nodes, res.edges, `Tests: ${res.target}`);
      }
    } catch (err: unknown) {
      setTestErr(err instanceof Error ? err.message : 'affected_tests failed');
    } finally {
      setTestLoading(false);
    }
  };

  const runHotspots = async () => {
    setHotLoading(true);
    setHotErr(null);
    try {
      const res = await api.hotspots(repo, 25);
      setHotRes(res);
      if (res.nodes.length > 0) {
        onShowOverlay('diff', res.nodes, res.edges, 'Churn Hotspots');
      }
    } catch (err: unknown) {
      setHotErr(err instanceof Error ? err.message : 'hotspots failed');
    } finally {
      setHotLoading(false);
    }
  };

  const runDrift = async () => {
    setDriftLoading(true);
    setDriftErr(null);
    try {
      const res = await api.docDrift(repo);
      setDriftRes(res);
    } catch (err: unknown) {
      setDriftErr(err instanceof Error ? err.message : 'doc-drift failed');
    } finally {
      setDriftLoading(false);
    }
  };

  const runJoin = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!joinFrom.trim() || !joinTo.trim()) return;
    setJoinLoading(true);
    setJoinErr(null);
    try {
      const res = await api.join(joinFrom.trim(), joinTo.trim(), repo);
      setJoinRes(res);
      if (res.found && res.nodes.length > 0) {
        onShowOverlay('join', res.nodes, res.edges, `Join: ${joinFrom} → ${joinTo}`);
      }
    } catch (err: unknown) {
      setJoinErr(err instanceof Error ? err.message : 'join failed');
    } finally {
      setJoinLoading(false);
    }
  };

  if (tab === 'grep') {
    return (
      <div className="tool-pane">
        <form onSubmit={runGrep} className="tool-form">
          <input
            type="text"
            placeholder="Literal or regex pattern…"
            value={grepQ}
            onChange={(e) => setGrepQ(e.target.value)}
          />
          <div className="tool-row">
            <input
              type="text"
              placeholder="Include (*.go, src/**)"
              value={grepInclude}
              onChange={(e) => setGrepInclude(e.target.value)}
            />
            <input
              type="text"
              placeholder="Exclude (*_test.go)"
              value={grepExclude}
              onChange={(e) => setGrepExclude(e.target.value)}
            />
          </div>
          <div className="tool-row between">
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={grepRegex}
                onChange={(e) => setGrepRegex(e.target.checked)}
              />
              <span>Regex</span>
            </label>
            <button type="submit" className="primary-btn" disabled={grepLoading}>
              {grepLoading ? 'Scanning…' : 'Grep'}
            </button>
          </div>
        </form>

        {grepErr && <div className="tool-error">{grepErr}</div>}

        {grepRes && (
          <div className="tool-results">
            <div className="tool-summary">
              <strong>{grepRes.total}</strong> match(es) across{' '}
              <strong>{grepRes.files_scanned}</strong> files
              {grepRes.total === 0 ? ' — proof of absence' : ''}
            </div>
            <div className="grep-list">
              {grepRes.hits.map((h, i) => (
                <button
                  key={`${h.path}:${h.line}:${i}`}
                  type="button"
                  className="grep-hit"
                  onClick={() => h.node_id && onJumpNode(h.node_id)}
                  disabled={!h.node_id}
                  title={h.node_id ? `Jump to ${h.node_label || h.path}` : h.path}
                >
                  <div className="grep-loc">
                    <span>
                      {h.path}:{h.line}
                    </span>
                    {h.node_label && <span className="grep-sym">{h.node_label}</span>}
                  </div>
                  <div className="grep-text">{h.text.trim()}</div>
                </button>
              ))}
            </div>
          </div>
        )}
      </div>
    );
  }

  if (tab === 'diff') {
    return (
      <div className="tool-pane">
        <form onSubmit={runDiff} className="tool-form">
          <div className="tool-row">
            <input
              type="text"
              placeholder="Git ref (HEAD, main, SHA…)"
              value={diffRef}
              onChange={(e) => setDiffRef(e.target.value)}
            />
            <button type="submit" className="primary-btn" disabled={diffLoading}>
              {diffLoading ? 'Diffing…' : 'Run Diff'}
            </button>
          </div>
        </form>

        {diffErr && <div className="tool-error">{diffErr}</div>}

        {diffRes && (
          <div className="tool-results">
            <div className="tool-summary">
              vs <code>{diffRes.ref}</code>: <strong>{diffRes.total_changes}</strong> structural
              change(s) in <strong>{diffRes.files_touched}</strong> file(s)
            </div>
            {diffRes.nodes.length > 0 && (
              <button
                type="button"
                className="secondary-btn full"
                onClick={() =>
                  onShowOverlay('diff', diffRes.nodes, diffRes.edges, `Diff vs ${diffRes.ref}`)
                }
              >
                Highlight on Graph ({diffRes.nodes.length} nodes)
              </button>
            )}
            <div className="diff-list">
              {diffRes.changes.length === 0 ? (
                <div className="muted-note">No structural symbol changes detected.</div>
              ) : (
                diffRes.changes.map((c, i) => {
                  const targetId = c.new_id || c.old_id;
                  const title = c.new_name || c.old_name || '(unknown)';
                  return (
                    <div key={i} className={`diff-card ${c.kind}`}>
                      <button
                        type="button"
                        className="diff-head"
                        onClick={() => targetId && onJumpNode(targetId)}
                      >
                        <span className={`diff-tag ${c.kind}`}>{c.kind}</span>
                        <span className="diff-name">{title}</span>
                      </button>
                      <div className="diff-file">{c.new_file || c.old_file}</div>
                      {c.callers && c.callers.length > 0 && (
                        <div className="diff-callers">
                          <span>Callers ({c.callers.length}):</span>
                          {c.callers.slice(0, 6).map((cl) => (
                            <button
                              key={cl.id}
                              type="button"
                              className="mini-chip"
                              onClick={() => onJumpNode(cl.id)}
                            >
                              {cl.label}
                            </button>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })
              )}
            </div>
          </div>
        )}
      </div>
    );
  }

  if (tab === 'ask') {
    return (
      <div className="tool-pane">
        <form onSubmit={runAsk} className="tool-form">
          <textarea
            rows={2}
            placeholder="Ask a codebase or SQL question… e.g. 'how does incremental refresh work?'"
            value={askQ}
            onChange={(e) => setAskQ(e.target.value)}
          />
          <div className="tool-row between">
            <select
              value={askBudget}
              onChange={(e) => setAskBudget(Number(e.target.value))}
              className="budget-select"
              title="Approximate token budget"
            >
              <option value={0}>Budget: Default</option>
              <option value={1200}>Compact (~1.2k tok)</option>
              <option value={3000}>Standard (~3k tok)</option>
              <option value={6000}>Deep (~6k tok)</option>
            </select>
            <button type="submit" className="primary-btn" disabled={askLoading}>
              {askLoading ? 'Building…' : 'Build Context'}
            </button>
          </div>
        </form>

        {askErr && <div className="tool-error">{askErr}</div>}

        {askRes && (
          <div className="tool-results">
            <div className="tool-summary">
              Method: <strong>{askRes.method || 'hybrid'}</strong> ·{' '}
              <strong>{askRes.nodes.length}</strong> graph nodes highlighted
            </div>
            <pre className="context-output">{askRes.text}</pre>
          </div>
        )}
      </div>
    );
  }

  if (tab === 'tests') {
    return (
      <div className="tool-pane">
        <form onSubmit={runTests} className="tool-form">
          <p className="muted-note" style={{ padding: 0, margin: 0 }}>
            Find minimal affected tests for a file, symbol, or leave empty for current git diff.
          </p>
          <div className="tool-row">
            <input
              type="text"
              placeholder="Empty = git diff, or file/symbol…"
              value={testTarget}
              onChange={(e) => setTestTarget(e.target.value)}
            />
            <button type="submit" className="primary-btn" disabled={testLoading}>
              {testLoading ? 'Finding…' : 'Tests'}
            </button>
          </div>
        </form>

        {testErr && <div className="tool-error">{testErr}</div>}

        {testRes && (
          <div className="tool-results">
            <div className="tool-summary">
              <strong>{testRes.test_files?.length ?? 0}</strong> test file(s) ·{' '}
              <strong>{testRes.test_functions?.length ?? 0}</strong> direct test fn(s)
            </div>
            {testRes.commands?.map((cmd) => (
              <div key={cmd} className="cmd-box">
                <code>{cmd}</code>
                <button
                  type="button"
                  className="mini-chip"
                  onClick={() => {
                    navigator.clipboard?.writeText(cmd).catch(() => {});
                    setCopiedCmd(cmd);
                    setTimeout(() => setCopiedCmd(null), 1500);
                  }}
                >
                  {copiedCmd === cmd ? 'Copied' : 'Copy'}
                </button>
              </div>
            ))}
            {testRes.test_functions && testRes.test_functions.length > 0 && (
              <div className="diff-list">
                {testRes.test_functions.map((tf) => (
                  <button
                    key={tf.id}
                    type="button"
                    className="grep-hit"
                    onClick={() => onJumpNode(tf.id)}
                  >
                    <div className="grep-loc">
                      <span className="grep-sym">{tf.name}</span>
                      <span>
                        {tf.file}:{tf.line}
                      </span>
                    </div>
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    );
  }

  if (tab === 'hotspots') {
    return (
      <div className="tool-pane">
        <div className="tool-form">
          <p className="muted-note" style={{ padding: 0, margin: 0 }}>
            Files with the highest git churn × dependency connectivity (architectural risk hotspots).
          </p>
          <button
            type="button"
            className="primary-btn"
            onClick={runHotspots}
            disabled={hotLoading}
          >
            {hotLoading ? 'Analyzing…' : 'Analyze Hotspots'}
          </button>
        </div>

        {hotErr && <div className="tool-error">{hotErr}</div>}

        {hotRes && (
          <div className="tool-results">
            <div className="diff-list">
              {hotRes.hotspots.map((hs, idx) => (
                <button
                  key={hs.node_id}
                  type="button"
                  className="grep-hit"
                  onClick={() => onJumpNode(hs.node_id)}
                >
                  <div className="grep-loc">
                    <span>
                      #{idx + 1} {hs.path}
                    </span>
                  </div>
                  <div className="grep-text">
                    {hs.commits} commits · {hs.degree} edges · score {hs.score}
                  </div>
                </button>
              ))}
            </div>
          </div>
        )}
      </div>
    );
  }

  if (tab === 'drift') {
    return (
      <div className="tool-pane">
        <div className="tool-form">
          <p className="muted-note" style={{ padding: 0, margin: 0 }}>
            Check markdown docs for broken symbol references or stale documentation modified before
            the code it documents.
          </p>
          <button
            type="button"
            className="primary-btn"
            onClick={runDrift}
            disabled={driftLoading}
          >
            {driftLoading ? 'Checking…' : 'Check Doc Drift'}
          </button>
        </div>

        {driftErr && <div className="tool-error">{driftErr}</div>}

        {driftRes && (
          <div className="tool-results">
            {driftRes.docs && driftRes.docs.length > 0 && (
              <div className="drift-docs">
                {driftRes.docs.map((d) => (
                  <button
                    key={d.id}
                    type="button"
                    className="mini-chip"
                    onClick={() => onJumpNode(d.id)}
                  >
                    📄 {d.path}
                  </button>
                ))}
              </div>
            )}
            <pre className="context-output">{driftRes.text}</pre>
          </div>
        )}
      </div>
    );
  }

  // tab === 'join'
  return (
    <div className="tool-pane">
      <form onSubmit={runJoin} className="tool-form">
        <p className="muted-note" style={{ padding: 0, margin: 0 }}>
          Find the cheapest FK / $ref join path between two SQL tables or OpenAPI schemas.
        </p>
        <input
          type="text"
          placeholder="From table / schema (e.g. users)"
          value={joinFrom}
          onChange={(e) => setJoinFrom(e.target.value)}
        />
        <input
          type="text"
          placeholder="To table / schema (e.g. products)"
          value={joinTo}
          onChange={(e) => setJoinTo(e.target.value)}
        />
        <button type="submit" className="primary-btn" disabled={joinLoading}>
          {joinLoading ? 'Computing…' : 'Find Join Path'}
        </button>
      </form>

      {joinErr && <div className="tool-error">{joinErr}</div>}

      {joinRes && (
        <div className="tool-results">
          <div className="tool-summary">
            {joinRes.found
              ? `Found ${joinRes.steps?.length ?? 0}-hop join path`
              : 'No join path found'}
          </div>
          {joinRes.hint && <div className="muted-note">{joinRes.hint}</div>}
          <pre className="context-output">{joinRes.text}</pre>
        </div>
      )}
    </div>
  );
};
