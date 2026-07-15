//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// wrapForShellExec is a no-op on Unix: exec can launch any executable file
// (shebang scripts included) directly, no shell wrapper needed.
func wrapForShellExec(exePath string, args []string) (string, []string) {
	return exePath, args
}

// addToUserPath appends a PATH export to the current shell's rc file if dir
// isn't already on PATH, mirroring what `agy install` does on this same
// machine (`echo 'export PATH=...' >> ~/.bashrc`). Idempotent: checks both
// the live PATH and the rc file's existing contents before writing.
func addToUserPath(dir string) error {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rc, line := rcFileAndExport(home, dir)
	if existing, _ := os.ReadFile(rc); strings.Contains(string(existing), dir) {
		return nil
	}
	if dir := filepath.Dir(rc); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n# added by `gatt install`\n" + line)
	return err
}

func rcFileAndExport(home, dir string) (rc, line string) {
	shell := os.Getenv("SHELL")
	switch {
	case strings.Contains(shell, "fish"):
		return filepath.Join(home, ".config", "fish", "config.fish"),
			fmt.Sprintf("set -gx PATH %s $PATH\n", dir)
	case strings.Contains(shell, "zsh"):
		return filepath.Join(home, ".zshrc"),
			fmt.Sprintf("export PATH=%q\n", dir+":$PATH")
	default:
		return filepath.Join(home, ".bashrc"),
			fmt.Sprintf("export PATH=%q\n", dir+":$PATH")
	}
}
