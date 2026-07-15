package codebase

// ORM model detection, the data-layer mirror of routes.go: Sequelize-style
// `Model.init({fields}, {tableName})` / `sequelize.define('Name', {fields})`
// calls become model nodes (with the field → column mapping the ORM hides
// from SQL greps), and `A.hasMany(B)` / `A.belongsTo(B)` / … calls become
// REFERENCES edges — so join questions work on a codebase graph the same way
// they do on a database graph. Scoped to JS/TS/JSX, same as routes: other
// ORMs are a different grammar shape each; tag those models by hand via
// annotate_entity until they grow their own detector.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

// assocKinds maps the Sequelize association methods to the edge attr value.
var assocKinds = map[string]bool{
	"hasMany": true, "hasOne": true, "belongsTo": true, "belongsToMany": true,
}

// defaultModelBases are inheritance markers that identify a class as an ORM
// model across frameworks. "Strong" bases qualify on their own; "weak" ones
// (generic names like Base that non-ORM classes also extend) additionally
// need field evidence in the class body. Extend per repo via
// .gatt/models.json: {"base_classes": ["MyOrmBase", ...]} (all strong).
var defaultModelBases = map[string]bool{
	// strong
	"models.Model": true, "db.Model": true, "DeclarativeBase": true, // Django / Flask-SQLAlchemy / SQLAlchemy 2
	"BaseEntity": true, "ApplicationRecord": true, // TypeORM active-record / Rails-style
	"Document": true, "EmbeddedDocument": true, // mongoengine
}
var weakModelBases = map[string]bool{
	"Base": true, "Model": true, // SQLAlchemy declarative_base() / Sequelize, Backbone, anything
}

// loadModelBases merges .gatt/models.json base_classes into the defaults.
func (c *Connector) loadModelBases() {
	c.modelBases = map[string]bool{}
	for k := range defaultModelBases {
		c.modelBases[k] = true
	}
	data, err := os.ReadFile(filepath.Join(c.dir, ".gatt", "models.json"))
	if err != nil {
		return
	}
	var payload struct {
		BaseClasses []string `json:"base_classes"`
	}
	if json.Unmarshal(data, &payload) == nil {
		for _, b := range payload.BaseClasses {
			c.modelBases[b] = true
		}
	}
}

// baseTokens splits an inheritance clause ("extends Model", "(models.Model,
// Generic[T])") into candidate base names, keeping dotted paths intact.
func baseTokens(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if t != "extends" && t != "implements" && t != "" {
			out = append(out, t)
		}
	}
	return out
}

// modelAssoc is one detected association call, resolved against the model
// registry by wireModels after the whole tree is parsed (two-pass, same as
// CALLS/routes: declaration order and file boundaries don't matter).
type modelAssoc struct {
	fromName, toName string
	kind             string
	as, foreignKey   string
	file             string
	line             int
}

// detectModelInit handles `<Model>.init({fields}, {options})`. The receiver
// must be capitalized — Sequelize models are PascalCase classes; this filters
// out `app.init(...)`-style calls that share the shape.
func (c *Connector) detectModelInit(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	if caps["model.method"].Content(data) != "init" {
		return
	}
	name := caps["model.obj"].Content(data)
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return
	}
	callNode := caps["model.call"]
	table := ""
	if argsNode := callNode.ChildByFieldName("arguments"); argsNode != nil && argsNode.NamedChildCount() > 1 {
		if opts := argsNode.NamedChild(1); opts != nil && opts.Type() == "object" {
			table = objectStringValue(opts, data, "tableName")
		}
	}
	c.emitModel(g, name, table, caps["model.fields"], relPath, fileID, data, int(callNode.StartPoint().Row)+1)
}

// detectModelDefine handles `<anything>.define('Name', {fields}, …)`.
func (c *Connector) detectModelDefine(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	if caps["modeldef.method"].Content(data) != "define" {
		return
	}
	name := strings.Trim(caps["modeldef.name"].Content(data), "\"'`")
	if name == "" {
		return
	}
	callNode := caps["modeldef.call"]
	table := ""
	if argsNode := callNode.ChildByFieldName("arguments"); argsNode != nil && argsNode.NamedChildCount() > 2 {
		if opts := argsNode.NamedChild(2); opts != nil && opts.Type() == "object" {
			table = objectStringValue(opts, data, "tableName")
		}
	}
	c.emitModel(g, name, table, caps["modeldef.fields"], relPath, fileID, data, int(callNode.StartPoint().Row)+1)
}

