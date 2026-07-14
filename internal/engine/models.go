package engine

import (
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

// Models lists every ORM model detected in the codebase (Sequelize-style
// Model.init / sequelize.define — see internal/connector/codebase/models.go):
// name, DB table, field count with renamed field→column mappings, and the
// association graph (hasMany/belongsTo/… with as/foreignKey), grouped by
// file. The codebase-side answer to "what does the data layer look like"
// when no live database is at hand. fileSubstr filters to files whose path
// contains it ("" = all).
func (e *Engine) Models(fileSubstr string) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("models needs a codebase graph")
	}
	models := e.G.NodesByType(graph.NodeModel)
	// Agent-tagged fallback, same as routes: any definition annotated with
	// model_table via annotate_entity counts as a model — the escape hatch
	// for ORMs/languages the static detectors don't recognize.
	for _, n := range e.G.NodesByType(graph.NodeDefinition) {
		if n.Attrs["model_table"] != "" {
			models = append(models, n)
		}
	}
	if fileSubstr != "" {
		filtered := models[:0]
		for _, n := range models {
			if strings.Contains(n.Attrs["file"], fileSubstr) {
				filtered = append(filtered, n)
			}
		}
		models = filtered
	}
	if len(models) == 0 {
		return "no ORM models detected (static: Sequelize init/define + associations, TypeORM decorators, Go DB struct tags, Django/SQLAlchemy classes; extend base classes via .gatt/models.json {\"base_classes\": [...]}; or tag a type via annotate_entity model_table=<table>)\n", nil
	}

	sort.Slice(models, func(i, j int) bool {
		if models[i].Attrs["file"] != models[j].Attrs["file"] {
			return models[i].Attrs["file"] < models[j].Attrs["file"]
		}
		return models[i].Name < models[j].Name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%d model(s)\n", len(models))
	lastFile := ""
	for _, n := range models {
		if f := n.Attrs["file"]; f != lastFile {
			fmt.Fprintf(&b, "\n%s\n", f)
			lastFile = f
		}
		line := "  " + n.Name
		table := n.Attrs["table"]
		if table == "" {
			table = n.Attrs["model_table"]
		}
		if table != "" {
			line += " (table " + table + ")"
		}
		if fc := n.Attrs["field_count"]; fc != "" {
			line += " " + fc + " fields"
		}
		if ls := n.Attrs["line_start"]; ls != "" {
			line += "  :" + ls
		}
		if n.Type != graph.NodeModel {
			line += "  [agent-tagged]"
		}
		b.WriteString(line + "\n")

		// Renamed field→column mappings only: the full field list is on the
		// node (describe_entity); renames are the part a SQL grep gets wrong.
		var renamed []string
		for _, f := range strings.Split(n.Attrs["fields"], ", ") {
			if strings.Contains(f, "→") {
				renamed = append(renamed, f)
			}
		}
		if len(renamed) > 0 {
			fmt.Fprintf(&b, "    columns renamed: %s\n", joinCapped(renamed, 12))
		}

		var out, in []string
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.Type != graph.EdgeReferences || ed.Attrs["kind"] == "" {
				continue
			}
			if ed.From == n.ID {
				on := e.G.Nodes[ed.To]
				if on == nil {
					continue
				}
				s := ed.Attrs["kind"] + " " + on.Name
				if as := ed.Attrs["as"]; as != "" {
					s += " (as " + as + ")"
				}
				if fk := ed.Attrs["foreign_key"]; fk != "" {
					s += " [fk " + fk + "]"
				}
				out = append(out, s)
			} else if on := e.G.Nodes[ed.From]; on != nil {
				in = append(in, on.Name+" "+ed.Attrs["kind"])
			}
		}
		sort.Strings(out)
		sort.Strings(in)
		if len(out) > 0 {
			fmt.Fprintf(&b, "    → %s\n", joinCapped(out, 10))
		}
		if len(in) > 0 {
			fmt.Fprintf(&b, "    ← %s\n", joinCapped(in, 10))
		}
	}
	return b.String(), nil
}
