package codebase

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

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
		// a variable DECLARED as the imported Helper resolves through its type;
		// a library type (java.util.List) proves nothing
		"src/main/java/com/poc/Lower.java": "package com.poc;\nimport com.poc.util.Helper;\npublic class Lower { void m(java.util.List l, Helper helper) { l.add(1); helper.hello(); } }\n",
		// the same variable name with an UNKNOWN type proves nothing
		"src/main/java/com/poc/Untyped.java": "package com.poc;\npublic class Untyped { void m(Unknown helper) { helper.hello(); } }\n",
		// no import and a different package: Helper is not visible
		"src/main/java/com/poc/far/Far.java": "package com.poc.far;\npublic class Far { public String run() { return Helper.hello(); } }\n",
	})
	wantEdges(t, got,
		"run->hello@src/main/java/com/poc/util/Helper.java",
		"m->hello@src/main/java/com/poc/util/Helper.java")
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

// `import html` is the stdlib; a different statement imports pkg/html.py. The
// receiver must be tied to the import that actually binds it.
func TestReceiverPythonBindingMustMatchItsOwnImport(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/html.py":     "def escape(s):\n    return s\nVOID = 1\n",
		"a.py":            "import html\nfrom pkg.html import VOID\n\ndef f(c):\n    return html.escape(c)\n",
		"b.py":            "from pkg import html\n\ndef g(c):\n    return html.escape(c)\n",
	})
	// a.py's `html` is the stdlib module; only b.py's binds pkg/html.py.
	wantEdges(t, got, "g->escape@pkg/html.py")
}

func TestPythonImportBindings(t *testing.T) {
	src := []byte("import os, sys as system\nimport a.b.c\nimport a.b.d as dd\nfrom m import x, y as z\nfrom n import (\n    p,\n    q as r,\n)\nfrom o import *\nfrom . import u\nfrom .w import v\n  # import fake\n")
	got := pythonImportBindings(src)
	want := map[string][]string{
		"os": {"os"}, "system": {"sys"}, "a": {"a"}, "dd": {"a.b.d"},
		"x": {"m.x", "m"}, "z": {"m.y", "m"}, "p": {"n.p", "n"}, "r": {"n.q", "n"},
		"u": {".u", "."}, "v": {".w.v", ".w"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bindings:\n  got  %v\n  want %v", got, want)
	}
}

// extractEdgesWithLines is extractFiles but the target also carries its line, to
// tell apart same-named methods of different types in one file.
func extractEdgesWithLines(t *testing.T, files map[string]string) []string {
	t.Helper()
	tmp := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
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
		out = append(out, from.Name+"->"+to.Name+"@"+to.Attrs["file"]+":"+to.Attrs["line_start"])
	}
	sort.Strings(out)
	return out
}

func TestTypedReceiverGoPicksTheDeclaredTypesMethod(t *testing.T) {
	got := extractEdgesWithLines(t, map[string]string{
		"go.mod": "module example.com/v\n\ngo 1.22\n",
		"p/p.go": `package p

type A struct{}
type B struct{}

func (a *A) Run() {}
func (b *B) Run() {}

func useA(x *A)    { x.Run() }
func useB(y B)     { y.Run() }
func untyped()     { z := NewA(); z.Run() }
func NewA() *A     { return &A{} }
`,
	})
	// useA -> A.Run (line 6), useB -> B.Run (line 7); z has no declared type.
	wantEdges(t, got, "useA->Run@p/p.go:6", "useB->Run@p/p.go:7")
}

func TestTypedReceiverGoAcrossPackages(t *testing.T) {
	got := extractEdgesWithLines(t, map[string]string{
		"go.mod":         "module example.com/v\n\ngo 1.22\n",
		"repo/repo.go":   "package repo\n\ntype Repo struct{}\n\nfunc (r *Repo) Find() {}\n",
		"other/o.go":     "package other\n\ntype Repo struct{}\n\nfunc (r *Repo) Find() {}\n",
		"svc/service.go": "package svc\n\nimport \"example.com/v/repo\"\n\nfunc Do(r *repo.Repo) { r.Find() }\n",
	})
	wantEdges(t, got, "Do->Find@repo/repo.go:5")
}

