//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gurupras/shelld/proctree"
)

// mountPoint is where cgroup v2 is mounted on a systemd host. It is checked with
// statfs rather than trusted: a v1 or hybrid host has something else there, and
// every operation in this package is v2-only.
const mountPoint = "/sys/fs/cgroup"

// namePrefix marks a base directory as ours. A base is namePrefix + pid + "-" +
// the daemon's start time in clock ticks, and the start time is there for the same
// reason KillTree takes one: a pid is not a process. pid_max wraps, so a bare
// shelld-<pid> is a name a later, unrelated daemon can be handed by the kernel, and
// a GC keyed on it would be deciding the fate of a directory belonging to whoever
// holds the number now. With the start time, "is this daemon alive" is answerable
// exactly rather than probably.
//
// This is also the one place the start time leaves memory, and it is worth saying so
// here rather than only in proctree, because a reader of StartTicks sees an opaque
// in-memory scalar and this is the site that makes it durable text. It is published
// here, and what keeps that exact is that ticks are boot-relative and so
// is this filesystem: a cgroup2 directory cannot outlive the kernel that ticks are
// counted from, so the names this GC reads are always from the boot their ticks
// belong to. Probe checks the filesystem really is cgroup2 rather than trusting the
// path, which is what makes that a fact about the mount and not about the constant.
//
// Where it does NOT bear: the accuracy of the identity test is what rests on this,
// not the safety of the GC. A stale name that matched a live process by coincidence
// would make the GC SKIP that directory — the comparison's true branch is `continue`
// — so the failure is a leaked directory, never an action against a live daemon.
// See GC, which owns the safety argument proper: it kills nothing, and removeIfEmpty
// refuses any base that cgroup.events still reports as populated. A scheme that
// wrote these names somewhere durable would need a boot id beside the ticks to keep
// the test exact; it would not thereby become destructive.
const namePrefix = "shelld-"

// baseName is the directory name for one daemon.
func baseName(pid int, start uint64) string {
	return namePrefix + strconv.Itoa(pid) + "-" + strconv.FormatUint(start, 10)
}