func (c *Connector) emitModel(g *graph.Graph, name, table string, fieldsNode *sitter.Node, relPath, fileID string, data []byte, line int) {
	c.emitModelFields(g, name, table, fieldColumnPairs(fieldsNode, data), relPath, fileID, line)
}

// emitModelFields materializes a model node. Two detectors can hit the same
// class (a Sequelize `class X extends Model` plus its `X.init(...)`), so an
// existing node is merged: non-empty table/fields win over empty ones.
func (c *Connector) emitModelFields(g *graph.Graph, name, table string, fields []string, relPath, fileID string, line int) {
	id := "model:" + relPath + ":" + name
	if prev := g.Nodes[id]; prev != nil {
		if table == "" {
			table = prev.Attrs["table"]
		}
		if len(fields) == 0 && prev.Attrs["fields"] != "" {
			fields = strings.Split(prev.Attrs["fields"], ", ")
		}
	}
	attrs := map[string]string{
		"file":       relPath,
		"line_start": fmt.Sprint(line),
	}
	if table != "" {
		attrs["table"] = table
	}
	if len(fields) > 0 {
		attrs["field_count"] = fmt.Sprint(len(fields))
		if len(fields) > 50 {
			fields = fields[:50]
		}
		attrs["fields"] = strings.Join(fields, ", ")
	}
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeModel, Name: name, Attrs: attrs})
	g.AddEdge(id, fileID, graph.EdgeBelongsTo, nil)
}

// detectJavaModel handles JPA/Hibernate-style entities: `@Entity` (a bare
// marker annotation — the primary, sufficient-on-its-own signal) and
// `@Table(name = "...")` (table name override) on a class_declaration.
// Every non-transient field counts as a column: JPA persists fields by
// default and @Column is only needed to customize name/constraints, so
// requiring an explicit decorator per field — the way the TS/Sequelize
// detector does, where every field-as-column needs its own @Column — would
// silently miss most real entities' fields, which skip @Column entirely
// when the column name already matches the field name.
func (c *Connector) detectJavaModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	annName := caps["jmodel.ann"]
	nameNode := caps["jmodel.name"]
	if annName == nil || nameNode == nil {
		return
	}
	ann := annName.Content(data)
	if ann != "Entity" && ann != "Table" {
		return
	}
	name := nameNode.Content(data)

	table := ""
	if ann == "Table" {
		if argsNode := caps["jmodel.annargs"]; argsNode != nil {
			table = javaAnnotationStringArg(argsNode, data, "name")
		}
	}

	var fields []string
	if body := caps["jmodel.body"]; body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			f := body.NamedChild(i)
			if f.Type() != "field_declaration" {
				continue
			}
			transient, colName := false, ""
			for j := 0; j < int(f.NamedChildCount()); j++ {
				mods := f.NamedChild(j)
				if mods.Type() != "modifiers" {
					continue
				}
				for k := 0; k < int(mods.NamedChildCount()); k++ {
					a := mods.NamedChild(k)
					var aname string
					var argsN *sitter.Node
					switch a.Type() {
					case "marker_annotation":
						if id := a.ChildByFieldName("name"); id != nil {
							aname = id.Content(data)
						}
					case "annotation":
						if id := a.ChildByFieldName("name"); id != nil {
							aname = id.Content(data)
						}
						argsN = a.ChildByFieldName("arguments")
					}
					if aname == "Transient" {
						transient = true
					}
					if aname == "Column" && argsN != nil {
						if n := javaAnnotationStringArg(argsN, data, "name"); n != "" {
							colName = n
						}
					}
				}
			}
			if transient {
				continue
			}
			declarator := f.ChildByFieldName("declarator")
			if declarator == nil {
				continue
			}
			fnameNode := declarator.ChildByFieldName("name")
			if fnameNode == nil {
				continue
			}
			fname := fnameNode.Content(data)
			entry := fname
			if colName != "" && colName != fname {
				entry += "→" + colName
			}
			fields = append(fields, entry)
		}
	}
	c.emitModelFields(g, name, table, fields, relPath, fileID, int(nameNode.StartPoint().Row)+1)
}

// javaAnnotationStringArg finds `key = "value"` inside a Java
// annotation_argument_list (element_value_pair nodes) — e.g. the "owners"
// in `@Table(name = "owners")`.
func javaAnnotationStringArg(argsNode *sitter.Node, data []byte, key string) string {
	for i := 0; i < int(argsNode.NamedChildCount()); i++ {
		pair := argsNode.NamedChild(i)
		if pair.Type() != "element_value_pair" {
			continue
		}
		k, v := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
		if k != nil && v != nil && k.Content(data) == key && v.Type() == "string_literal" {
			return strings.Trim(v.Content(data), "\"")
		}
	}
	return ""
}

