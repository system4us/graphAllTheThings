package codebase

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"graphallthethings/internal/graph"
)

// extractFiles writes files into a temp dir, extracts it and returns the
// receiver-resolved CALLS edges as sorted "caller->target@targetFile" strings.
func extractFiles(t *testing.T, files map[string]string) []string {
	t.Helper()
	tmp := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := New(tmp).Extract(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range g.Edges {
		if e.Type != graph.EdgeCalls || e.Attrs["via"] != "receiver" {
			continue
		}
		from, to := g.Nodes[e.From], g.Nodes[e.To]
		if from == nil || to == nil {
			continue
		}
		out = append(out, from.Name+"->"+to.Name+"@"+to.Attrs["file"])
	}
	sort.Strings(out)
	return out
}

func wantEdges(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("receiver edges:\n  got  %v\n  want %v", got, want)
	}
}

func TestReceiverJava(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/java/com/poc/util/Helper.java":  "package com.poc.util;\npublic class Helper { public static String hello() { return \"x\"; } }\n",
		"src/main/java/com/poc/other/Helper.java": "package com.poc.other;\npublic class Helper { public static String hello() { return \"y\"; } }\n",
		// imports the class: provable
		"src/main/java/com/poc/App.java": "package com.poc;\nimport com.poc.util.Helper;\npublic class App { public String run() { return Helper.hello(); } }\n",
		// both packages imported: two different Helper types, ambiguous
		"src/main/java/com/poc/Amb.java": "package com.poc;\nimport com.poc.util.*;\nimport com.poc.other.*;\npublic class Amb { public String run() { return Helper.hello(); } }\n",
		// lower-case variables and library types: nothing to prove
		"src/main/java/com/poc/Lower.java": "package com.poc;\nimport com.poc.util.Helper;\npublic class Lower { void m(java.util.List l, Helper helper) { l.add(1); helper.hello(); } }\n",
		// no import and a different package: Helper is not visible
		"src/main/java/com/poc/far/Far.java": "package com.poc.far;\npublic class Far { public String run() { return Helper.hello(); } }\n",
	})
	wantEdges(t, got, "run->hello@src/main/java/com/poc/util/Helper.java")
}

func TestReceiverGoPackage(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"go.mod":         "module example.com/v\n\ngo 1.22\n",
		"util/util.go":   "package util\n\nfunc Helper() string { return \"x\" }\n\ntype T struct{}\n\nfunc (T) Helper() string { return \"method\" }\n",
		"main.go":        "package main\n\nimport \"example.com/v/util\"\n\nfunc a() string { return util.Helper() }\nfunc b() string { return other.Helper() }\nfunc main()     { a(); b() }\n",
		"other/other.go": "package other\n\nfunc Helper() string { return \"o\" }\n",
	})
	// util is imported: a -> the package-level func, not the method of T.
	// other is NOT imported: b gets nothing.
	if len(got) != 1 || got[0] != "a->Helper@util/util.go" {
		t.Errorf("got %v, want [a->Helper@util/util.go]", got)
	}
}

func TestReceiverCSharp(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"Poc.csproj":      "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup><RootNamespace>Poc</RootNamespace></PropertyGroup></Project>\n",
		"Util/Helper.cs":  "namespace Poc.Util { public static class Helper { public static string Hello() { return \"x\"; } } }\n",
		"Other/Helper.cs": "namespace Poc.Other { public static class Helper { public static string Hello() { return \"y\"; } } }\n",
		"App.cs":          "using Poc.Util;\nnamespace Poc { public class App { public string Run() { return Helper.Hello(); } } }\n",
	})
	// Two Helper types exist but only Util's namespace is imported.
	wantEdges(t, got, "Run->Hello@Util/Helper.cs")
}

func TestReceiverKotlin(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/kotlin/com/poc/util/Helper.kt": "package com.poc.util\nobject Helper { fun hello(): String = \"x\" }\n",
		"src/main/kotlin/com/poc/App.kt":         "package com.poc\nimport com.poc.util.Helper\nfun run(): String = Helper.hello()\n",
	})
	wantEdges(t, got, "run->hello@src/main/kotlin/com/poc/util/Helper.kt")
}

func TestReceiverTypeScript(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/foo.ts": "export class Foo { static bar() { return 1; } }\n",
		"src/oth.ts": "export class Oth { static bar() { return 2; } }\n",
		"src/app.ts": "import { Foo } from './foo';\nexport function run() { Foo.bar(); foo.bar(); Missing.bar(); }\n",
	})
	// Only the imported binding is provable; a lower-case or unknown receiver is not.
	wantEdges(t, got, "run->bar@src/foo.ts")
}

func TestReceiverPythonNeedsAnImportBinding(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/graph.py":    "class MigrationGraph:\n    def add_node(self, n):\n        return n\n",
		// `graph` is a local variable; the import binds MigrationGraph, not `graph`.
		"test_a.py": "from pkg.graph import MigrationGraph\n\ndef test():\n    graph = MigrationGraph()\n    graph.add_node(1)\n",
		// `import pkg.graph as graph` really binds `graph` to that module.
		"test_b.py": "import pkg.graph as graph\n\ndef test2():\n    graph.add_node(1)\n",
	})
	wantEdges(t, got, "test2->add_node@pkg/graph.py")
}

func TestPythonImportBindings(t *testing.T) {
	src := []byte("import os, sys as system\nimport a.b.c\nimport a.b.d as dd\nfrom m import x, y as z\nfrom n import (\n    p,\n    q as r,\n)\nfrom o import *\n  # import fake\n")
	got := pythonImportBindings(src)
	for _, name := range []string{"os", "system", "a", "dd", "x", "z", "p", "r"} {
		if !got[name] {
			t.Errorf("binding %q missing: %v", name, got)
		}
	}
	for _, name := range []string{"sys", "c", "y", "q", "fake", "*"} {
		if got[name] {
			t.Errorf("unexpected binding %q: %v", name, got)
		}
	}
}

func TestReceiverTypeNameAllowsUnnamedReceivers(t *testing.T) {
	// receiverTypeName is fed the raw "(...)" parameter list of a Go method.
	for in, want := range map[string]string{
		"(s *Set[T])": "Set[T]", "(s Set)": "Set", "(*Set)": "Set", "(Set)": "Set", "(_ *T)": "T", "()": "",
	} {
		if got := receiverTypeFromText(in); got != want {
			t.Errorf("receiver %q -> %q, want %q", in, got, want)
		}
	}
}
