//go:build windows

package daemon

import "github.com/gurupras/shelld/internal/sock"

// DefaultDir is %TEMP%, which is already per-user.
func DefaultDir() string { return sock.DefaultDir() }

// CheckPrivateDir does nothing on Windows: the default directory is the
// user's own %TEMP%, and the pipe is protected by its security descriptor.
func CheckPrivateDir(dir string) error { return sock.CheckPrivateDir(dir) }

// EnsurePrivateDir does nothing on Windows; see CheckPrivateDir.
func EnsurePrivateDir(string) error { return nil }

// UnsafeParentWarning is empty on Windows, where the socket is a named pipe.
func UnsafeParentWarning(string) string { return "" }
