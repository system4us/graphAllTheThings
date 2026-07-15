package engine

import (
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

// Routes lists every HTTP route detected in the codebase: statically-detected
// Express-style router/app.METHOD registrations (see
// internal/connector/codebase/routes.go), plus any function annotated with
// route_method/route_path/route_framework via annotate_entity — the way to
// cover routes in other languages/frameworks/styles the static detector
// doesn't recognize. Grouped by file: method, path, handler with file:line,
// and the middleware chain in order (agent-tagged entries have no middleware
// chain). fileSubstr filters to files whose path contains it ("" = all).
func (e *Engine) Routes(fileSubstr string) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("routes needs a codebase graph")
	}
	routes := e.G.NodesByType(graph.NodeRoute)
	for _, n := range e.G.NodesByType(graph.NodeFunction) {
		if n.Attrs["route_method"] != "" {
			routes = append(routes, n)
		}
	}
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
		return "no HTTP routes detected (static: Express JS/TS/JSX, Spring/ASP.NET annotations, gin/chi/echo/gorilla/net-http Go, Flask/FastAPI decorators; tag others via annotate_entity's route_method/route_path/route_framework)\n", nil
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
		if n.Type == graph.NodeFunction {
			line := fmt.Sprintf("  :%s  %s %s  → %s (%s:%s)  [agent", n.Attrs["line_start"],
				n.Attrs["route_method"], n.Attrs["route_path"], n.Name, n.Attrs["file"], n.Attrs["line_start"])
			if fw := n.Attrs["route_framework"]; fw != "" {
				line += ": " + fw
			}
			b.WriteString(line + "]\n")
			continue
		}

		var handler string
		var middleware, models, callers []string
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.Type == graph.EdgeCallsEndpoint && ed.To == n.ID {
				if on := e.G.Nodes[ed.From]; on != nil {
					label := on.Name
					if f := on.Attrs["file"]; f != "" {
						label += " (" + f
						if l := ed.Attrs["line"]; l != "" {
							label += ":" + l
						}
						label += ")"
					}
					callers = append(callers, label)
				}
				continue
			}
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
			case graph.EdgeUsesModel:
				models = append(models, on.Name)
			}
		}
		method := n.Attrs["method"]
		if method == "" {
			method = "ANY" // wildcard registration (net/http HandleFunc)
		}
		line := fmt.Sprintf("  :%s  %s %s", n.Attrs["line_start"], method, n.Attrs["path"])
		if handler != "" {
			line += "  → " + handler
		}
		if len(middleware) > 0 {
			line += "  [middleware: " + strings.Join(middleware, ", ") + "]"
		}
		b.WriteString(line + "\n")
		if len(models) > 0 {
			sort.Strings(models)
			fmt.Fprintf(&b, "      models: %s\n", joinCapped(models, 10))
		}
		if len(callers) > 0 {
			sort.Strings(callers)
			fmt.Fprintf(&b, "      called from: %s\n", joinCapped(callers, 6))
		}
	}
	return b.String(), nil
}