func TestTypedReceiverCSharpField(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"Poc.csproj":                     "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup><RootNamespace>Poc</RootNamespace></PropertyGroup></Project>\n",
		"Services/UserService.cs":        "namespace Poc.Services { public class UserService { public string Find() { return \"u\"; } } }\n",
		"Controllers/UsersController.cs": "using Poc.Services;\nnamespace Poc.Controllers { public class UsersController {\n  private readonly UserService _svc;\n  public string Get() { return _svc.Find(); }\n  public string Other(Unknown u) { return u.Find(); } } }\n",
	})
	wantEdges(t, got, "Get->Find@Services/UserService.cs")
}

func TestTypedReceiverTypeScriptAndKotlin(t *testing.T) {
	ts := extractFiles(t, map[string]string{
		"src/svc.ts": "export class UserService { find() { return 1; } }\n",
		"src/app.ts": "import { UserService } from './svc';\nexport class App {\n  constructor(private svc: UserService) {}\n  run() { return this.svc.find(); }\n  run2(s: UserService) { return s.find(); }\n}\n",
	})
	// this.svc.find() names the field `svc` (declared as a constructor property).
	wantEdges(t, ts, "run->find@src/svc.ts", "run2->find@src/svc.ts")

	kt := extractFiles(t, map[string]string{
		"src/main/kotlin/com/poc/svc/UserService.kt": "package com.poc.svc\nclass UserService { fun find(): Int = 1 }\n",
		"src/main/kotlin/com/poc/App.kt":             "package com.poc\nimport com.poc.svc.UserService\nclass App(private val svc: UserService) { fun run(): Int = svc.find() }\n",
	})
	wantEdges(t, kt, "run->find@src/main/kotlin/com/poc/svc/UserService.kt")
}

func TestTypedReceiverConflictingDeclarationsAreIgnored(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/java/com/poc/A.java": "package com.poc;\npublic class A { public String go() { return \"a\"; } }\n",
		"src/main/java/com/poc/B.java": "package com.poc;\npublic class B { public String go() { return \"b\"; } }\n",
		// `x` is an A in one method and a B in the other: which one is x.go()? unknown.
		"src/main/java/com/poc/C.java": "package com.poc;\npublic class C { void m1(A x) { x.go(); } void m2(B x) { x.go(); } }\n",
	})
	wantEdges(t, got)
}

// Found on ktor: `data` was declared as a typed parameter in one method and as an
// untyped `val data = ...` in another; the second one inherited the first's type.
func TestTypedReceiverIgnoresNamesRedeclaredWithoutAType(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/kotlin/com/poc/Store.kt": "package com.poc\nabstract class Store { abstract fun find(): Int }\n",
		"src/main/kotlin/com/poc/Use.kt": "package com.poc\n" +
			"class Use {\n" +
			"  fun typed(data: Store): Int = data.find()\n" +
			"  fun untyped(): Int { val data = makeSomething(); return data.find() }\n" +
			"}\n",
	})
	// `data` means two different things in this file: neither call is provable.
	wantEdges(t, got)

	only := extractFiles(t, map[string]string{
		"src/main/kotlin/com/poc/Store.kt": "package com.poc\nabstract class Store { abstract fun find(): Int }\n",
		"src/main/kotlin/com/poc/Use.kt":   "package com.poc\nclass Use { fun typed(data: Store): Int = data.find() }\n",
	})
	wantEdges(t, only, "typed->find@src/main/kotlin/com/poc/Store.kt")
}