// detectKotlinModel is detectJavaModel's Kotlin counterpart — same JPA
// annotations (@Entity/@Table/@Column), different grammar shape entirely.
// Kotlin's tree-sitter grammar exposes no field names on these nodes (the
// existing kann.* route captures already navigate it positionally, same
// convention followed here): a no-args annotation like `@Entity` wraps a
// bare user_type, while an argument-bearing one like `@Table(name = "x")`
// wraps a constructor_invocation instead — structurally distinct node
// shapes for the same @Annotation syntax, so both need their own query
// pattern same as Java's marker_annotation/annotation split.
func (c *Connector) detectKotlinModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	annName := caps["kmodel.ann"]
	nameNode := caps["kmodel.name"]
	if annName == nil || nameNode == nil {
		return
	}
	ann := annName.Content(data)
	if ann != "Entity" && ann != "Table" {
		return
	}
	name := nameNode.Content(data)

	table := ""
	if ann == "Table" {
		if argsNode := caps["kmodel.annargs"]; argsNode != nil {
			table = kotlinAnnotationStringArg(argsNode, data, "name")
		}
	}

	var fields []string
	if body := caps["kmodel.body"]; body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			prop := body.NamedChild(i)
			if prop.Type() != "property_declaration" {
				continue
			}
			transient, colName := false, ""
			var varDecl *sitter.Node
			for j := 0; j < int(prop.NamedChildCount()); j++ {
				switch ch := prop.NamedChild(j); ch.Type() {
				case "modifiers":
					for k := 0; k < int(ch.NamedChildCount()); k++ {
						a := ch.NamedChild(k)
						if a.Type() != "annotation" {
							continue
						}
						aname, argsN := kotlinAnnotationNameArgs(a, data)
						if aname == "Transient" {
							transient = true
						}
						if aname == "Column" && argsN != nil {
							if n := kotlinAnnotationStringArg(argsN, data, "name"); n != "" {
								colName = n
							}
						}
					}
				case "variable_declaration":
					varDecl = ch
				}
			}
			if transient || varDecl == nil {
				continue
			}
			var fname string
			for j := 0; j < int(varDecl.NamedChildCount()); j++ {
				if id := varDecl.NamedChild(j); id.Type() == "simple_identifier" {
					fname = id.Content(data)
					break
				}
			}
			if fname == "" {
				continue
			}
			entry := fname
			if colName != "" && colName != fname {
				entry += "→" + colName
			}
			fields = append(fields, entry)
		}
	}
	c.emitModelFields(g, name, table, fields, relPath, fileID, int(nameNode.StartPoint().Row)+1)
}

// kotlinAnnotationNameArgs extracts an annotation's bare type name and its
// value_arguments node (nil for a marker-style annotation with no parens):
// @Entity -> (annotation (user_type (type_identifier))); @Table(...) ->
// (annotation (constructor_invocation (user_type (type_identifier))
// (value_arguments ...))).
func kotlinAnnotationNameArgs(ann *sitter.Node, data []byte) (string, *sitter.Node) {
	if ann.NamedChildCount() == 0 {
		return "", nil
	}
	child := ann.NamedChild(0)
	if child.Type() == "constructor_invocation" {
		var name string
		var args *sitter.Node
		for i := 0; i < int(child.NamedChildCount()); i++ {
			switch c := child.NamedChild(i); c.Type() {
			case "user_type":
				if ti := c.NamedChild(0); ti != nil && ti.Type() == "type_identifier" {
					name = ti.Content(data)
				}
			case "value_arguments":
				args = c
			}
		}
		return name, args
	}
	if child.Type() == "user_type" {
		if ti := child.NamedChild(0); ti != nil && ti.Type() == "type_identifier" {
			return ti.Content(data), nil
		}
	}
	return "", nil
}

// kotlinAnnotationStringArg finds `key = "value"` inside a Kotlin
// value_arguments list (value_argument nodes: name then value, no field
// labels in this grammar).
func kotlinAnnotationStringArg(argsNode *sitter.Node, data []byte, key string) string {
	for i := 0; i < int(argsNode.NamedChildCount()); i++ {
		arg := argsNode.NamedChild(i)
		if arg.Type() != "value_argument" || arg.NamedChildCount() < 2 {
			continue
		}
		k, v := arg.NamedChild(0), arg.NamedChild(1)
		if k.Type() == "simple_identifier" && k.Content(data) == key && v.Type() == "string_literal" {
			return strings.Trim(v.Content(data), "\"")
		}
	}
	return ""
}

