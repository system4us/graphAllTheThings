//go:build windows

package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// wrapForShellExec re-targets a .bat/.cmd shim through cmd.exe: CreateProcess
// (what os/exec ultimately calls) can't launch a .bat/.cmd as a Win32 image
// the way it can an .exe — only cmd.exe knows how to interpret one. npm's
// global installs for CLIs like `claude` leave exactly this shape behind
// (claude.cmd next to claude.ps1 and an extension-less POSIX shim), and
// exec.LookPath("claude") resolves to the .cmd via PATHEXT.
func wrapForShellExec(exePath string, args []string) (string, []string) {
	lower := strings.ToLower(exePath)
	if strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd") {
		return "cmd.exe", append([]string{"/C", exePath}, args...)
	}
	return exePath, args
}

// addToUserPath appends dir to the current user's persistent PATH
// (HKCU\Environment) if it isn't already there, then broadcasts
// WM_SETTINGCHANGE so newly spawned processes pick it up without requiring a
// logoff — existing shells (including the one that ran `gatt install`) still
// need to be restarted, same as after any `setx`.
func addToUserPath(dir string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer key.Close()

	existing, _, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("read Path: %w", err)
	}
	for _, p := range strings.Split(existing, ";") {
		if strings.EqualFold(strings.TrimSpace(p), dir) {
			return nil
		}
	}
	newPath := dir
	if existing != "" {
		newPath = existing + ";" + dir
	}
	if err := key.SetStringValue("Path", newPath); err != nil {
		return fmt.Errorf("write Path: %w", err)
	}
	broadcastEnvChange()
	return nil
}

// broadcastEnvChange tells top-level windows (Explorer included) the
// environment changed, the same signal `setx` and installers send, so newly
// launched processes see the updated PATH without a logoff/logon.
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	user32 := syscall.NewLazyDLL("user32.dll")
	sendMessageTimeout := user32.NewProc("SendMessageTimeoutW")
	envPtr, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	sendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(envPtr)), smtoAbortIfHung, 5000, 0)
}
