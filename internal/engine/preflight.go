package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/system4us/graphAllTheThings/internal/codeutil"
	"github.com/system4us/graphAllTheThings/internal/connector/codebase"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// PreflightCheck runs a single composite post-edit verification pack against
// git diff vs ref (default HEAD): structural code diff, affected tests +
// ready-to-run commands, risk alerts (edited generated files, touched hotspots,
// gotcha/invariant notes on changed symbols), and relevant doc drift.
func (e *Engine) PreflightCheck(ctx context.Context, ref string, maxTokens int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("preflight_check requires a codebase graph")
	}
	if ref == "" {
		ref = "HEAD"
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	pairs, err := codebase.GitChangedFiles(ctx, dir, ref)
	if err != nil {
		return "", fmt.Errorf("git diff %s: %w", ref, err)
	}
	if len(pairs) == 0 {
		return fmt.Sprintf("preflight_check vs %s: working tree clean (no changes).\n", ref), nil
	}

	changedFiles := map[string]bool{}
	for _, p := range pairs {
		if p.New != "" {
			changedFiles[filepath.ToSlash(p.New)] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Preflight Check (vs %s — %d file(s) changed)\n\n", ref, len(changedFiles))

	// 1. Risk alerts: generated files edited directly, hotspots touched, gotchas/invariants
	var alerts []string
	hotspotMap := map[string]HotspotEntry{}
	if hs, err := e.Hotspots(ctx, 20); err == nil {
		for _, h := range hs {
			hotspotMap[h.Path] = h
		}
	}

	for _, n := range e.G.Nodes {
		if n.Attrs["external"] == "true" {
			continue
		}
		f := filepath.ToSlash(n.Attrs["file"])
		if f == "" && n.Type == graph.NodeFile {
			f = filepath.ToSlash(n.Name)
		}
		if !changedFiles[f] {
			continue
		}
		if n.Type == graph.NodeFile && n.Attrs["generated"] == "true" {
			var sources []string
			for _, ed := range e.G.EdgesOf(n.ID) {
				if ed.Type == graph.EdgeGenerates && ed.To == n.ID {
					if sn := e.G.Nodes[ed.From]; sn != nil {
						sources = append(sources, sn.Name)
					}
				}
			}
			if len(sources) > 0 {
				alerts = append(alerts, fmt.Sprintf("GENERATED FILE EDITED: %s (generated from %s)", f, strings.Join(sources, ", ")))
			} else {
				alerts = append(alerts, fmt.Sprintf("GENERATED FILE EDITED: %s", f))
			}
		}
		if inv := n.Attrs["invariant"]; inv != "" {
			alerts = append(alerts, fmt.Sprintf("INVARIANT on %s (%s): %s", n.Name, f, inv))
		}
		if g := n.Attrs["gotcha"]; g != "" {
			alerts = append(alerts, fmt.Sprintf("GOTCHA on %s (%s): %s", n.Name, f, g))
		}
	}
	for f := range changedFiles {
		if h, ok := hotspotMap[f]; ok {
			alerts = append(alerts, fmt.Sprintf("HOTSPOT TOUCHED: %s (%d recent commits, degree %d)", f, h.Commits, h.Degree))
		}
	}
	sort.Strings(alerts)
	if len(alerts) > 0 {
		b.WriteString("## Risk Alerts\n")
		for _, a := range alerts {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}

	// 2. Structural code diff
	diffOut, diff, err := e.codeDiff(ctx, ref, 25)
	if err == nil && strings.TrimSpace(diffOut) != "" {
		b.WriteString("## Structural Diff\n")
		b.WriteString(strings.TrimSpace(diffOut))
		b.WriteString("\n\n")
	}

	// 3. Affected tests
	if testRes, err := e.AffectedTests(ctx, "diff:"+ref, 3); err == nil && strings.TrimSpace(testRes.Text) != "" {
		b.WriteString("## Affected Tests\n")
		b.WriteString(strings.TrimSpace(testRes.Text))
		b.WriteString("\n\n")
	}

	// 4. Docs that mention what actually changed: a symbol whose signature or
	// doc changed (or was renamed/moved), or a changed file by path. A doc
	// that merely mentions some other symbol of an edited file is not affected
	// by the edit — listing those made every README show up on every diff.
	changedIDs := map[string]string{} // node id -> what to call it
	if diff != nil {
		for _, c := range diff.Changes {
			if c.NewID != "" && c.Kind != codeutil.CodeAdded {
				changedIDs[c.NewID] = "symbol " + c.NewName
			}
		}
	}
	reasons := map[string][]string{}
	for _, ed := range e.G.Edges {
		if ed.Type != graph.EdgeMentions {
			continue
		}
		tn, dn := e.G.Nodes[ed.To], e.G.Nodes[ed.From]
		if tn == nil || dn == nil || changedFiles[filepath.ToSlash(dn.Name)] || changedFiles[filepath.ToSlash(dn.Attrs["file"])] {
			continue
		}
		why := changedIDs[tn.ID]
		if why == "" && tn.Type == graph.NodeFile && changedFiles[filepath.ToSlash(tn.Name)] && !isProseDoc(tn.Name) {
			why = "file " + tn.Name
		}
		if why != "" {
			reasons[dn.Name] = append(reasons[dn.Name], why)
		}
	}
	if len(reasons) > 0 {
		docs := make([]string, 0, len(reasons))
		for d := range reasons {
			docs = append(docs, d)
		}
		sort.Strings(docs)
		b.WriteString("## Docs Potentially Affected\n")
		for _, d := range docs {
			why := dedupeStrings(reasons[d])
			if len(why) > 3 {
				why = append(why[:3], fmt.Sprintf("+%d more", len(why)-3))
			}
			fmt.Fprintf(&b, "- %s (mentions changed %s)\n", d, strings.Join(why, ", "))
		}
		b.WriteString("\n")
	}

	return TrimToTokenBudget(b.String(), maxTokens), nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
