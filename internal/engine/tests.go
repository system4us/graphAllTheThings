package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/system4us/graphAllTheThings/internal/connector/codebase"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// TestRef identifies a test function affected by a change.
type TestRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
	Line string `json:"line,omitempty"`
}

// AffectedTestsResult is the structured output of AffectedTests.
type AffectedTestsResult struct {
	Target        string    `json:"target"`
	SeedNodes     []string  `json:"seed_nodes"`
	TestFiles     []string  `json:"test_files"`
	TestFunctions []TestRef `json:"test_functions"`
	Commands      []string  `json:"commands"`
	Text          string    `json:"text"`
}

// IsTestFilePath reports whether a relative file path looks like a test file
// across Go, JS/TS, Python, Rust, Java/Kotlin, and C#.
func IsTestFilePath(p string) bool {
	p = filepath.ToSlash(strings.ToLower(p))
	base := filepath.Base(p)
	if strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasSuffix(base, "test.java") ||
		strings.HasSuffix(base, "tests.java") ||
		strings.HasSuffix(base, "test.kt") ||
		strings.HasSuffix(base, "tests.cs") ||
		strings.HasSuffix(base, "_test.rs") {
		return true
	}
	if strings.Contains(p, "/__tests__/") || strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/") {
		switch filepath.Ext(base) {
		case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".rs", ".java", ".kt", ".cs", ".sh":
			return true
		}
	}
	return false
}