// detectCSharpModel handles two independent EF Core signals: a class whose
// base list matches a configured model base (weak defaults "Base"/"Model",
// or a project-specific name like "BaseEntity" via .gatt/models.json — C#
// projects that map entities by pure convention, no attributes anywhere,
// have no *other* signal available at all, so this is the only path in for
// them), and DataAnnotations attributes ([Table("...")]/[Column("...")]),
// which — unlike a project's own base-class name — are a universal,
// safe-to-auto-detect signal the same way JPA's @Entity is.
// [NotMapped] excludes a field the same way JPA's @Transient does.
func (c *Connector) detectCSharpModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	if c.modelBases == nil {
		c.loadModelBases()
	}
	nameNode := caps["csmodel.name"]
	if nameNode == nil {
		return
	}
	name := nameNode.Content(data)

	strongBase, weakBase := false, false
	if basesNode := caps["csmodel.bases"]; basesNode != nil {
		for _, tok := range baseTokens(basesNode.Content(data)) {
			if c.modelBases[tok] {
				strongBase = true
			} else if weakModelBases[tok] {
				weakBase = true
			}
		}
	}

	table := ""
	if annNode := caps["csmodel.ann"]; annNode != nil {
		switch ann := annNode.Content(data); ann {
		case "Table":
			strongBase = true
			if argsNode := caps["csmodel.annargs"]; argsNode != nil {
				table = csharpAttributeFirstString(argsNode, data)
			}
		case "Column":
			strongBase = true
		}
	}
	if !strongBase && !weakBase {
		return
	}

	var fields []string
	if body := caps["csmodel.body"]; body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			prop := body.NamedChild(i)
			if prop.Type() != "property_declaration" {
				continue
			}
			propNameNode := prop.ChildByFieldName("name")
			if propNameNode == nil {
				continue
			}
			transient, colName := false, ""
			for j := 0; j < int(prop.NamedChildCount()); j++ {
				al := prop.NamedChild(j)
				if al.Type() != "attribute_list" {
					continue
				}
				for k := 0; k < int(al.NamedChildCount()); k++ {
					a := al.NamedChild(k)
					if a.Type() != "attribute" {
						continue
					}
					anameNode := a.ChildByFieldName("name")
					if anameNode == nil {
						continue
					}
					switch aname := anameNode.Content(data); aname {
					case "NotMapped":
						transient = true
					case "Column":
						if a.NamedChildCount() > 1 {
							if s := csharpAttributeFirstString(a.NamedChild(1), data); s != "" {
								colName = s
							}
						}
					}
				}
			}
			if transient {
				continue
			}
			fname := propNameNode.Content(data)
			entry := fname
			if colName != "" && colName != fname {
				entry += "→" + colName
			}
			fields = append(fields, entry)
		}
	}
	if !strongBase && table == "" && len(fields) == 0 {
		return // weak base with no ORM evidence: not a model
	}
	c.emitModelFields(g, name, table, fields, relPath, fileID, int(nameNode.StartPoint().Row)+1)
}

// csharpAttributeFirstString returns the first positional string-literal
// argument of a C# attribute_argument_list — e.g. "Products" in
// [Table("Products")]. attribute_argument has no named "value" field for
// its inner literal in this grammar, hence positional (NamedChild(0)).
func csharpAttributeFirstString(argsNode *sitter.Node, data []byte) string {
	for i := 0; i < int(argsNode.NamedChildCount()); i++ {
		arg := argsNode.NamedChild(i)
		if arg.Type() != "attribute_argument" || arg.NamedChildCount() == 0 {
			continue
		}
		if v := arg.NamedChild(0); v.Type() == "string_literal" {
			return strings.Trim(v.Content(data), "\"")
		}
	}
	return ""
}

