package codebase

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"graphallthethings/internal/graph"
)

// GitChangedFiles runs `git diff --name-status -M` between ref and the
// working tree, restricted to indexable extensions, and returns one
// FilePair per touched file: Old=="" for an added file, New=="" for a
// deleted one, both set (equal, unless git detected a rename) otherwise.
// Reusing git's own -M rename detection here means DiffCode never has to
// reinvent file-level rename heuristics — only within-file function/type
// matching is its job.
func GitChangedFiles(ctx context.Context, dir, ref string) ([]graph.FilePair, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--name-status", "-M", "--diff-filter=ACDMR", ref, "--", ".")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-status %s: %w", ref, err)
	}
	var pairs []graph.FilePair
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		status := cols[0]
		switch {
		case strings.HasPrefix(status, "R"):
			if len(cols) < 3 || !indexableExt(cols[2]) {
				continue
			}
			pairs = append(pairs, graph.FilePair{Old: cols[1], New: cols[2]})
		case status == "A":
			if !indexableExt(cols[1]) {
				continue
			}
			pairs = append(pairs, graph.FilePair{New: cols[1]})
		case status == "D":
			if !indexableExt(cols[1]) {
				continue
			}
			pairs = append(pairs, graph.FilePair{Old: cols[1]})
		default: // M
			if len(cols) < 2 || !indexableExt(cols[1]) {
				continue
			}
			pairs = append(pairs, graph.FilePair{Old: cols[1], New: cols[1]})
		}
	}
	return pairs, nil
}

// GitFileSet returns the set of relative paths git considers non-ignored
// under dir — tracked files plus untracked-but-not-ignored ones — using
// git's own .gitignore/.git/info/exclude/core.excludesfile resolution
// instead of reimplementing gitignore glob semantics. ok is false when dir
// isn't inside a git work tree (or the command fails), telling callers to
// fall back to a plain SkipDir-based walk.
//
// No .git-directory pre-check: `git -C dir` resolves the repo root by
// walking up from dir like git itself does, so this also works when dir is a
// subdirectory of a repo (e.g. `gatt extract codebase ./backend` in a
// monorepo) rather than only the checkout root — a bare os.Stat(dir/.git)
// would silently miss that case and disable gitignore filtering entirely.
//
// gatt's own `.gatt`/`gatt-out` are excluded from the result even when the
// target repo hasn't gitignored them — those are gatt-specific output, not
// the project's, and must never be treated as source regardless of the
// target's own .gitignore. Exported so non-extraction callers (e.g. the
// engine's exhaustive `grep`) apply the same exclusion without duplicating
// the git plumbing.
func GitFileSet(dir string) (map[string]bool, bool) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard").Output()
	if err != nil {
		return nil, false
	}
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if line == ".gatt" || strings.HasPrefix(line, ".gatt/") ||
			line == "gatt-out" || strings.HasPrefix(line, "gatt-out/") {
			continue
		}
		set[filepath.FromSlash(line)] = true
	}
	return set, true
}

// gitFileSet is GitFileSet(c.dir), cached on the Connector: an extract
// touches this from scanFiles, parseFiles, detectProjects, and
// loadTSConfigs, and git ls-files is not free on a very large repo.
func (c *Connector) gitFileSet() (map[string]bool, bool) {
	if !c.gitFilesKnown {
		c.gitFilesKnown = true
		c.gitFiles, c.gitFilesOK = GitFileSet(c.dir)
	}
	return c.gitFiles, c.gitFilesOK
}

// ExtractAt materializes ref into a temporary git worktree and runs a normal
// Extract against it — the "old" side of a structural diff. Self-contained:
// the worktree is removed before returning, so the caller has no cleanup to
// do.
func (c *Connector) ExtractAt(ctx context.Context, ref string) (*graph.Graph, error) {
	if _, err := os.Stat(filepath.Join(c.dir, ".git")); err != nil {
		return nil, fmt.Errorf("%s is not a git checkout", c.dir)
	}
	tmp, err := os.MkdirTemp("", "gatt-diff-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if out, err := exec.CommandContext(ctx, "git", "-C", c.dir, "worktree", "add", "--detach", tmp, ref).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	defer exec.Command("git", "-C", c.dir, "worktree", "remove", "--force", tmp).Run()

	return New(tmp).Extract(ctx)
}