// AffectedTests finds the minimal set of test files, test functions, and ready-to-run
// test commands impacted by a symbol, file, or git working-tree diff (when target
// is "" or "diff:<ref>").
func (e *Engine) AffectedTests(ctx context.Context, target string, depth int) (AffectedTestsResult, error) {
	if !e.IsCodebase() {
		return AffectedTestsResult{}, fmt.Errorf("affected_tests requires a codebase graph")
	}
	if depth <= 0 {
		depth = 3
	}
	if depth > 5 {
		depth = 5
	}

	target = strings.TrimSpace(target)
	seedIDs := map[string]bool{}
	seedFiles := map[string]bool{}
	displayTarget := target

	if target == "" || strings.HasPrefix(target, "diff:") {
		ref := "HEAD"
		if strings.HasPrefix(target, "diff:") {
			if r := strings.TrimSpace(strings.TrimPrefix(target, "diff:")); r != "" {
				ref = r
			}
		}
		displayTarget = "git diff vs " + ref
		dir := strings.TrimPrefix(e.G.Source, "codebase:")
		pairs, err := codebase.GitChangedFiles(ctx, dir, ref)
		if err != nil {
			return AffectedTestsResult{}, fmt.Errorf("git diff %s: %w", ref, err)
		}
		changedSet := map[string]bool{}
		for _, p := range pairs {
			if p.New != "" {
				changedSet[filepath.ToSlash(p.New)] = true
			}
		}
		for _, n := range e.G.Nodes {
			if n.Attrs["external"] == "true" {
				continue
			}
			f := filepath.ToSlash(n.Attrs["file"])
			if f == "" && n.Type == graph.NodeFile {
				f = filepath.ToSlash(n.Name)
			}
			// A prose doc edit runs no tests: seeding it pulled in whatever
			// tests the doc historically co-changed with (docs/CONFIG.md →
			// the config package's tests).
			if changedSet[f] && !isProseDoc(f) {
				seedIDs[n.ID] = true
				seedFiles[f] = true
			}
		}
	} else {
		// Match exact ID, file path, or symbol name.
		if n := e.G.Nodes[target]; n != nil {
			seedIDs[n.ID] = true
			if f := n.Attrs["file"]; f != "" {
				seedFiles[filepath.ToSlash(f)] = true
			} else if n.Type == graph.NodeFile {
				seedFiles[filepath.ToSlash(n.Name)] = true
			}
		} else {
			norm := filepath.ToSlash(target)
			for _, n := range e.G.Nodes {
				if n.Attrs["external"] == "true" {
					continue
				}
				if n.Name == target || filepath.ToSlash(n.Name) == norm || strings.HasSuffix(filepath.ToSlash(n.Name), "/"+norm) {
					seedIDs[n.ID] = true
					if f := n.Attrs["file"]; f != "" {
						seedFiles[filepath.ToSlash(f)] = true
					} else if n.Type == graph.NodeFile {
						seedFiles[filepath.ToSlash(n.Name)] = true
					}
				}
			}
		}
	}

	// A file target means everything declared in it, as in diff mode: its
	// functions' callers in other packages are affected too, not only the
	// file's own directory.
	fileTargets := map[string]bool{}
	if target != "" && !strings.HasPrefix(target, "diff:") {
		for id := range seedIDs {
			if n := e.G.Nodes[id]; n != nil && n.Type == graph.NodeFile && n.Attrs["path"] != "" {
				fileTargets[filepath.ToSlash(n.Name)] = true
			}
		}
	}
	for f := range fileTargets {
		for _, n := range e.G.Nodes {
			if n.Attrs["external"] != "true" && filepath.ToSlash(n.Attrs["file"]) == f {
				seedIDs[n.ID] = true
			}
		}
	}

	if len(seedIDs) == 0 {
		return AffectedTestsResult{
			Target: displayTarget,
			Text:   fmt.Sprintf("no matching nodes found for %q\n", displayTarget),
		}, nil
	}

	// Reverse dependency BFS to collect impacted nodes & files.
	type qItem struct {
		id   string
		dist int
	}
	visited := map[string]bool{}
	var queue []qItem
	for id := range seedIDs {
		visited[id] = true
		queue = append(queue, qItem{id: id, dist: 0})
	}
	impactedDirs := map[string]bool{}
	for f := range seedFiles {
		impactedDirs[filepath.ToSlash(filepath.Dir(f))] = true
	}

	testFilesSet := map[string]bool{}
	testFnMap := map[string]TestRef{}

	recordIfTest := func(n *graph.Node) {
		f := filepath.ToSlash(n.Attrs["file"])
		if f == "" && n.Type == graph.NodeFile {
			f = filepath.ToSlash(n.Name)
		}
		if f == "" || !IsTestFilePath(f) {
			return
		}
		testFilesSet[f] = true
		if n.Type == graph.NodeFunction && (strings.HasPrefix(n.Name, "Test") || strings.HasPrefix(n.Name, "Benchmark") || strings.HasPrefix(strings.ToLower(n.Name), "test")) {
			testFnMap[n.ID] = TestRef{
				ID:   n.ID,
				Name: n.Name,
				File: f,
				Line: n.Attrs["line_start"],
			}
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cn := e.G.Nodes[cur.id]; cn != nil {
			recordIfTest(cn)
			if f := cn.Attrs["file"]; f != "" {
				impactedDirs[filepath.ToSlash(filepath.Dir(f))] = true
			} else if cn.Type == graph.NodeFile {
				impactedDirs[filepath.ToSlash(filepath.Dir(cn.Name))] = true
			}
		}
		if cur.dist >= depth {
			continue
		}
		for _, ed := range e.G.EdgesOf(cur.id) {
			var nextID string
			switch ed.Type {
			case graph.EdgeCalls, graph.EdgeImports, graph.EdgeDependsOn,
				graph.EdgeHandledBy, graph.EdgeUsesMiddleware, graph.EdgeUsesModel,
				graph.EdgeHasMethod, graph.EdgeReferences:
				if ed.To == cur.id {
					nextID = ed.From
				}
			case graph.EdgeGenerates:
				if ed.From == cur.id {
					nextID = ed.To
				}
			case graph.EdgeCoChanged:
				// Only record direct co-change test files at depth 0 without marking
				// their parent directory (e.g. tests/) as impacted, so a single co-change
				// with tests/check.sh doesn't drag in every unrelated file under tests/.
				if cur.dist == 0 {
					cand := ed.To
					if cand == cur.id {
						cand = ed.From
					}
					if cn := e.G.Nodes[cand]; cn != nil && cn.Type == graph.NodeFile && IsTestFilePath(cn.Name) {
						recordIfTest(cn)
					}
				}
			}
			if nextID == "" || visited[nextID] {
				continue
			}
			nn := e.G.Nodes[nextID]
			if nn == nil || nn.Attrs["external"] == "true" {
				continue
			}
			visited[nextID] = true
			queue = append(queue, qItem{id: nextID, dist: cur.dist + 1})
		}
	}

	// Include sibling test files in the same package/directory as directly seeded files
	// (e.g., Go package tests in the same dir don't need an explicit import edge).
	for _, f := range e.G.NodesByType(graph.NodeFile) {
		if f.Attrs["external"] == "true" {
			continue
		}
		p := filepath.ToSlash(f.Name)
		if !IsTestFilePath(p) {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(p))
		if impactedDirs[dir] {
			testFilesSet[p] = true
		}
	}

	testFiles := make([]string, 0, len(testFilesSet))
	for f := range testFilesSet {
		testFiles = append(testFiles, f)
	}
	sort.Strings(testFiles)

	testFns := make([]TestRef, 0, len(testFnMap))
	for _, tf := range testFnMap {
		testFns = append(testFns, tf)
	}
	sort.Slice(testFns, func(i, j int) bool {
		if testFns[i].File != testFns[j].File {
			return testFns[i].File < testFns[j].File
		}
		return testFns[i].Name < testFns[j].Name
	})

	cmds := buildTestCommands(testFiles, testFns)

	seeds := make([]string, 0, len(seedIDs))
	for id := range seedIDs {
		seeds = append(seeds, id)
	}
	sort.Strings(seeds)

	var b strings.Builder
	fmt.Fprintf(&b, "affected tests for %s (%d test file(s), %d direct test function(s))\n", displayTarget, len(testFiles), len(testFns))
	if len(cmds) > 0 {
		b.WriteString("\nrecommended command(s):\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	if len(testFns) > 0 {
		b.WriteString("\ndirect test functions:\n")
		for _, tf := range testFns {
			fmt.Fprintf(&b, "  %s (%s:%s)\n", tf.Name, tf.File, tf.Line)
		}
	}
	if len(testFiles) > 0 {
		b.WriteString("\ntest files:\n")
		for _, f := range testFiles {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	} else {
		b.WriteString("no test files detected for this target\n")
	}

	return AffectedTestsResult{
		Target:        displayTarget,
		SeedNodes:     seeds,
		TestFiles:     testFiles,
		TestFunctions: testFns,
		Commands:      cmds,
		Text:          b.String(),
	}, nil
}

func buildTestCommands(files []string, fns []TestRef) []string {
	var cmds []string
	goPkgs := map[string]bool{}
	var tsFiles, pyFiles []string
	hasRust := false

	for _, f := range files {
		lf := strings.ToLower(f)
		switch {
		case strings.HasSuffix(lf, "_test.go"):
			dir := filepath.ToSlash(filepath.Dir(f))
			if dir == "." {
				goPkgs["./"] = true
			} else {
				goPkgs["./"+dir] = true
			}
		case strings.HasSuffix(lf, ".ts") || strings.HasSuffix(lf, ".tsx") || strings.HasSuffix(lf, ".js") || strings.HasSuffix(lf, ".jsx"):
			tsFiles = append(tsFiles, f)
		case strings.HasSuffix(lf, ".py"):
			pyFiles = append(pyFiles, f)
		case strings.HasSuffix(lf, ".rs"):
			hasRust = true
		}
	}

	if len(goPkgs) > 0 {
		pkgs := make([]string, 0, len(goPkgs))
		for p := range goPkgs {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		cmd := "go test " + strings.Join(pkgs, " ")
		var goFns []string
		for _, tf := range fns {
			if strings.HasSuffix(tf.File, "_test.go") && strings.HasPrefix(tf.Name, "Test") {
				goFns = append(goFns, tf.Name)
			}
		}
		if len(goFns) > 0 && len(goFns) <= 8 {
			cmd += " -run '^(" + strings.Join(goFns, "|") + ")$'"
		}
		cmds = append(cmds, cmd)
	}
	if len(tsFiles) > 0 {
		shown := tsFiles
		if len(shown) > 10 {
			shown = shown[:10]
		}
		cmds = append(cmds, "npx vitest run "+strings.Join(shown, " "))
	}
	if len(pyFiles) > 0 {
		shown := pyFiles
		if len(shown) > 10 {
			shown = shown[:10]
		}
		cmds = append(cmds, "pytest "+strings.Join(shown, " "))
	}
	if hasRust {
		cmds = append(cmds, "cargo test")
	}
	return cmds
}
