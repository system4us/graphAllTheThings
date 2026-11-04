package codebase

import (
	"context"
	"reflect"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

func declsOf(t *testing.T, ext, src string) map[string]string {
	t.Helper()
	lc := langFor(ext)
	if lc == nil {
		t.Fatalf("no language for %s", ext)
	}
	q, err := sitter.NewQuery([]byte(declQueryFor(ext)), lc.lang)
	if err != nil {
		t.Fatalf("%s decl query does not compile: %v", ext, err)
	}
	p := sitter.NewParser()
	p.SetLanguage(lc.lang)
	tree, err := p.ParseCtx(context.Background(), nil, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return collectDecls(tree, q, []byte(src))
}

func TestDeclQueries(t *testing.T) {
	cases := []struct {
		name, ext, src string
		want           map[string]string
	}{
		{"java", ".java", `
class A {
  private UserService svc;
  List<Order> orders;
  void m(Repo repo, int n, java.util.Map<String,String> map) {
    Helper h = new Helper();
    var x = new Foo();
    for (Item it : items) {}
  }
}`, map[string]string{"svc": "UserService", "orders": "List", "repo": "Repo", "map": "Map", "h": "Helper", "it": "Item"}},
		{"csharp", ".cs", `
class A {
  private readonly IUserService _svc;
  public Order Current { get; set; }
  void M(Repo repo, string s) {
    Helper h = new Helper();
    var x = new Foo();
  }
}`, map[string]string{"_svc": "IUserService", "Current": "Order", "repo": "Repo", "h": "Helper"}},
		{"kotlin", ".kt", `
class A(private val repo: Repo, val n: Int) {
  private val svc: UserService = UserService()
  fun m(h: Helper, l: List<Order>) {}
}`, map[string]string{"repo": "Repo", "n": "Int", "svc": "UserService", "h": "Helper", "l": "List"}},
		{"typescript", ".ts", `
class A {
  private svc: UserService;
  constructor(private repo: Repo, n: number) {}
  m(h?: Helper) { const x: Order = f(); let y = 1; }
}`, map[string]string{"svc": "UserService", "repo": "Repo", "h": "Helper", "x": "Order"}},
		{"go", ".go", `
package p
func f(s *UserService, r util.Repo, a, b int) {
  var h Helper
  x := New()
  _ = x
}`, map[string]string{"s": "UserService", "r": "util.Repo", "a": "int", "b": "int", "h": "Helper"}},
		{"conflict", ".java", `
class A { void a(Foo x) {} void b(Bar x) {} }`, map[string]string{"x": ""}},
		{"untyped-redeclaration-java", ".java", `
class A { void a(Foo x) {} void b() { var x = make(); } }`, map[string]string{"x": ""}},
		{"untyped-redeclaration-kotlin", ".kt", `
class A { fun a(data: Store) {} fun b() { val data = make() } }`, map[string]string{"data": ""}},
		{"untyped-redeclaration-go", ".go", `
package p
func a(s *Store) {}
func b() { s := make2(); _ = s }`, map[string]string{"s": ""}},
		{"untyped-redeclaration-ts", ".ts", `
class A { a(s: Store) {} b() { const s = make(); } }`, map[string]string{"s": ""}},
		{"untyped-redeclaration-cs", ".cs", `
class A { void A1(Store s) {} void B() { var s = Make(); } }`, map[string]string{"s": ""}},
	}
	for _, c := range cases {
		got := declsOf(t, c.ext, c.src)
		// Untrusted names map to "": only compare them when the case expects one.
		for k, v := range got {
			if _, want := c.want[k]; v == "" && !want {
				delete(got, k)
			}
		}
		// Primitive types may appear (Kotlin Int, Go int): harmless, resolution
		// needs a visible definition with that name and there is none.
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n  got  %v\n  want %v", c.name, got, c.want)
		}
	}
}
