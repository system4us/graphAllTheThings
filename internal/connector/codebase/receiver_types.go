package codebase

// Declared types of variables, parameters and fields, so that `svc.find()` can
// be resolved when `svc` is declared as a UserService (receiver.go does the
// resolving). Collected with a second tree-sitter query per language, compiled
// separately from the main one: if a grammar rejects a pattern, only typed
// receivers are disabled for that language, never the whole extraction.

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// declQueryFor returns the declaration query for a source extension. Every
// pattern captures @decl.name (the variable) and @decl.type (its type node).
func declQueryFor(ext string) string {
	switch ext {
	case ".java":
		return `
		(formal_parameter type: (_) @decl.type name: (identifier) @decl.name)
		(local_variable_declaration type: (_) @decl.type declarator: (variable_declarator name: (identifier) @decl.name))
		(field_declaration type: (_) @decl.type declarator: (variable_declarator name: (identifier) @decl.name))
		(enhanced_for_statement type: (_) @decl.type name: (identifier) @decl.name)
		(formal_parameter name: (identifier) @decl.any)
		(local_variable_declaration declarator: (variable_declarator name: (identifier) @decl.any))
		(field_declaration declarator: (variable_declarator name: (identifier) @decl.any))
		(enhanced_for_statement name: (identifier) @decl.any)
		(inferred_parameters (identifier) @decl.any)
		(catch_formal_parameter name: (identifier) @decl.any)
		`
	case ".cs":
		return `
		(parameter type: (_) @decl.type name: (identifier) @decl.name)
		(variable_declaration type: (_) @decl.type (variable_declarator (identifier) @decl.name))
		(property_declaration type: (_) @decl.type name: (identifier) @decl.name)
		(parameter name: (identifier) @decl.any)
		(variable_declaration (variable_declarator (identifier) @decl.any))
		(property_declaration name: (identifier) @decl.any)
		(foreach_statement left: (identifier) @decl.any)
		`
	case ".kt":
		return `
		(parameter (simple_identifier) @decl.name (user_type) @decl.type)
		(class_parameter (simple_identifier) @decl.name (user_type) @decl.type)
		(property_declaration (variable_declaration (simple_identifier) @decl.name (user_type) @decl.type))
		(parameter (simple_identifier) @decl.any)
		(class_parameter (simple_identifier) @decl.any)
		(property_declaration (variable_declaration (simple_identifier) @decl.any))
		(lambda_parameters (variable_declaration (simple_identifier) @decl.any))
		(for_statement (variable_declaration (simple_identifier) @decl.any))
		`
	case ".ts", ".tsx":
		return `
		(required_parameter pattern: (identifier) @decl.name type: (type_annotation (_) @decl.type))
		(optional_parameter pattern: (identifier) @decl.name type: (type_annotation (_) @decl.type))
		(public_field_definition name: (property_identifier) @decl.name type: (type_annotation (_) @decl.type))
		(variable_declarator name: (identifier) @decl.name type: (type_annotation (_) @decl.type))
		(required_parameter pattern: (identifier) @decl.any)
		(optional_parameter pattern: (identifier) @decl.any)
		(public_field_definition name: (property_identifier) @decl.any)
		(variable_declarator name: (identifier) @decl.any)
		(arrow_function parameter: (identifier) @decl.any)
		`
	case ".go":
		return `
		(parameter_declaration name: (identifier) @decl.name type: (_) @decl.type)
		(var_spec name: (identifier) @decl.name type: (_) @decl.type)
		(parameter_declaration name: (identifier) @decl.any)
		(var_spec name: (identifier) @decl.any)
		(short_var_declaration left: (expression_list (identifier) @decl.any))
		(range_clause left: (expression_list (identifier) @decl.any))
		`
	}
	return ""
}

// typeNameOf reduces a declared-type node to the bare name of the type it
// names: List<Foo> -> List, *pkg.Foo -> pkg.Foo (Go keeps the package
// qualifier), a.b.Foo -> Foo, Foo? -> Foo. Built-in types, arrays, function
// types and anything else return "".
func typeNameOf(n *sitter.Node, data []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "type_identifier", "identifier", "simple_identifier":
		return n.Content(data)
	case "generic_type", "generic_name", "nullable_type", "pointer_type", "reference_type", "user_type", "type":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			if name := typeNameOf(n.NamedChild(i), data); name != "" {
				return name
			}
		}
	case "qualified_type": // Go: pkg.Type
		var pkg, name string
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			switch c.Type() {
			case "package_identifier":
				pkg = c.Content(data)
			case "type_identifier":
				name = c.Content(data)
			}
		}
		if pkg != "" && name != "" {
			return pkg + "." + name
		}
	case "scoped_type_identifier", "qualified_name":
		if cnt := int(n.NamedChildCount()); cnt > 0 {
			return typeNameOf(n.NamedChild(cnt-1), data)
		}
	}
	return ""
}

// collectDecls returns variable name -> declared type name for one file. The
// scope of a declaration is not tracked, so a name is only trusted when EVERY
// declaration of it in the file (parameter, local, field, loop variable...)
// carries the same type: `val data = compute()` in one method must not inherit
// the type of `data: Store` declared in another. Anything else maps to "".
func collectDecls(tree *sitter.Tree, q *sitter.Query, data []byte) map[string]string {
	any := map[string]int{}
	typed := map[string]map[string]int{}
	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, tree.RootNode())
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		var name, typ, anyName string
		for _, c := range m.Captures {
			switch q.CaptureNameForId(c.Index) {
			case "decl.name":
				name = c.Node.Content(data)
			case "decl.type":
				typ = typeNameOf(c.Node, data)
			case "decl.any":
				anyName = c.Node.Content(data)
			}
		}
		if anyName != "" {
			any[anyName]++
			continue
		}
		if name == "" || typ == "" || strings.EqualFold(typ, "var") || typ == "dynamic" {
			continue
		}
		if typed[name] == nil {
			typed[name] = map[string]int{}
		}
		typed[name][typ]++
	}
	out := map[string]string{}
	for name, n := range any {
		out[name] = "" // trusted only if proven below
		if len(typed[name]) == 1 {
			for typ, cnt := range typed[name] {
				if cnt == n {
					out[name] = typ
				}
			}
		}
	}
	return out
}
