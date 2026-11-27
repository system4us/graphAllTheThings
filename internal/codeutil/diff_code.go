package codeutil

import (
	"strings"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// FilePair is one file touched between two extractions of the same repo:
// Old=="" for an added file, New=="" for a deleted one, both set (equal
// unless renamed) otherwise. Produced by codebase.GitChangedFiles.
type FilePair struct {
	Old, New string
}

// CodeChangeKind is the flavor of a function/definition-level change
// DiffCode detected.
type CodeChangeKind string

const (
	CodeAdded   CodeChangeKind = "added"
	CodeRemoved CodeChangeKind = "removed"
	CodeChanged CodeChangeKind = "changed"
	CodeRenamed CodeChangeKind = "renamed"
	CodeMoved   CodeChangeKind = "moved"
)

// CodeChange is one function/definition-level change between two codebase
// extractions.
type CodeChange struct {
	Kind    CodeChangeKind
	Type    string // "function" or "definition"
	OldName string
	NewName string // == OldName unless Kind == renamed
	OldFile string // set for removed/renamed/moved
	NewFile string // set for added/changed/renamed/moved
	NewID   string // the entity's id in the NEW graph; "" for removed
}

// CodeDiff is the result of DiffCode: git-detected whole-file renames plus
// every function/definition-level change within the touched files.
type CodeDiff struct {
	FileRenames map[string]string // old path -> new path
	Changes     []CodeChange
}

func (d *CodeDiff) Empty() bool {
	return len(d.FileRenames) == 0 && len(d.Changes) == 0
}

type codeEntry struct {
	name, signature, doc, id string
}

func entriesFor(g *graph.Graph, file, nodeType string) []codeEntry {
	var out []codeEntry
	for id, n := range g.Nodes {
		if n.Type != nodeType || n.Attrs["file"] != file {
			continue
		}
		if n.Attrs["external"] == "true" || n.Attrs["generated"] == "true" {
			continue
		}
		out = append(out, codeEntry{name: n.Name, signature: n.Attrs["signature"], doc: n.Attrs["doc"], id: id})
	}
	return out
}

func sigModuloName(e codeEntry) string {
	return strings.Replace(e.signature, e.name, "\x00NAME\x00", 1)
}

func entityLabel(nodeType string) string {
	if nodeType == graph.NodeDefinition {
		return "definition"
	}
	return "function"
}

func findSignature(g *graph.Graph, file, name string) string {
	for _, n := range g.Nodes {
		if n.Type == graph.NodeFunction && n.Attrs["file"] == file && n.Name == name {
			return n.Attrs["signature"]
		}
	}
	return ""
}

// DiffCode compares old -> new across pairs, matching functions/definitions by
// (file, name) and detecting renames and cross-file moves by signature.
func DiffCode(old, new *graph.Graph, pairs []FilePair, renamed map[string]string) *CodeDiff {
	d := &CodeDiff{FileRenames: renamed}
	var leftoverRemoved, leftoverAdded []CodeChange

	for _, p := range pairs {
		for _, nodeType := range []string{graph.NodeFunction, graph.NodeDefinition} {
			var oldEntries, newEntries []codeEntry
			if p.Old != "" {
				oldEntries = entriesFor(old, p.Old, nodeType)
			}
			if p.New != "" {
				newEntries = entriesFor(new, p.New, nodeType)
			}
			matchedOld := map[int]bool{}
			matchedNew := map[int]bool{}

			for ni, ne := range newEntries {
				for oi, oe := range oldEntries {
					if matchedOld[oi] || oe.name != ne.name {
						continue
					}
					matchedOld[oi], matchedNew[ni] = true, true
					if oe.signature != ne.signature || oe.doc != ne.doc {
						d.Changes = append(d.Changes, CodeChange{
							Kind: CodeChanged, Type: entityLabel(nodeType),
							OldName: oe.name, NewName: ne.name,
							OldFile: p.Old, NewFile: p.New, NewID: ne.id,
						})
					}
					break
				}
			}

			if nodeType == graph.NodeFunction {
				for ni, ne := range newEntries {
					if matchedNew[ni] {
						continue
					}
					for oi, oe := range oldEntries {
						if matchedOld[oi] || sigModuloName(oe) != sigModuloName(ne) {
							continue
						}
						matchedOld[oi], matchedNew[ni] = true, true
						d.Changes = append(d.Changes, CodeChange{
							Kind: CodeRenamed, Type: entityLabel(nodeType),
							OldName: oe.name, NewName: ne.name,
							OldFile: p.Old, NewFile: p.New, NewID: ne.id,
						})
						break
					}
				}
			}

			for oi, oe := range oldEntries {
				if matchedOld[oi] {
					continue
				}
				ch := CodeChange{Kind: CodeRemoved, Type: entityLabel(nodeType), OldName: oe.name, OldFile: p.Old}
				if nodeType == graph.NodeFunction {
					leftoverRemoved = append(leftoverRemoved, ch)
				} else {
					d.Changes = append(d.Changes, ch)
				}
			}
			for ni, ne := range newEntries {
				if matchedNew[ni] {
					continue
				}
				ch := CodeChange{Kind: CodeAdded, Type: entityLabel(nodeType), NewName: ne.name, NewFile: p.New, NewID: ne.id}
				if nodeType == graph.NodeFunction {
					leftoverAdded = append(leftoverAdded, ch)
				} else {
					d.Changes = append(d.Changes, ch)
				}
			}
		}
	}

	sigCountOld, sigCountNew := map[string]int{}, map[string]int{}
	oldSig := func(ch CodeChange) string { return findSignature(old, ch.OldFile, ch.OldName) }
	newSig := func(ch CodeChange) string { return findSignature(new, ch.NewFile, ch.NewName) }
	for _, ch := range leftoverRemoved {
		sigCountOld[oldSig(ch)]++
	}
	for _, ch := range leftoverAdded {
		sigCountNew[newSig(ch)]++
	}
	usedOld, usedNew := map[int]bool{}, map[int]bool{}
	for ni, na := range leftoverAdded {
		sig := newSig(na)
		if sig == "" || sigCountNew[sig] != 1 {
			continue
		}
		for oi, oa := range leftoverRemoved {
			if usedOld[oi] || oldSig(oa) != sig || sigCountOld[sig] != 1 {
				continue
			}
			usedOld[oi], usedNew[ni] = true, true
			d.Changes = append(d.Changes, CodeChange{
				Kind: CodeMoved, Type: "function",
				OldName: oa.OldName, NewName: na.NewName,
				OldFile: oa.OldFile, NewFile: na.NewFile, NewID: na.NewID,
			})
			break
		}
	}
	for oi, oa := range leftoverRemoved {
		if !usedOld[oi] {
			d.Changes = append(d.Changes, oa)
		}
	}
	for ni, na := range leftoverAdded {
		if !usedNew[ni] {
			d.Changes = append(d.Changes, na)
		}
	}

	return d
}