// detectClassModel handles TS/JS class declarations: TypeORM-style decorated
// classes (@Entity/@Table on the class, @Column({name}) on fields) and any
// class extending a configured base. A weak base (Model, Base) alone is not
// enough — Backbone views extend Model too — unless decorators or the
// .gatt/models.json overlay vouch for it.
func (c *Connector) detectClassModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	if c.modelBases == nil {
		c.loadModelBases()
	}
	classNode := caps["clsmodel.class"]
	name := caps["clsmodel.name"].Content(data)
	if name == "" {
		return
	}

	// Class decorators hang off the class node, or off the surrounding
	// export_statement when written above the export keyword.
	decoratorHosts := []*sitter.Node{classNode}
	if p := classNode.Parent(); p != nil && p.Type() == "export_statement" {
		decoratorHosts = append(decoratorHosts, p)
	}
	table := ""
	decorated := false
	for _, host := range decoratorHosts {
		for i := 0; i < int(host.NamedChildCount()); i++ {
			ch := host.NamedChild(i)
			if ch.Type() != "decorator" {
				continue
			}
			dec := ch.Content(data)
			switch {
			case strings.HasPrefix(dec, "@Entity"), strings.HasPrefix(dec, "@Table"):
				decorated = true
				table = decoratorTable(ch, data)
			}
		}
	}

	strongBase := false
	for _, tok := range baseTokens(caps["clsmodel.heritage"].Content(data)) {
		if c.modelBases[tok] {
			strongBase = true
			break
		}
	}
	if !decorated && !strongBase {
		return
	}

	var fields []string
	if body := classNode.ChildByFieldName("body"); body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			f := body.NamedChild(i)
			if f.Type() != "public_field_definition" {
				continue
			}
			nameNode := f.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			entry := nameNode.Content(data)
			for j := 0; j < int(f.NamedChildCount()); j++ {
				d := f.NamedChild(j)
				if d.Type() == "decorator" && strings.Contains(d.Content(data), "Column") {
					if col := decoratorTable(d, data); col != "" && col != entry {
						entry += "→" + col
					}
				}
			}
			fields = append(fields, entry)
		}
	}
	sort.Strings(fields)
	c.emitModelFields(g, name, table, fields, relPath, fileID, int(classNode.StartPoint().Row)+1)
}

// decoratorTable extracts the table/column name from a decorator call:
// @Entity('users'), @Table({tableName: 'users'}), @Column({name: 'first_name'}).
func decoratorTable(dec *sitter.Node, data []byte) string {
	call := dec.NamedChild(0)
	if call == nil || call.Type() != "call_expression" {
		return ""
	}
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return ""
	}
	first := args.NamedChild(0)
	switch first.Type() {
	case "string":
		return strings.Trim(first.Content(data), "\"'`")
	case "object":
		return objectStringValue(first, data, "tableName", "name")
	}
	return ""
}

