package codebase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

// bruteForceJVM is the original resolver: try every root in order, full path
// first and then the path minus its last segment, hitting the disk each time.
func bruteForceJVM(c *Connector, g *graph.Graph, importPath string) string {
	segments := strings.Split(importPath, ".")
	tries := [][]string{segments}
	if len(segments) > 1 {
		tries = append(tries, segments[:len(segments)-1])
	}
	for _, root := range c.jvmSourceRoots {
		for _, segs := range tries {
			if id := c.resolveDirSegments(g, root, segs, strings.Join(segs, ".")); id != "" {
				return id
			}
		}
	}
	return ""
}

func TestJVMImportIndexMatchesBruteForce(t *testing.T) {
	tmp := t.TempDir()
	for _, d := range []string{
		"flat/pkg",                               // flat layout straight under the extraction root
		"a/src/main/java/com/acme/util",          // module a
		"a/src/main/java/com/acme/only_a",        //
		"b/src/test/java/com/acme/util",          // module b: same package as a (a must win)
		"b/src/test/java/com/acme/only_b/deep",   //
		"c/src/main/kotlin/org/kt/lib",           // kotlin module
		"c/src/main/kotlin/com/acme/util/nested", // third copy of com.acme.util
		"d/src/main/java/com/acme/Shadow",        // a directory named like a class
		"node_modules/dep/src/main/java/ignored", // must be skipped, like the walk does
	} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := New(tmp)
	g := graph.New("codebase:" + tmp)
	c.loadJVMRoots()
	c.jvmRootsLoaded = true
	if len(c.jvmSourceRoots) < 5 {
		t.Fatalf("setup: only %d source roots found: %v", len(c.jvmSourceRoots), c.jvmSourceRoots)
	}

	imports := []string{
		"com.acme.util.Helper", // class import: parent dir is the package
		"com.acme.util.*",      // wildcard: full path is the package
		"com.acme.only_a.X", "com.acme.only_b.deep.Y", "com.acme.only_b.deep",
		"org.kt.lib.Foo", "com.acme.util.nested.Z",
		"com.acme.Shadow", "com.acme.Shadow.Inner", // dir named like a class
		"flat.pkg.Thing", "flat.pkg",
		"java.util.List", "java.util.*", "org.apache.lucene.Index", "nope", "com", "com.acme",
		"ignored.X", "dep.src.main.java.ignored",
	}
	hits := 0
	for _, imp := range imports {
		want := bruteForceJVM(c, g, imp)
		got := c.resolveJVMImport(g, imp)
		if got != want {
			t.Errorf("import %q: indexed=%q brute-force=%q", imp, got, want)
		}
		if want != "" {
			hits++
		}
	}
	if hits < 8 {
		t.Errorf("only %d imports resolved at all; the fixture is not exercising the resolver", hits)
	}

	// The earliest root wins: com.acme.util must come from module a.
	id := c.resolveJVMImport(g, "com.acme.util.Helper")
	if n := g.Nodes[id]; n == nil || !strings.Contains(n.ID, "a/src/main/java/com/acme/util") {
		t.Errorf("com.acme.util resolved to %q, want module a's directory", id)
	}
}

// Resolving an import must not touch the disk any more.
func TestJVMImportResolutionDoesNotStatPerRoot(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 50; i++ {
		os.MkdirAll(filepath.Join(tmp, "m"+string(rune('a'+i%26))+string(rune('a'+i/26)), "src/main/java/com/x"), 0o755)
	}
	c := New(tmp)
	g := graph.New("codebase:" + tmp)
	c.loadJVMRoots()
	c.jvmRootsLoaded = true
	// An external import resolves to "" purely from the index: remove the tree
	// and it must still answer (no os.Stat involved).
	os.RemoveAll(tmp)
	if got := c.resolveJVMImport(g, "java.util.List"); got != "" {
		t.Errorf("external import resolved to %q", got)
	}
}