// parseBaseName splits a base directory name back into the daemon it names. The
// split is on the LAST '-' because the prefix contains one.
func parseBaseName(name string) (pid int, start uint64, ok bool) {
	rest, cut := strings.CutPrefix(name, namePrefix)
	if !cut {
		return 0, 0, false
	}
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 || i == len(rest)-1 {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(rest[:i])
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	start, err = strconv.ParseUint(rest[i+1:], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pid, start, true
}

// Manager owns one per-daemon base directory, with one child cgroup per window.
type Manager struct {
	// base is the daemon's own directory, named by baseName: pid and start time.
	base string
	// parent is the cgroup the daemon itself is in, and so the directory our base
	// and any stale sibling base live under.
	parent string
}

// Probe returns a Manager or the reason there is none. Every failure here is a
// decline rather than a fault: the caller keeps the /proc union and runs without
// the membership arm.
//
// The checks are in the order of what they can rule out cheaply, and the last of
// them is a real spawn. That is not belt-and-braces — the two failures that matter
// most are invisible until fork. A CgroupFD that is not a cgroup directory is not
// rejected when the fd is opened, only at clone3 (EBADF), and a cgroup removed
// after the fd was opened gives ENOENT there too. Nor is there a fallback to fall
// back on: Go retries a clone3 failure with clone only when UseCgroupFD is unset,
// so on a kernel or under a seccomp policy without CLONE_INTO_CGROUP every window
// spawn would fail outright. A probe that answered from file permissions alone
// would be answering a different question.
func Probe() (*Manager, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(mountPoint, &st); err != nil {
		return nil, fmt.Errorf("cgroup: statfs %s: %w", mountPoint, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, fmt.Errorf("cgroup: %s is not cgroup v2 (fs type %#x): %w",
			mountPoint, st.Type, ErrUnsupported)
	}
	own, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	// Our own start time, read from our own /proc entry, which cannot be a different
	// process. Without it there is no name that distinguishes this daemon from a dead
	// one that held the same pid, so declining is the honest answer rather than
	// falling back to a name that lies.
	start, ok := proctree.StartTicks(os.Getpid())
	if !ok {
		return nil, fmt.Errorf("cgroup: cannot read this process's start time: %w", ErrUnsupported)
	}
	m := &Manager{
		parent: filepath.Join(mountPoint, own),
		base:   filepath.Join(mountPoint, own, baseName(os.Getpid(), start)),
	}
	// ErrExist is not a conflict here: pid AND start time together name one process,
	// and that process is this one. Reaching it means Probe ran twice.
	if err := os.Mkdir(m.base, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("cgroup: mkdir %s: %w", m.base, err)
	}
	if err := m.verify(); err != nil {
		_, _ = removeIfEmpty(m.base)
		return nil, err
	}
	return m, nil
}

// Base is the daemon's own cgroup directory.
func (m *Manager) Base() string { return m.base }

// verify creates a child, checks the two files a close will need, and spawns a
// real process into it.
func (m *Manager) verify() error {
	probe := filepath.Join(m.base, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cgroup: mkdir %s: %w", probe, err)
	}
	defer func() { _, _ = removeIfEmpty(probe) }()

	if _, err := os.Stat(filepath.Join(probe, "cgroup.kill")); err != nil {
		return fmt.Errorf("cgroup: no cgroup.kill in a cgroup we created: %w", err)
	}
	// Opened for WRITE, and read by nobody: the hostile shape measured on this box
	// is a session scope owned by uid 0 mode 755, one directory away from the
	// friendly case, where every read succeeds and both the mkdir above and this
	// open are EACCES. A readability check would pass there and every window would
	// then fail to spawn.
	f, err := os.OpenFile(filepath.Join(probe, "cgroup.procs"), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("cgroup: cgroup.procs is not writable: %w", err)
	}
	f.Close()

	exe, err := lookTrue()
	if err != nil {
		return err
	}
	dir, err := os.Open(probe)
	if err != nil {
		return fmt.Errorf("cgroup: open %s: %w", probe, err)
	}
	// A plain open, not O_PATH: CLONE_INTO_CGROUP needs the kernfs file behind the
	// fd, and an O_PATH descriptor has no file operations for clone3 to use.
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{
		Sys: &unix.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())},
	})
	dir.Close()
	if err != nil {
		return fmt.Errorf("cgroup: spawning %s into %s: %w", exe, probe, err)
	}
	// Success here IS placement, and that is a property of the flag rather than an
	// assumption: CLONE_INTO_CGROUP has no mode in which the child is created and
	// put somewhere else. What it does not tell us is anything about a child that
	// outlives its spawn, which is why the membership assertion lives in the tests,
	// against a process that can be held still.
	st, err := proc.Wait()
	if err != nil {
		return fmt.Errorf("cgroup: waiting for the probe child: %w", err)
	}
	if !st.Exited() || st.ExitCode() != 0 {
		return fmt.Errorf("cgroup: probe child ended as %v, want exit 0: %w", st, ErrUnsupported)
	}
	return nil
}

// lookTrue finds a program that exits 0 and reads nothing. The probe needs a
// subject, not a shell: a shell would need an environment and a working directory
// this package has no business choosing.
func lookTrue() (string, error) {
	for _, p := range []string{"/bin/true", "/usr/bin/true"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("cgroup: no /bin/true to probe with: %w", ErrUnsupported)
}

// ownCgroup reads the daemon's own v2 path from /proc/self/cgroup. The v2 line is
// the one with hierarchy id 0 and no controller name; a host still running v1
// controllers has other lines, and one running v1 alone has no such line at all.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup: read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			rest = strings.TrimSpace(rest)
			// A daemon in the root cgroup would put its base directly under the
			// mount point. Nothing forbids it, and the path join below is the same.
			if rest == "" {
				return "", fmt.Errorf("cgroup: empty v2 path in /proc/self/cgroup: %w", ErrUnsupported)
			}
			return rest, nil
		}
	}
	return "", fmt.Errorf("cgroup: no v2 line in /proc/self/cgroup: %w", ErrUnsupported)
}

// New makes the cgroup for one window. The argument is a number and not the
// window's name on purpose: a window name is user input and this is a filesystem
// path.
func (m *Manager) New(id uint64) (*Cgroup, error) {
	dir := filepath.Join(m.base, "win-"+strconv.FormatUint(id, 10))
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("cgroup: mkdir %s: %w", dir, err)
		}
		// The base is unique to this process, and window ids do not repeat within one
		// daemon, so an existing directory here is one we made and did not remove. An
		// empty one is reused after removing it, which keeps New idempotent; a
		// POPULATED one is refused rather than adopted, because taking it would make
		// this window responsible for killing processes it never started — and this
		// package does not kill on a guess.
		left, err := removeIfEmpty(dir)
		if err != nil {
			return nil, fmt.Errorf("cgroup: %s already exists and will not clear: %w", dir, err)
		}
		if left > 0 {
			return nil, fmt.Errorf("cgroup: %s already exists and holds a process", dir)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cgroup: mkdir %s after clearing it: %w", dir, err)
		}
	}
	return &Cgroup{dir: dir, base: m.base}, nil
}