// detectPyModel handles Python ORM classes: Django (models.Model), SQLAlchemy
// (declarative Base / DeclarativeBase, __tablename__, Column(...)), Flask
// (db.Model), mongoengine, SQLModel (table=True). Weak bases need field
// evidence in the body.
func (c *Connector) detectPyModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	if c.modelBases == nil {
		c.loadModelBases()
	}
	name := caps["pymodel.name"].Content(data)
	if name == "" {
		return
	}
	basesNode := caps["pymodel.bases"]
	strongBase, weakBase := false, false
	for _, tok := range baseTokens(basesNode.Content(data)) {
		if c.modelBases[tok] {
			strongBase = true
		} else if weakModelBases[tok] {
			weakBase = true
		}
	}
	// SQLModel: `class User(UserBase, table=True):` — table=True is a
	// self-sufficient signal independent of the other base name(s).
	// UserBase-style intermediates (shared Pydantic field mixins) are
	// almost never in modelBases themselves — they're same-file classes
	// extending bare SQLModel, not a name this connector's base-class list
	// could enumerate — so relying on base-name matching alone misses
	// every SQLModel table. baseTokens' identifier-only split already
	// drops the `=True` half of the keyword argument, so this needs the
	// actual argument_list node, not the flattened token string.
	for i := range int(basesNode.NamedChildCount()) {
		kw := basesNode.NamedChild(i)
		if kw.Type() != "keyword_argument" {
			continue
		}
		kwName, kwVal := kw.ChildByFieldName("name"), kw.ChildByFieldName("value")
		if kwName != nil && kwName.Content(data) == "table" && kwVal != nil && kwVal.Content(data) == "True" {
			strongBase = true
		}
	}
	if !strongBase && !weakBase {
		return
	}

	table := ""
	var fields []string
	body := caps["pymodel.body"]
	for i := 0; i < int(body.NamedChildCount()); i++ {
		stmt := body.NamedChild(i)
		// class Meta: db_table = '...' (Django)
		if stmt.Type() == "class_definition" {
			if mn := stmt.ChildByFieldName("name"); mn != nil && mn.Content(data) == "Meta" {
				if mb := stmt.ChildByFieldName("body"); mb != nil {
					if t := pyAssignedString(mb, data, "db_table"); t != "" {
						table = t
					}
				}
			}
			continue
		}
		if stmt.Type() != "expression_statement" || stmt.NamedChildCount() == 0 {
			continue
		}
		assign := stmt.NamedChild(0)
		if assign.Type() != "assignment" {
			continue
		}
		left, right := assign.ChildByFieldName("left"), assign.ChildByFieldName("right")
		if left == nil || left.Type() != "identifier" {
			continue
		}
		fname := left.Content(data)
		if fname == "__tablename__" {
			if right != nil {
				table = strings.Trim(right.Content(data), "\"'")
			}
			continue
		}
		if right == nil || right.Type() != "call" {
			// Bare `name: Type` (no value) or `name: Type = <plain
			// default>` (no Column/Field/Relationship call) — SQLModel/
			// dataclass-style, every annotated class attribute is a real
			// column whether or not it carries a call. Only trusted once
			// the class already has a strong, independent signal it's a
			// table (table=True, or a known strong base) — for a weak
			// base this alone would false-positive on plain typed
			// attributes that were never meant as schema.
			if strongBase {
				fields = append(fields, fname)
			}
			continue
		}
		fn := right.ChildByFieldName("function")
		if fn == nil {
			continue
		}
		fnName := fn.Content(data)
		short := fnName[strings.LastIndex(fnName, ".")+1:]
		// SQLModel spells its own field/relationship helpers capitalized
		// (Field, Relationship); SQLAlchemy's are lowercase (Column,
		// relationship). EqualFold covers both without a second literal set.
		if !strings.EqualFold(short, "Column") && !strings.EqualFold(short, "relationship") && !strings.HasSuffix(short, "Field") {
			continue
		}
		entry := fname
		if col := pyColumnName(right, data); col != "" && col != fname {
			entry += "→" + col
		}
		fields = append(fields, entry)
	}
	if !strongBase && table == "" && len(fields) == 0 {
		return // weak base with no ORM evidence: not a model
	}
	sort.Strings(fields)
	c.emitModelFields(g, name, table, fields, relPath, fileID, int(caps["pymodel.class"].StartPoint().Row)+1)
}

// pyAssignedString finds `key = 'value'` inside a block.
func pyAssignedString(block *sitter.Node, data []byte, key string) string {
	for i := 0; i < int(block.NamedChildCount()); i++ {
		stmt := block.NamedChild(i)
		if stmt.Type() != "expression_statement" || stmt.NamedChildCount() == 0 {
			continue
		}
		a := stmt.NamedChild(0)
		if a.Type() != "assignment" {
			continue
		}
		if l := a.ChildByFieldName("left"); l != nil && l.Content(data) == key {
			if r := a.ChildByFieldName("right"); r != nil {
				return strings.Trim(r.Content(data), "\"'")
			}
		}
	}
	return ""
}

// pyColumnName extracts the DB column from a field call: Column('col', …)
// positional, or name=/db_column= keyword (SQLAlchemy / Django).
func pyColumnName(call *sitter.Node, data []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		switch a.Type() {
		case "string":
			if i == 0 {
				return strings.Trim(a.Content(data), "\"'")
			}
		case "keyword_argument":
			if n := a.ChildByFieldName("name"); n != nil {
				if k := n.Content(data); k == "db_column" || k == "name" {
					if v := a.ChildByFieldName("value"); v != nil && v.Type() == "string" {
						return strings.Trim(v.Content(data), "\"'")
					}
				}
			}
		}
	}
	return ""
}

// detectGoModel handles Go structs with DB struct tags (gorm/db/bun/xorm) or
// an embedded gorm.Model. Structs without any DB tag are not models.
func (c *Connector) detectGoModel(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte) {
	name := caps["gomodel.name"].Content(data)
	structNode := caps["gomodel.struct"]
	if name == "" || structNode.NamedChildCount() == 0 {
		return
	}
	list := structNode.NamedChild(0)
	if list == nil || list.Type() != "field_declaration_list" {
		return
	}
	var fields []string
	qualified := false
	for i := 0; i < int(list.NamedChildCount()); i++ {
		f := list.NamedChild(i)
		if f.Type() != "field_declaration" {
			continue
		}
		if f.ChildByFieldName("name") == nil {
			// embedded field: gorm.Model qualifies the struct by itself
			if t := f.ChildByFieldName("type"); t != nil && t.Content(data) == "gorm.Model" {
				qualified = true
			}
			continue
		}
		fname := f.ChildByFieldName("name").Content(data)
		entry := fname
		if tagNode := f.ChildByFieldName("tag"); tagNode != nil {
			tag := strings.Trim(tagNode.Content(data), "`")
			if hasDBTag(tag) {
				qualified = true
				if col := goTagColumn(tag); col != "" && col != fname {
					entry += "→" + col
				}
			}
		}
		fields = append(fields, entry)
	}
	if !qualified {
		return
	}
	sort.Strings(fields)
	c.emitModelFields(g, name, "", fields, relPath, fileID, int(structNode.StartPoint().Row)+1)
}

