//go:build !linux

package cgroup

import (
	"os"
	"time"
)

// The non-Linux build is a shape, not a feature: cgroups are a Linux interface, and
// on every other platform the /proc union — or, on Windows, a job object, which is
// a separate question — is the whole of the answer. Probe declines, and the methods
// exist only so the caller compiles without build tags around every call.

// Manager is the placeholder for a platform with no cgroups.
type Manager struct{}

// Probe always declines here.
func Probe() (*Manager, error) { return nil, ErrUnsupported }

// Base is empty.
func (m *Manager) Base() string { return "" }

// New always fails.
func (m *Manager) New(id uint64) (*Cgroup, error) { return nil, ErrUnsupported }

// GC has nothing to collect.
func (m *Manager) GC() (removed, busy int, err error) { return 0, 0, nil }

// Close has nothing to remove.
func (m *Manager) Close() error { return nil }

// Cgroup is the placeholder for one window's cgroup.
type Cgroup struct{}

// Dir is empty.
func (c *Cgroup) Dir() string { return "" }

// Open always fails.
func (c *Cgroup) Open() (*os.File, error) { return nil, ErrUnsupported }

// Kill always fails.
func (c *Cgroup) Kill() error { return ErrUnsupported }

// WaitEmpty always fails.
func (c *Cgroup) WaitEmpty(within time.Duration) error { return ErrUnsupported }

// Release always fails.
func (c *Cgroup) Release() (int, error) { return 0, ErrUnsupported }

// Remove always fails.
func (c *Cgroup) Remove() error { return ErrUnsupported }