// GC removes the directories left by daemons that are gone. It KILLS NOTHING, and
// that is the design rather than a shortcoming.
//
// The temptation is to write cgroup.kill into a stale base: a crashed daemon left
// windows whose PTY master is closed and whose shells no client can ever reach
// again, which reads like garbage. But a cgroup does not distinguish those from a
// job the user deliberately detached, and nobody asked for either to die. A window
// close kills a tree because a user named that window; a GC that killed would be
// SIGKILLing processes on the strength of a directory name. So a stale base with
// anything alive in it is LEFT, counted, and reported to the caller — a GC at some
// later daemon's start gets it once those processes exit on their own.
//
// Staleness is decided on pid AND start time, both taken from the directory's name:
// if the pid is gone, or holds a process that started at a different time, the
// daemon that made the directory is gone. A base whose (pid, start) still matches a
// live process is skipped, which includes our own.
//
// "Gone" means the platform SAID so, and the distinction is the whole of baseGone.
// An earlier version read `if live, ok := StartTicks(pid); ok && live == start`, so
// every path where the read merely failed fell through to the removal — and a read
// can fail on a live daemon. The case to cite is a descriptor shortage: at EMFILE the
// open fails while stat, which needs no descriptor, still answers, so baseGone sees a
// present entry and an unreadable one and says "cannot tell". That case is handled by
// construction rather than by a value this comment has to get right.
//
// hidepid was cited here and should not have been, because whether it supports the fix
// or defeats it depends on the value, which makes it the wrong example either way. At
// hidepid=1 another uid's entry is LISTED and stat succeeds while the read fails, so
// the fix applies — a supporting case. At hidepid=2 the kernel's stated intent is that
// the entry is not visible at all, which would make stat ENOENT for a LIVE peer and
// this function answer "gone"; the /proc/self control does not help, since our own
// entry is always visible. That would be a residual, not a handled case. It is
// unverified here rather than either: this box mounts /proc with no hidepid option,
// remounting a shared machine's /proc is not acceptable to find out, and building a
// private hidepid mount in a user namespace is refused
// (kernel.apparmor_restrict_unprivileged_userns=1). Reaching it also needs two daemons
// as different uids sharing one parent cgroup, and m.parent is the daemon's own
// cgroup. Do not promote it to either column without measuring it.
//
// What a wrong answer costs, and so why the bias runs this way: a LIVE peer's base is
// empty whenever that peer is
// between windows, so removeIfEmpty would take it, and the peer holds no descriptor
// to it: its next Manager.New mkdirs under a parent whose base no longer exists and
// fails ENOENT, for every window it opens from then on. Every one of those windows
// spawns with no cgroup, which means no CLONE_INTO_CGROUP and no cgroup.kill — and
// that is the only arm that reaches a process which has both setsid'd away and been
// orphaned (see shelld's reapCgroup). A GC that cannot tell now leaves the
// directory; a later GC, or the peer's own Close, gets it.
//
// "A later GC" is a later DAEMON's, and there is no series of them inside one process
// to wait for: this runs once per daemon start, from InitCgroups' single call site,
// which itself returns early once the manager exists. Nothing sweeps on a timer. So
// the collecting opportunities are daemon starts, and the gap is not bounded by
// anything about
// shelld. Pids are recycled, so a base whose daemon really did die can name a live
// process that has nothing to do with this daemon, and if that process is one whose
// start time cannot be read, every GC answers "cannot tell" until it exits — its
// lifetime, not ours, is the wait. The two collecting conditions are the read
// starting to succeed and /proc/<pid> going away, and neither is on a schedule.
// What waits is one empty directory on cgroup2fs holding no process, which is why
// the wait is acceptable and the bias stays where it is: the cost of leaving a
// collectable base is a directory, and the cost of removing a live one is every
// window that peer opens afterwards.
func (m *Manager) GC() (removed, busy int, err error) {
	ents, err := os.ReadDir(m.parent)
	if err != nil {
		return 0, 0, fmt.Errorf("cgroup: read %s: %w", m.parent, err)
	}
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, start, ok := parseBaseName(e.Name())
		if !ok {
			continue
		}
		if !baseGone(pid, start) {
			continue
		}
		left, err := removeIfEmpty(filepath.Join(m.parent, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if left > 0 {
			busy++
			continue
		}
		removed++
	}
	return removed, busy, errors.Join(errs...)
}

// A seam, because the branch baseGone below exists for is the one where the read
// FAILS against a live process, and nothing a test can do to a real pid produces
// that: /proc/<pid>/stat is readable for every process a test can name, zombies
// included. Only the pid is substituted for, so the existence check underneath
// still runs against real /proc.
var startTicks = proctree.StartTicks

// procDir is the procfs baseGone stats. A seam for the same reason as startTicks and
// no more: what the control in baseGone guards against is a procfs that answers for
// nothing, and a test cannot unmount the real one. Both stats go through it, so a
// test pointing it at a directory cannot accidentally leave the control reading a
// /proc that is healthy while the pid lookup reads one that is not.
var procDir = "/proc"

// baseGone reports whether the daemon that owns a base named (pid, start) is
// definitely gone. It answers on positive evidence only, because the caller acts by
// removing a directory a live peer needs.
//
// Two things count as evidence. The pid resolves to a process that started at a
// different time, so the name's owner has been reaped and its number handed out
// again. Or /proc has no entry for the pid at all, which is the ordinary case this
// GC exists for — a daemon that exited.
//
// Everything else is "cannot tell", and that includes the case the old code read as
// gone: StartTicks answering false with /proc/<pid> still present. The existence
// check is what separates them, and it has to be a separate syscall because
// StartTicks collapses both into one bool — the ENOENT this wants and the parse or
// descriptor failure it must not act on.
//
// The ENOENT needs a control, which is the /proc/self stat. An absent entry is
// evidence about the pid only if this /proc answers for a pid that certainly exists;
// against a /proc that is not mounted, or an empty directory standing where it should
// be, EVERY pid stats ENOENT and this function would report every base gone — every
// live peer's at once, which is the failure the paragraph in GC describes arriving on
// one peer, arriving on all of them. That is the shape this tree already asks for
// elsewhere: a count is not evidence without a liveness field beside it, and here the
// count is a negative answer, where an absent instrument reads exactly like a healthy
// one. Cost is one extra syscall, and only on the path that has already decided the
// pid looks gone.
//
// /proc/self specifically, because it is the one entry whose existence does not depend
// on the caller's permissions or on another process still being alive. Note what it
// does NOT cover: a procfs that answers for us and hides other uids. See GC's note on
// hidepid.
func baseGone(pid int, start uint64) bool {
	if live, ok := startTicks(pid); ok {
		return live != start
	}
	if _, err := os.Stat(filepath.Join(procDir, strconv.Itoa(pid))); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	// The control described above: without it an unusable /proc answers ENOENT for
	// every pid and this returns true for every base, live peers included.
	if _, err := os.Stat(filepath.Join(procDir, "self")); err != nil {
		return false
	}
	return true
}

// Close removes the daemon's base, and like GC it kills nothing: a window cgroup
// still populated at shutdown holds either a window the caller never closed or a job
// that outlived its shell, and neither is something shutdown should decide about. The
// directory is left for the next daemon's GC, and the error says so.
func (m *Manager) Close() error {
	left, err := removeIfEmpty(m.base)
	if err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("cgroup: %s left in place: %d cgroup(s) under %s still hold a process",
			m.base, left, m.base)
	}
	return nil
}

