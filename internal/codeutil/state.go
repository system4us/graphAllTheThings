package codeutil

import (
	"fmt"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// FileMtimes returns relPath -> mtime for every physical file node — the
// recorded disk state a codebase drift check compares against.
func FileMtimes(g *graph.Graph) map[string]string {
	m := map[string]string{}
	for _, n := range g.Nodes {
		if n.Type == graph.NodeFile && n.Attrs["path"] != "" {
			m[n.Name] = n.Attrs["mtime"]
		}
	}
	return m
}

// LoadCodebaseState reads just the source string and the file-node mtimes —
// the inputs of a codebase drift check — without materializing the whole graph.
func LoadCodebaseState(path string) (string, map[string]string, error) {
	meta, err := graph.LoadMeta(path)
	if err != nil {
		return "", nil, err
	}
	source, ok := meta["source"]
	if !ok {
		return "", nil, fmt.Errorf("graph %s: missing source metadata", path)
	}
	nodes, err := graph.QueryNodesByType(path, graph.NodeFile)
	if err != nil {
		return "", nil, err
	}
	m := map[string]string{}
	for _, n := range nodes {
		if n.Attrs["path"] != "" {
			m[n.Name] = n.Attrs["mtime"]
		}
	}
	return source, m, nil
}