// hasDBTag reports whether a struct tag carries any known DB/ORM key.
func hasDBTag(tag string) bool {
	for _, key := range []string{`gorm:"`, `db:"`, `bun:"`, `xorm:"`} {
		if strings.Contains(tag, key) {
			return true
		}
	}
	return false
}

// goTagColumn extracts a column name from a struct tag: gorm:"column:x;…",
// db:"x", bun:"x,…", xorm:"'x'".
func goTagColumn(tag string) string {
	for _, key := range []string{"gorm", "db", "bun", "xorm"} {
		marker := key + `:"`
		idx := strings.Index(tag, marker)
		if idx < 0 {
			continue
		}
		val := tag[idx+len(marker):]
		if end := strings.IndexByte(val, '"'); end >= 0 {
			val = val[:end]
		}
		if key == "gorm" {
			for _, part := range strings.Split(val, ";") {
				if strings.HasPrefix(part, "column:") {
					return strings.TrimPrefix(part, "column:")
				}
			}
			continue // gorm tag without column: still a DB tag, but unnamed
		}
		col := val
		if comma := strings.IndexByte(col, ','); comma >= 0 {
			col = col[:comma]
		}
		col = strings.Trim(col, "'")
		if col != "" && col != "-" {
			return col
		}
	}
	return ""
}

// detectAssoc handles `<Model>.<hasMany|hasOne|belongsTo|belongsToMany>(<Target>, {opts})`.
// Both sides must be capitalized identifiers; resolution to actual model
// nodes happens in wireModels, so a call on a non-model never makes an edge.
func (c *Connector) detectAssoc(caps map[string]*sitter.Node, relPath string, data []byte) {
	kind := caps["assoc.method"].Content(data)
	if !assocKinds[kind] {
		return
	}
	from := caps["assoc.obj"].Content(data)
	to := caps["assoc.target"].Content(data)
	if from == "" || to == "" || from[0] < 'A' || from[0] > 'Z' || to[0] < 'A' || to[0] > 'Z' {
		return
	}
	a := modelAssoc{fromName: from, toName: to, kind: kind, file: relPath,
		line: int(caps["assoc.call"].StartPoint().Row) + 1}
	if argsNode := caps["assoc.call"].ChildByFieldName("arguments"); argsNode != nil && argsNode.NamedChildCount() > 1 {
		if opts := argsNode.NamedChild(1); opts != nil && opts.Type() == "object" {
			a.as = objectStringValue(opts, data, "as")
			a.foreignKey = objectStringValue(opts, data, "foreignKey")
		}
	}
	c.pendingAssocs = append(c.pendingAssocs, a)
}

