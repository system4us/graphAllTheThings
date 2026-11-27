package engine

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// CommitEntry is a compact git commit record associated with a file.
type CommitEntry struct {
	SHA     string   `json:"sha"`
	Date    string   `json:"date"`
	Author  string   `json:"author"`
	Subject string   `json:"subject"`
	Issues  []string `json:"issues,omitempty"`
}

// HotspotEntry ranks files by churn (recent commit count) × graph connectivity.
type HotspotEntry struct {
	NodeID  string `json:"node_id"`
	Path    string `json:"path"`
	Commits int    `json:"commits"`
	Degree  int    `json:"degree"`
	Score   int    `json:"score"`
}

var issueRefRe = regexp.MustCompile(`(?:\b[A-Z]{2,10}-\d+\b|#\d+\b)`)

func extractIssueRefs(subject string) []string {
	matches := issueRefRe.FindAllString(subject, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// FileHistory returns up to limit recent git commits touching relPath, with any
// issue references (#42, PROJ-123) extracted from commit subjects.
func (e *Engine) FileHistory(ctx context.Context, relPath string, limit int) []CommitEntry {
	if !e.IsCodebase() || relPath == "" {
		return nil
	}
	if limit <= 0 {
		limit = 8
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log",
		fmt.Sprintf("-n%d", limit),
		"--format=%h\t%cs\t%an\t%s",
		"--", relPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var res []CommitEntry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		res = append(res, CommitEntry{
			SHA:     parts[0],
			Date:    parts[1],
			Author:  parts[2],
			Subject: parts[3],
			Issues:  extractIssueRefs(parts[3]),
		})
	}
	return res
}

// FileGitDiff returns the unified git diff for relPath against ref (default HEAD).
func (e *Engine) FileGitDiff(ctx context.Context, relPath, ref string) (string, error) {
	if !e.IsCodebase() || relPath == "" {
		return "", fmt.Errorf("file diff requires a codebase graph and file path")
	}
	if ref == "" {
		ref = "HEAD"
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--unified=3", ref, "--", relPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	text := string(out)
	if len(text) > 24*1024 {
		text = text[:24*1024] + "\n… (diff truncated)\n"
	}
	return text, nil
}

// Hotspots ranks codebase files by recent git churn × graph connectivity (degree),
// surfacing risky architectural hubs that change frequently.
func (e *Engine) Hotspots(ctx context.Context, limit int) ([]HotspotEntry, error) {
	if !e.IsCodebase() {
		return nil, fmt.Errorf("hotspots requires a codebase graph")
	}
	if limit <= 0 {
		limit = 25
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "-n200", "--name-only", "--pretty=format:")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	churn := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		p := filepath.ToSlash(strings.TrimSpace(line))
		if p != "" {
			churn[p]++
		}
	}

	var entries []HotspotEntry
	for _, f := range e.G.NodesByType(graph.NodeFile) {
		if f.Attrs["external"] == "true" {
			continue
		}
		p := filepath.ToSlash(f.Name)
		switch strings.ToLower(filepath.Ext(p)) {
		case ".md", ".mdx", ".rst", ".txt":
			continue
		}
		c := churn[p]
		if c == 0 {
			continue
		}
		deg := len(e.G.EdgesOf(f.ID))
		entries = append(entries, HotspotEntry{
			NodeID:  f.ID,
			Path:    p,
			Commits: c,
			Degree:  deg,
			Score:   c * (deg + 1),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		return entries[i].Path < entries[j].Path
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// RenderHotspots formats hotspot entries into a compact agent-readable table.
func RenderHotspots(entries []HotspotEntry) string {
	if len(entries) == 0 {
		return "no hotspots found (no recent git churn matching graph files)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "top %d hotspot(s) (recent git churn × graph connectivity):\n", len(entries))
	for i, h := range entries {
		fmt.Fprintf(&b, "  %2d. %s  commits=%d degree=%d score=%d\n", i+1, h.Path, h.Commits, h.Degree, h.Score)
	}
	return b.String()
}

// RenderFileHistory formats recent commits for a file into a compact text list.
func RenderFileHistory(relPath string, entries []CommitEntry) string {
	if len(entries) == 0 {
		return fmt.Sprintf("no git history found for %s\n", relPath)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "recent git history for %s (%d commit(s)):\n", relPath, len(entries))
	for _, c := range entries {
		issues := ""
		if len(c.Issues) > 0 {
			issues = " [" + strings.Join(c.Issues, ", ") + "]"
		}
		fmt.Fprintf(&b, "  %s %s (%s): %s%s\n", c.SHA, c.Date, c.Author, c.Subject, issues)
	}
	return b.String()
}