// Cgroup is one window's cgroup.
type Cgroup struct {
	dir  string
	base string
}

// Dir is the cgroup's path.
func (c *Cgroup) Dir() string { return c.dir }

// Open returns a directory handle for SysProcAttr.CgroupFD. It is opened per spawn
// and never cached: an fd for a cgroup that has since been removed is accepted by
// open and fails at clone3 with ENOENT, so a cached fd turns a teardown race into a
// spawn failure.
func (c *Cgroup) Open() (*os.File, error) {
	f, err := os.Open(c.dir)
	if err != nil {
		return nil, fmt.Errorf("cgroup: open %s: %w", c.dir, err)
	}
	return f, nil
}

// Kill SIGKILLs every member, including members in nested cgroups: cgroup.kill
// descends, which is what makes a window's own `mkdir` under itself nesting rather
// than escape.
func (c *Cgroup) Kill() error {
	if err := c.guard(); err != nil {
		return err
	}
	return kill(c.dir)
}

// WaitEmpty waits for the cgroup to report no members. It is the step between
// cgroup.kill and rmdir, and it is not optional: an rmdir 0.1 ms after the kill was
// measured EBUSY, while `populated` was already 0 by 0.3 ms. The processes are dead
// either way; what the wait is for is the directory.
func (c *Cgroup) WaitEmpty(within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		pop, err := populated(c.dir)
		if err != nil {
			return err
		}
		if !pop {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("cgroup: %s still populated after %v", c.dir, within)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// Release deletes the cgroup if nothing is in it, deepest first, and KILLS
// NOTHING. It reports how many cgroups were left in place: 0 once the directory is
// gone, and otherwise one per cgroup that still holds a process, counting this one
// and any nested under it. Depth-first is the only order that works, because a
// nested cgroup makes rmdir of the parent EBUSY whether or not anything is in it.
//
// A non-zero count is an answer, not an error. It is this package's standing policy
// that a cgroup is killed only when a user named the window — GC and Close leave a
// populated directory for the same reason — so a caller tearing a window down
// because its terminal reached EOF, rather than because it was asked to, gets the
// count and leaves the survivors running.
//
// An already-removed directory reports 0 and no error, so calling this twice, or
// racing another path that removes the same cgroup, is not a failure.
func (c *Cgroup) Release() (left int, err error) {
	if err := c.guard(); err != nil {
		return 0, err
	}
	return removeIfEmpty(c.dir)
}

// Remove deletes the cgroup and requires it to be empty: it is Release plus the
// expectation of the caller that has already killed. It runs after Kill and
// WaitEmpty, so anything still populated here is a fault worth reporting rather
// than a state to work around.
func (c *Cgroup) Remove() error {
	left, err := c.Release()
	if err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("cgroup: %s still holds a process after Kill and WaitEmpty", c.dir)
	}
	return nil
}

// guard is the standing rule of this package written down: only ever write
// cgroup.kill to a directory we created. A path outside the base cannot be one of
// ours, and the base's own name has to look like a base — the two together mean a
// caller cannot turn a Cgroup into a write to an ancestor.
func (c *Cgroup) guard() error {
	if !strings.HasPrefix(filepath.Base(c.base), namePrefix) {
		return fmt.Errorf("cgroup: %s is not a shelld base", c.base)
	}
	if !strings.HasPrefix(c.dir, c.base+string(filepath.Separator)) {
		return fmt.Errorf("cgroup: %s is not under %s", c.dir, c.base)
	}
	return nil
}

// kill writes cgroup.kill, which SIGKILLs every member of the cgroup and of every
// cgroup below it, atomically with respect to forks: a member cannot outrun it by
// forking, which a pid-by-pid sweep has to be written to handle.
func kill(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0); err != nil {
		return fmt.Errorf("cgroup: kill %s: %w", dir, err)
	}
	return nil
}