// wireModels resolves pending associations against every model node in the
// graph (survivors plus newly parsed — an association declared in a central
// setupAssociations file must resolve models defined anywhere). Ambiguous
// names (same model name in several files) are skipped, same rule as CALLS.
func (c *Connector) wireModels(g *graph.Graph) {
	byName := map[string][]string{}
	for id, n := range g.Nodes {
		if n.Type == graph.NodeModel {
			byName[n.Name] = append(byName[n.Name], id)
		}
	}
	if len(byName) == 0 {
		c.pendingAssocs = nil
		return
	}
	// Seed dedupe with edges already in the graph: on incremental refresh a
	// dirty declaring file re-emits its associations while the surviving
	// endpoints kept the old edges. (The flip side: an association *removed*
	// from source lingers until the next full extract, like CO_CHANGED.)
	seen := map[string]bool{}
	pairSeen := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeReferences && e.Attrs["kind"] != "" {
			seen[e.From+"\x00"+e.To+"\x00"+e.Attrs["kind"]+"\x00"+e.Attrs["as"]] = true
			pairSeen[e.From+"\x00"+e.To] = true
			pairSeen[e.To+"\x00"+e.From] = true
		}
	}
	for _, a := range c.pendingAssocs {
		froms, tos := byName[a.fromName], byName[a.toName]
		if len(froms) != 1 || len(tos) != 1 {
			continue
		}
		key := froms[0] + "\x00" + tos[0] + "\x00" + a.kind + "\x00" + a.as
		if seen[key] {
			continue
		}
		seen[key] = true
		attrs := map[string]string{"kind": a.kind, "declared_in": a.file, "line": fmt.Sprint(a.line)}
		if a.as != "" {
			attrs["as"] = a.as
		}
		if a.foreignKey != "" {
			attrs["foreign_key"] = a.foreignKey
		}
		g.AddEdge(froms[0], tos[0], graph.EdgeReferences, attrs)
		pairSeen[froms[0]+"\x00"+tos[0]] = true
		pairSeen[tos[0]+"\x00"+froms[0]] = true
	}
	c.pendingAssocs = nil

	// Foreign-key name inference, the language/framework-agnostic fallback:
	// a field named productId / product_id on model A pointing at an existing
	// model Product becomes A —REFERENCES→ Product. Fields come from whatever
	// detector built the model (Sequelize object, Go struct tags, Python
	// class body, TS decorators), so this works uniformly. Explicit
	// associations between the same pair always win; inferred edges are
	// tagged inferred=true so consumers can weigh them accordingly.
	for id, n := range g.Nodes {
		if n.Type != graph.NodeModel || n.Attrs["fields"] == "" {
			continue
		}
		for _, f := range strings.Split(n.Attrs["fields"], ", ") {
			fname, _, _ := strings.Cut(f, "→")
			base := ""
			switch {
			case len(fname) > 2 && strings.HasSuffix(fname, "Id"):
				base = fname[:len(fname)-2]
			case len(fname) > 3 && strings.HasSuffix(fname, "_id"):
				base = fname[:len(fname)-3]
			case len(fname) > 2 && strings.HasSuffix(fname, "ID"): // Go style: ProductID
				base = fname[:len(fname)-2]
			default:
				continue
			}
			cand := pascalCase(base)
			targets := byName[cand]
			if len(targets) != 1 || targets[0] == id || pairSeen[id+"\x00"+targets[0]] {
				continue
			}
			g.AddEdge(id, targets[0], graph.EdgeReferences, map[string]string{
				"kind": "foreignKey", "foreign_key": fname, "inferred": "true",
				"declared_in": n.Attrs["file"],
			})
			pairSeen[id+"\x00"+targets[0]] = true
			pairSeen[targets[0]+"\x00"+id] = true
		}
	}
}

// pascalCase maps a foreign-key base name to a model-name candidate:
// product→Product, stock_movement→StockMovement, stockMovement→StockMovement.
func pascalCase(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// fieldColumnPairs walks a Sequelize attributes object literal and returns
// one entry per field: "name" when the DB column matches, "name→column" when
// a `field: 'snake_case'` mapping renames it — the mapping SQL-side greps
// can't see.
func fieldColumnPairs(obj *sitter.Node, data []byte) []string {
	if obj == nil || obj.Type() != "object" {
		return nil
	}
	var out []string
	for i := 0; i < int(obj.NamedChildCount()); i++ {
		pair := obj.NamedChild(i)
		if pair.Type() != "pair" {
			continue
		}
		keyNode := pair.ChildByFieldName("key")
		if keyNode == nil {
			continue
		}
		name := strings.Trim(keyNode.Content(data), "\"'`")
		if name == "" {
			continue
		}
		entry := name
		if val := pair.ChildByFieldName("value"); val != nil && val.Type() == "object" {
			if col := objectStringValue(val, data, "field"); col != "" && col != name {
				entry = name + "→" + col
			}
		}
		out = append(out, entry)
	}
	sort.Strings(out)
	return out
}

// objectStringValue returns the string value of the first of the given keys
// present in an object literal, or "".
func objectStringValue(obj *sitter.Node, data []byte, keys ...string) string {
	for i := 0; i < int(obj.NamedChildCount()); i++ {
		pair := obj.NamedChild(i)
		if pair.Type() != "pair" {
			continue
		}
		keyNode := pair.ChildByFieldName("key")
		if keyNode == nil {
			continue
		}
		k := strings.Trim(keyNode.Content(data), "\"'`")
		for _, want := range keys {
			if k != want {
				continue
			}
			val := pair.ChildByFieldName("value")
			if val == nil {
				return ""
			}
			switch val.Type() {
			case "string":
				return strings.Trim(val.Content(data), "\"'`")
			}
			return ""
		}
	}
	return ""
}