func TestTypedReceiverThroughThisField(t *testing.T) {
	java := extractFiles(t, map[string]string{
		"src/main/java/com/poc/svc/UserService.java": "package com.poc.svc;\npublic class UserService { public String find() { return \"u\"; } }\n",
		"src/main/java/com/poc/App.java":             "package com.poc;\nimport com.poc.svc.UserService;\npublic class App {\n  private UserService svc;\n  public String run() { return this.svc.find(); }\n  public String deep() { return this.svc.inner.find(); }\n}\n",
	})
	wantEdges(t, java, "run->find@src/main/java/com/poc/svc/UserService.java")

	ts := extractFiles(t, map[string]string{
		"src/svc.ts": "export class UserService { find() { return 1; } }\n",
		"src/app.ts": "import { UserService } from './svc';\nexport class App {\n  constructor(private svc: UserService) {}\n  run() { return this.svc.find(); }\n}\n",
	})
	wantEdges(t, ts, "run->find@src/svc.ts")

	cs := extractFiles(t, map[string]string{
		"Poc.csproj":              "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup><RootNamespace>Poc</RootNamespace></PropertyGroup></Project>\n",
		"Services/UserService.cs": "namespace Poc.Services { public class UserService { public string Find() { return \"u\"; } } }\n",
		"App.cs":                  "using Poc.Services;\nnamespace Poc { public class App {\n  private UserService _svc;\n  public string Run() { return this._svc.Find(); } } }\n",
	})
	wantEdges(t, cs, "Run->Find@Services/UserService.cs")

	kt := extractFiles(t, map[string]string{
		"src/main/kotlin/com/poc/svc/UserService.kt": "package com.poc.svc\nclass UserService { fun find(): Int = 1 }\n",
		"src/main/kotlin/com/poc/App.kt":             "package com.poc\nimport com.poc.svc.UserService\nclass App(private val svc: UserService) { fun run(): Int = this.svc.find() }\n",
	})
	wantEdges(t, kt, "run->find@src/main/kotlin/com/poc/svc/UserService.kt")
}

// An incremental refresh re-parses only changed files: the receiver edges of a
// changed caller must be rebuilt, and those of untouched callers must survive.
func TestReceiverEdgesSurviveIncrementalUpdate(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main/java/com/poc/util/Helper.java", "package com.poc.util;\npublic class Helper { public static String hello() { return \"x\"; } public static String bye() { return \"y\"; } }\n")
	write("src/main/java/com/poc/A.java", "package com.poc;\nimport com.poc.util.Helper;\npublic class A { public String run() { return Helper.hello(); } }\n")
	write("src/main/java/com/poc/B.java", "package com.poc;\nimport com.poc.util.Helper;\npublic class B { public String run() { return Helper.hello(); } }\n")

	c := New(tmp)
	g, err := c.Extract(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	edges := func(g *graph.Graph) []string {
		var out []string
		for _, e := range g.Edges {
			if e.Type == graph.EdgeCalls && e.Attrs["via"] == "receiver" {
				out = append(out, g.Nodes[e.From].Attrs["file"]+"->"+g.Nodes[e.To].Name)
			}
		}
		sort.Strings(out)
		return out
	}
	wantEdges(t, edges(g), "src/main/java/com/poc/A.java->hello", "src/main/java/com/poc/B.java->hello")

	// Change A so that it calls bye() instead; B is untouched.
	write("src/main/java/com/poc/A.java", "package com.poc;\nimport com.poc.util.Helper;\npublic class A { public String run() { return Helper.bye(); } }\n")
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(tmp, "src/main/java/com/poc/A.java"), future, future)

	g2, summary, err := New(tmp).Update(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if summary == "" {
		t.Fatal("Update saw no drift")
	}
	wantEdges(t, edges(g2), "src/main/java/com/poc/A.java->bye", "src/main/java/com/poc/B.java->hello")

	// Now change the TARGET file (its functions are evicted and re-added): callers
	// that did not change must keep pointing at the re-parsed functions.
	write("src/main/java/com/poc/util/Helper.java", "package com.poc.util;\n// edited\npublic class Helper { public static String hello() { return \"x2\"; } public static String bye() { return \"y2\"; } }\n")
	later := time.Now().Add(2 * time.Hour)
	os.Chtimes(filepath.Join(tmp, "src/main/java/com/poc/util/Helper.java"), later, later)
	g3, summary, err := New(tmp).Update(context.Background(), g2)
	if err != nil || summary == "" {
		t.Fatalf("second Update: %q %v", summary, err)
	}
	wantEdges(t, edges(g3), "src/main/java/com/poc/A.java->bye", "src/main/java/com/poc/B.java->hello")
}

// Found on Elasticsearch: `routing` is a ShardRouting variable, and the caller also
// imports a package whose directory is named `routing`. The package must not capture it.
func TestJavaPackageNamedLikeAVariableDoesNotCaptureIt(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/java/com/poc/routing/Shard.java": "package com.poc.routing;\npublic class Shard { public String index() { return \"s\"; } }\n",
		"src/main/java/com/poc/routing/Table.java": "package com.poc.routing;\npublic class Table { public String index() { return \"t\"; } public String other() { return \"o\"; } }\n",
		"src/main/java/com/poc/Use.java":           "package com.poc;\nimport com.poc.routing.Shard;\nimport com.poc.routing.Table;\npublic class Use { void m(Shard routing) { routing.index(); } }\n",
	})
	// Only the declared type's method, not every index() of the `routing` package.
	wantEdges(t, got, "m->index@src/main/java/com/poc/routing/Shard.java")
}

// In Go a variable declared in the file shadows an imported package of that name.
func TestGoVariableShadowsImportedPackage(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"go.mod":       "module example.com/v\n\ngo 1.22\n",
		"util/util.go": "package util\n\nfunc Helper() string { return \"x\" }\n",
		"main.go":      "package main\n\nimport \"example.com/v/util\"\n\nfunc a() string { return util.Helper() }\nfunc b() string { util := makeThing(); return util.Helper() }\n",
	})
	// `util` is both an import and a local variable in this file: neither call is provable.
	wantEdges(t, got)
}