// populated reports whether cgroup.events says the cgroup (or anything below it)
// holds a process.
func populated(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "populated "); ok {
			return strings.TrimSpace(v) != "0", nil
		}
	}
	return false, fmt.Errorf("cgroup: no populated line in %s/cgroup.events", dir)
}

// removeIfEmpty rmdirs dir and every cgroup under it, deepest first, and removes
// nothing that still holds a process. It returns how many cgroups it left behind, so
// a caller can tell "there was nothing to do" from "something is still running in
// there" without being told which.
//
// Only directories are recursed into: the interface files a cgroup contains are not
// removable and are not there to be counted. Depth-first is not a preference — rmdir
// of a cgroup with a child directory is EBUSY whether or not anything is in either.
// A parent left behind because its child was is counted once for each, which
// overstates nothing that matters: the number is a signal, and zero is the only value
// a caller branches on.
func removeIfEmpty(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("cgroup: read %s: %w", dir, err)
	}
	left := 0
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, err := removeIfEmpty(filepath.Join(dir, e.Name()))
		left += n
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		if err := errors.Join(errs...); err != nil {
			return left, err
		}
	}
	pop, err := populated(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return left, nil
		}
		return left, err
	}
	if pop || left > 0 {
		return left + 1, nil
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return left + 1, fmt.Errorf("cgroup: rmdir %s: %w", dir, err)
	}
	return left, nil
}
