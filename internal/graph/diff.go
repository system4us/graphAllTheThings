package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Diff is the structural change between two extractions of the same source:
// which nodes appeared, disappeared, or had their attributes change, plus a
// per-type tally of added/removed edges (FKs, references, ...). It powers
// `gatt extract --check` (is the graph stale?) and the change summary printed
// on re-extraction.
type Diff struct {
	AddedNodes   []string     // node ids only in the new graph
	RemovedNodes []string     // node ids only in the old graph
	ChangedNodes []NodeChange // present in both, attributes differ
	AddedEdges   map[string]int
	RemovedEdges map[string]int
}

// NodeChange describes how one node's attributes changed, as short
// human-readable lines ("data_type: int → bigint", "+enum_values", "-comment").
type NodeChange struct {
	ID      string
	Type    string
	Changes []string
}

// Empty reports whether the two graphs are structurally identical.
func (d *Diff) Empty() bool {
	return len(d.AddedNodes) == 0 && len(d.RemovedNodes) == 0 && len(d.ChangedNodes) == 0 &&
		len(d.AddedEdges) == 0 && len(d.RemovedEdges) == 0
}

// ShortID strips the type prefix (and public. schema) from a node id for
// display, e.g. "table:public.users" -> "users", "schema:User" -> "User".
func ShortID(id string) string {
	for _, p := range []string{"table:", "view:", "column:", "index:", "schema:", "property:", "endpoint:", "api:"} {
		id = strings.TrimPrefix(id, p)
	}
	return strings.TrimPrefix(id, "public.")
}

// Text renders the drift as a compact multi-line summary (added/removed/changed
// nodes and per-type edge deltas), or "" when the graphs are identical. Shared
// by `gatt extract` output and the MCP check_drift/refresh tools.
func (d *Diff) Text() string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("schema drift:\n")
	for _, id := range d.AddedNodes {
		fmt.Fprintf(&b, "  + %s\n", ShortID(id))
	}
	for _, id := range d.RemovedNodes {
		fmt.Fprintf(&b, "  - %s\n", ShortID(id))
	}
	for _, c := range d.ChangedNodes {
		fmt.Fprintf(&b, "  ~ %s: %s\n", ShortID(c.ID), strings.Join(c.Changes, "; "))
	}
	for t, n := range d.AddedEdges {
		fmt.Fprintf(&b, "  + %d %s edge(s)\n", n, t)
	}
	for t, n := range d.RemovedEdges {
		fmt.Fprintf(&b, "  - %d %s edge(s)\n", n, t)
	}
	return b.String()
}

// DiffGraphs computes old → new. Both should be raw (un-annotated) graphs;
// see LoadRaw.
func DiffGraphs(old, new *Graph) *Diff {
	d := &Diff{AddedEdges: map[string]int{}, RemovedEdges: map[string]int{}}

	for id := range new.Nodes {
		if old.Nodes[id] == nil {
			d.AddedNodes = append(d.AddedNodes, id)
		}
	}
	for id, on := range old.Nodes {
		nn := new.Nodes[id]
		if nn == nil {
			d.RemovedNodes = append(d.RemovedNodes, id)
			continue
		}
		if ch := attrChanges(on.Attrs, nn.Attrs); len(ch) > 0 {
			d.ChangedNodes = append(d.ChangedNodes, NodeChange{ID: id, Type: nn.Type, Changes: ch})
		}
	}
	sort.Strings(d.AddedNodes)
	sort.Strings(d.RemovedNodes)
	sort.Slice(d.ChangedNodes, func(i, j int) bool { return d.ChangedNodes[i].ID < d.ChangedNodes[j].ID })

	// Edges have no identity, so compare multisets keyed by content.
	oldEdges := edgeCounts(old.Edges)
	newEdges := edgeCounts(new.Edges)
	for key, n := range newEdges {
		if extra := n - oldEdges[key]; extra > 0 {
			d.AddedEdges[edgeType(key)] += extra
		}
	}
	for key, n := range oldEdges {
		if extra := n - newEdges[key]; extra > 0 {
			d.RemovedEdges[edgeType(key)] += extra
		}
	}
	return d
}

// attrChanges returns per-key change lines between two attribute maps.
// Values are truncated and DDL/SQL blobs are compared but rendered as a bare
// "changed" so the summary stays a few characters wide.
func attrChanges(oldA, newA map[string]string) []string {
	keys := map[string]bool{}
	for k := range oldA {
		keys[k] = true
	}
	for k := range newA {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		ov, oOK := oldA[k]
		nv, nOK := newA[k]
		switch {
		case oOK && !nOK:
			out = append(out, "-"+k)
		case !oOK && nOK:
			out = append(out, "+"+k)
		case ov != nv:
			if k == "sql" || len(ov) > 32 || len(nv) > 32 {
				out = append(out, k+" changed")
			} else {
				out = append(out, fmt.Sprintf("%s: %s → %s", k, ov, nv))
			}
		}
	}
	sort.Strings(out)
	return out
}

// edgeCounts builds a multiset of edges keyed by a canonical string so
// re-ordering between extractions doesn't register as a change.
func edgeCounts(edges []Edge) map[string]int {
	m := map[string]int{}
	for _, e := range edges {
		m[edgeKey(e)]++
	}
	return m
}

func edgeKey(e Edge) string {
	var b strings.Builder
	b.WriteString(e.Type)
	b.WriteByte('\x00')
	b.WriteString(e.From)
	b.WriteByte('\x00')
	b.WriteString(e.To)
	if len(e.Attrs) > 0 {
		ks := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			b.WriteByte('\x00')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(e.Attrs[k])
		}
	}
	return b.String()
}

func edgeType(key string) string {
	t, _, _ := strings.Cut(key, "\x00")
	return t
}