// Same Java shape, but `routing` is an inherited field: nothing in the caller's file
// declares it, so only "package-qualified receivers are Go-only" keeps the imported
// package `routing` from capturing it.
func TestJavaImportedPackageNeverQualifiesACall(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"src/main/java/com/poc/routing/Table.java": "package com.poc.routing;\npublic class Table { public String index() { return \"t\"; } }\n",
		"src/main/java/com/poc/Base.java":          "package com.poc;\npublic class Base { protected Object routing; }\n",
		"src/main/java/com/poc/Use.java":           "package com.poc;\nimport com.poc.routing.Table;\npublic class Use extends Base { void m() { routing.index(); } }\n",
	})
	wantEdges(t, got)
}

func TestCSharpExtensionMethodThroughDeclaredType(t *testing.T) {
	csproj := "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup><RootNamespace>Poc</RootNamespace></PropertyGroup></Project>\n"
	got := extractFiles(t, map[string]string{
		"Poc.csproj":                  csproj,
		"Abstractions/IAppBuilder.cs": "namespace Poc.Abstractions { public interface IAppBuilder { void Own(); } }\n",
		"Abstractions/IOther.cs":      "namespace Poc.Abstractions { public interface IOther { } }\n",
		"Builder/Ext.cs": "using Poc.Abstractions;\nnamespace Poc.Builder { public static class Ext {\n" +
			"  public static IAppBuilder UseRouting(this IAppBuilder app) { return app; }\n" +
			"  public static IOther UseRouting(this IOther o) { return o; }\n" +
			"  public static void Own(this IAppBuilder app) { }\n} }\n",
		"Startup.cs": "using Poc.Abstractions;\nusing Poc.Builder;\nnamespace Poc { public class Startup {\n" +
			"  public void Configure(IAppBuilder app) { app.UseRouting(); app.Own(); }\n" +
			"  public void Lib(Unknown u) { u.UseRouting(); }\n} }\n",
	})
	// UseRouting on IAppBuilder -> only the IAppBuilder overload (not IOther's).
	// Own() is an INSTANCE method of IAppBuilder, which wins over the extension: it
	// resolves to the interface's own declaration, never to Ext.Own.
	// Unknown is not defined in the repo: nothing is provable.
	wantEdges(t, got, "Configure->Own@Abstractions/IAppBuilder.cs", "Configure->UseRouting@Builder/Ext.cs")
}

