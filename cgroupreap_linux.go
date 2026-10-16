package shelld

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/gurupras/shelld/internal/cgroup"
)

// The cgroup half of SHELLD_KILL_MODE=hangup.
//
// A hangup kill does not kill the cgroup and does not wait, so at the moment Kill
// returns the shell is usually still alive and the directory cannot be removed yet
// (rmdir of a populated cgroup is EBUSY). The choice made here: REMOVE IT AS SOON AS
// IT EMPTIES, driven by the kernel's own notification rather than a timer.
// cgroup.events generates an inotify IN_MODIFY whenever its "populated" value
// changes (Documentation/admin-guide/cgroup-v2.rst, "cgroup.events"), so one
// process-wide inotify instance with one watch per pending cgroup wakes this reaper
// exactly when a window's last member exits -- the shell, normally, a few hundred
// microseconds after the hangup; a SIGHUP-ignoring survivor, whenever it exits.
//
// Not killed, ever: a member still running here is a process tmux's kill-window
// would also have left running, and killing it would make the mode not tmux's.
//
// What is left at daemon exit: CloseCgroups first waits (bounded) for pending
// cgroups to empty, which covers the ordinary case of a shell killed just before
// shutdown; a cgroup still holding a survivor after that is left in place with the
// rest of the base, exactly as Manager.Close already does for any populated window
// cgroup, and the next daemon's GC removes it once the survivor exits. That is the
// one directory this mode can leave behind, and it only exists while a process the
// user deliberately detached from the hangup is still running in it.
//
// One instance for the process, not one per window, because inotify instances are
// capped per user (fs.inotify.max_user_instances, 128 by default) while watches are
// not meaningfully (max_user_watches).
type cgReaper struct {
	mu    sync.Mutex
	fd    int // inotify fd; -1 until started, -2 if inotify is unusable
	f     *os.File
	byWD  map[int32]*cgroup.Cgroup
	stray []*cgroup.Cgroup // no watch could be added; retried at drain
	// inflight counts cgroups handed over whose add has not run yet, so that
	// drain cannot read zero between the hand-over and the watch.
	inflight atomic.Int32
}

var hupReaper = &cgReaper{fd: -1, byWD: map[int32]*cgroup.Cgroup{}}

// reapCgroupWhenEmpty hands c to the reaper and returns at once. It does no
// filesystem work on the caller's goroutine: the watch, the first emptiness check
// and the rmdir all run on the reaper's side.
func reapCgroupWhenEmpty(c *cgroup.Cgroup) {
	if c == nil {
		return
	}
	hupReaper.inflight.Add(1)
	go hupReaper.add(c)
}

func (r *cgReaper) add(c *cgroup.Cgroup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.inflight.Add(-1)
	if r.fd == -1 {
		r.start()
	}
	if r.fd < 0 {
		r.tryStray(c)
		return
	}
	// Watch first, then check: a cgroup that empties between the two still
	// produces an event, so the check can never miss the transition.
	wd, err := unix.InotifyAddWatch(r.fd, filepath.Join(c.Dir(), "cgroup.events"), unix.IN_MODIFY)
	if err != nil {
		// ENOENT: the directory is already gone, nothing to do.
		r.tryStray(c)
		return
	}
	r.byWD[int32(wd)] = c
	r.try(int32(wd))
}

// tryStray removes c if it is empty and otherwise keeps it for the drain at exit.
func (r *cgReaper) tryStray(c *cgroup.Cgroup) {
	left, err := c.Release()
	if err == nil && left == 0 {
		return
	}
	r.stray = append(r.stray, c)
}

// try removes the cgroup behind wd if it has emptied. Caller holds r.mu.
func (r *cgReaper) try(wd int32) {
	c, ok := r.byWD[wd]
	if !ok {
		return
	}
	left, err := c.Release()
	if err != nil {
		slog.Warn("removing a hung-up window's cgroup", "dir", c.Dir(), "err", err)
		return
	}
	if left > 0 {
		return
	}
	delete(r.byWD, wd)
	// The rmdir already dropped the watch (IN_IGNORED follows); this is for the
	// case where the directory went some other way and is harmless otherwise.
	_, _ = unix.InotifyRmWatch(r.fd, uint32(wd))
}

// start opens the inotify instance and the goroutine that reads it. Caller holds
// r.mu.
func (r *cgReaper) start() {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		slog.Warn("no inotify for hung-up window cgroups; they are removed at daemon exit", "err", err)
		r.fd = -2
		return
	}
	r.fd = fd
	r.f = os.NewFile(uintptr(fd), "inotify-cgroup")
	go r.loop(r.f)
}

func (r *cgReaper) loop(f *os.File) {
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := f.Read(buf)
		if err != nil {
			return // closed by drain
		}
		r.mu.Lock()
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			r.try(ev.Wd)
			if ev.Mask&unix.IN_IGNORED != 0 {
				// The watched file is gone: the cgroup was removed (by try above,
				// or by something else). Release on a missing directory is 0, so
				// try has already settled it; drop any leftover entry.
				if c, ok := r.byWD[ev.Wd]; ok {
					delete(r.byWD, ev.Wd)
					r.stray = append(r.stray, c)
				}
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
		r.mu.Unlock()
	}
}

// drain waits up to within for every pending cgroup to be removed, then makes one
// last removal attempt at each one still pending and reports how many are left.
// CloseCgroups calls it before releasing the base.
func (r *cgReaper) drain(within time.Duration) (left int) {
	// Polled, not signalled: this runs once, at shutdown, and a 5 ms tick over
	// at most `within` is cheaper to get right than a second notification path.
	for deadline := time.Now().Add(within); r.pendingCount() > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for wd := range r.byWD {
		r.try(wd)
	}
	keep := r.stray[:0]
	for _, c := range r.stray {
		if n, err := c.Release(); err == nil && n == 0 {
			continue
		}
		keep = append(keep, c)
	}
	r.stray = keep
	return len(r.byWD) + len(r.stray)
}

// pendingCount is how many handed-over cgroups have not been removed yet.
func (r *cgReaper) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byWD) + len(r.stray) + int(r.inflight.Load())
}
