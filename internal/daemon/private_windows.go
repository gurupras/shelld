//go:build windows

package daemon

import "os"

// DefaultDir is %TEMP%, which is already per-user.
func DefaultDir() string { return os.TempDir() }

// CheckPrivateDir does nothing on Windows: the default directory is the
// user's own %TEMP%, and the pipe is protected by its security descriptor.
func CheckPrivateDir(string) error { return nil }

// EnsurePrivateDir does nothing on Windows; see CheckPrivateDir.
func EnsurePrivateDir(string) error { return nil }

// UnsafeParentWarning is empty on Windows, where the socket is a named pipe.
func UnsafeParentWarning(string) string { return "" }