func TestCSharpExtensionReceiver(t *testing.T) {
	for in, want := range map[string]string{
		"UseRouting(this IApplicationBuilder app)":            "IApplicationBuilder",
		"Add<T>(this IServiceCollection services, T x)":       "IServiceCollection",
		"Map(this Microsoft.AspNetCore.Builder.IEndpoints e)": "IEndpoints",
		"Normal(IApplicationBuilder app)":                     "",
		"Run(IThing thisOne)":                                 "",
	} {
		if got := csharpExtensionReceiver(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// C# visibility is by namespace, not directory; and a refresh that re-parses only
// the caller must still know the namespaces of the files it does not re-parse.
func TestCSharpNamespaceVisibilityAndIncrementalUpdate(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The namespace has nothing to do with the folders.
	write("a/b/c/Widget.cs", "namespace Acme.Core.Things { public class Widget { public string Spin() { return \"s\"; } } }\n")
	write("x/Other.cs", "namespace Acme.Hidden { public class Widget { public string Spin() { return \"h\"; } } }\n")
	write("z/User.cs", "using Acme.Core.Things;\nnamespace Acme.App { public class User { public string Go(Widget w) { return w.Spin(); } } }\n")
	write("z/Same.cs", "namespace Acme.Core.Things.Deep { public class Same { public string Go(Widget w) { return w.Spin(); } } }\n")
	write("z/None.cs", "namespace Acme.Elsewhere { public class None { public string Go(Widget w) { return w.Spin(); } } }\n")

	c := New(tmp)
	g, err := c.Extract(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	edges := func(g *graph.Graph) []string {
		var out []string
		for _, e := range g.Edges {
			if e.Type == graph.EdgeCalls && e.Attrs["via"] == "receiver" {
				out = append(out, g.Nodes[e.From].Attrs["file"]+"->"+g.Nodes[e.To].Attrs["file"])
			}
		}
		sort.Strings(out)
		return out
	}
	// `using` imports the namespace; an enclosing namespace is visible without one;
	// an unrelated namespace sees nothing (two Widget types exist, neither is in scope).
	want := []string{"z/Same.cs->a/b/c/Widget.cs", "z/User.cs->a/b/c/Widget.cs"}
	wantEdges(t, edges(g), want...)

	// Re-parse only the caller: Widget.cs is untouched, its namespace must persist.
	write("z/User.cs", "using Acme.Core.Things;\n// touched\nnamespace Acme.App { public class User { public string Go(Widget w) { return w.Spin(); } } }\n")
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(tmp, "z/User.cs"), future, future)
	g2, summary, err := New(tmp).Update(context.Background(), g)
	if err != nil || summary == "" {
		t.Fatalf("Update: %q %v", summary, err)
	}
	wantEdges(t, edges(g2), want...)
}

// A Python parameter (or local) with the same name as an imported module shadows it.
func TestPythonLocalShadowsImportedModule(t *testing.T) {
	got := extractFiles(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/graph.py":    "def add_node(n):\n    return n\n",
		"use.py": "from pkg import graph\n\n" +
			"def uses_module():\n    return graph.add_node(1)\n\n" +
			"def uses_param(graph):\n    return graph.add_node(2)\n",
	})
	// `graph` is both the module and a parameter name in this file: neither call is provable.
	wantEdges(t, got)

	clean := extractFiles(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/graph.py":    "def add_node(n):\n    return n\n",
		"use.py":          "from pkg import graph\n\ndef uses_module():\n    return graph.add_node(1)\n",
	})
	wantEdges(t, clean, "uses_module->add_node@pkg/graph.py")
}
