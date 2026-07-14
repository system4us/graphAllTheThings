package engine

import (
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

// Routes lists every HTTP route detected in the codebase (Express-style
// router/app.METHOD registrations — see internal/connector/codebase/routes.go),
// grouped by file: method, path, handler with file:line, and the middleware
// chain in order. fileSubstr filters to files whose path contains it ("" =
// all).
func (e *Engine) Routes(fileSubstr string) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("routes needs a codebase graph")
	}
	routes := e.G.NodesByType(graph.NodeRoute)
	if fileSubstr != "" {
		filtered := routes[:0]
		for _, n := range routes {
			if strings.Contains(n.Attrs["file"], fileSubstr) {
				filtered = append(filtered, n)
			}
		}
		routes = filtered
	}
	if len(routes) == 0 {
		return "no HTTP routes detected (Express-style JS/TS/JSX only, v1)\n", nil
	}

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Attrs["file"] != routes[j].Attrs["file"] {
			return routes[i].Attrs["file"] < routes[j].Attrs["file"]
		}
		var li, lj int
		fmt.Sscanf(routes[i].Attrs["line_start"], "%d", &li)
		fmt.Sscanf(routes[j].Attrs["line_start"], "%d", &lj)
		return li < lj
	})

	var b strings.Builder
	lastFile := ""
	for _, n := range routes {
		f := n.Attrs["file"]
		if f != lastFile {
			fmt.Fprintf(&b, "%s\n", f)
			lastFile = f
		}
		var handler string
		var middleware []string
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.From != n.ID {
				continue
			}
			on := e.G.Nodes[ed.To]
			if on == nil {
				continue
			}
			label := on.Name
			if loc := on.Attrs["file"]; loc != "" && on.Attrs["line_start"] != "" {
				label += fmt.Sprintf(" (%s:%s)", loc, on.Attrs["line_start"])
			}
			switch ed.Type {
			case graph.EdgeHandledBy:
				handler = label
			case graph.EdgeUsesMiddleware:
				middleware = append(middleware, label)
			}
		}
		line := fmt.Sprintf("  :%s  %s %s", n.Attrs["line_start"], n.Attrs["method"], n.Attrs["path"])
		if handler != "" {
			line += "  → " + handler
		}
		if len(middleware) > 0 {
			line += "  [middleware: " + strings.Join(middleware, ", ") + "]"
		}
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}
