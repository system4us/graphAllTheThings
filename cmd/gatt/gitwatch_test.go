package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWatchGitCommitsDetectsNewCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "a.go")
	runGit(t, repo, "commit", "-m", "initial")

	sha1 := runGit(t, repo, "rev-parse", "HEAD")
	if got := gitHeadSHA(repo); got != sha1 {
		t.Fatalf("gitHeadSHA = %q, want %q", got, sha1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var triggered atomic.Int32
	var gotSHA atomic.Value
	go watchGitCommits(ctx, 25*time.Millisecond, func() []string { return []string{repo} }, func(root, sha string) {
		if root == repo {
			gotSHA.Store(sha)
			triggered.Add(1)
		}
	})

	// No new commit yet -> should not trigger.
	time.Sleep(80 * time.Millisecond)
	if triggered.Load() != 0 {
		t.Fatalf("triggered %d times before any new commit", triggered.Load())
	}

	// Make a second commit -> watcher must detect it and fire once.
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "a.go")
	runGit(t, repo, "commit", "-m", "second")
	sha2 := runGit(t, repo, "rev-parse", "HEAD")

	waitFor(t, func() bool { return triggered.Load() == 1 })
	if got, _ := gotSHA.Load().(string); got != sha2 {
		t.Errorf("detected SHA = %q, want %q", got, sha2)
	}
}

func TestGitHeadSHANonGitDir(t *testing.T) {
	if got := gitHeadSHA(t.TempDir()); got != "" {
		t.Errorf("non-git dir returned %q, want empty", got)
	}
}
