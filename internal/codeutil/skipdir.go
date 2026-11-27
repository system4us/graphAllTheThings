// Package codeutil holds codebase-specific helpers (directory skip rules,
// file mtime drift checks, and function/definition-level code diffing) that
// belong to gatt's codebase analyzer rather than the generic pkg/graph library.
package codeutil

import "strings"

// SkipDir reports whether a directory named name should be excluded from a
// codebase walk: VCS/dependency/gatt-output/build-output directories, and
// any hidden directory other than the walk root itself (isRoot lets a hidden
// root still be walked).
func SkipDir(name string, isRoot bool) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".gatt", "gatt-out",
		"dist", "build", "out", "target", "__pycache__":
		return true
	}
	return strings.HasPrefix(name, ".") && !isRoot
}
