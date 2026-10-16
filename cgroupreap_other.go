//go:build !linux

package shelld

import (
	"time"

	"github.com/gurupras/shelld/internal/cgroup"
)

// No cgroup arm off Linux: windows never have a cgroup here, so there is nothing
// to reap. See cgroupreap_linux.go.
type cgReaper struct{}

var hupReaper = &cgReaper{}

func reapCgroupWhenEmpty(c *cgroup.Cgroup) {
	if c != nil {
		_, _ = c.Release()
	}
}

func (r *cgReaper) drain(within time.Duration) int { return 0 }

func (r *cgReaper) pendingCount() int { return 0 }
