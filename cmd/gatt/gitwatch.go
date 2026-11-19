package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gitHeadSHA reads the current HEAD commit SHA of a git repository (or
// worktree) directly from .git metadata without spawning a subprocess.
// Returns "" if root is not a git repository or has no commits yet.
func gitHeadSHA(root string) string {
	gitDir, commonDir := resolveGitDirs(root)
	if gitDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if head == "" {
		return ""
	}
	if !strings.HasPrefix(head, "ref:") {
		// Detached HEAD: the file holds the commit SHA directly.
		return head
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, "ref:"))
	if ref == "" {
		return ""
	}
	// Check loose ref in gitDir, then commonDir (for worktrees).
	for _, dir := range []string{gitDir, commonDir} {
		if dir == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
			if sha := strings.TrimSpace(string(b)); sha != "" {
				return sha
			}
		}
	}
	// Fall back to packed-refs in commonDir (or gitDir).
	packedDir := commonDir
	if packedDir == "" {
		packedDir = gitDir
	}
	if sha := lookupPackedRef(filepath.Join(packedDir, "packed-refs"), ref); sha != "" {
		return sha
	}
	return ""
}

// resolveGitDirs returns (gitDir, commonDir) for a normal repo or a worktree.
func resolveGitDirs(root string) (gitDir, commonDir string) {
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", ""
	}
	if fi.IsDir() {
		return dotGit, dotGit
	}
	// Worktree or submodule: .git is a file containing "gitdir: <path>".
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", ""
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", ""
	}
	gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gd) {
		gd = filepath.Clean(filepath.Join(root, gd))
	}
	cd := gd
	if cb, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		rel := strings.TrimSpace(string(cb))
		if rel != "" {
			if filepath.IsAbs(rel) {
				cd = rel
			} else {
				cd = filepath.Clean(filepath.Join(gd, rel))
			}
		}
	}
	return gd, cd
}

func lookupPackedRef(path, ref string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0]
		}
	}
	return ""
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// watchGitCommits polls HEAD for one or more repositories at interval and
// invokes onCommit(root, newSHA) whenever HEAD advances or changes (commit,
// amend, pull, merge, rebase, checkout). Polling .git/HEAD is pure file I/O
// (no subprocesses, no inotify limits) and works across Docker bind mounts.
func watchGitCommits(ctx context.Context, interval time.Duration, roots func() []string, onCommit func(root, sha string)) {
	if interval <= 0 {
		return
	}
	last := map[string]string{}
	// Seed initial HEADs so startup doesn't double-trigger (serve already
	// triggers an initial pass when each repo is loaded).
	for _, r := range roots() {
		last[r] = gitHeadSHA(r)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, r := range roots() {
				sha := gitHeadSHA(r)
				prev, seen := last[r]
				if !seen {
					// Repo registered after watcher started: record baseline.
					last[r] = sha
					continue
				}
				if sha != "" && sha != prev {
					last[r] = sha
					fmt.Fprintf(os.Stderr, "gatt serve: commit detected in %s (%s -> %s), triggering incremental reindex\n",
						r, shortSHA(prev), shortSHA(sha))
					onCommit(r, sha)
				}
			}
		}
	}
}
