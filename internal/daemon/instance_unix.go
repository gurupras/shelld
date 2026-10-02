//go:build linux || darwin

package daemon

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// InstanceLock is held by the daemon serving a socket for as long as it runs:
// a POSIX write lock on <socket>.lock. The kernel releases it when the
// process exits, however it exits, and F_GETLK names the holder's pid, so
// the lock both keeps a second daemon off the socket and identifies the
// daemon without trusting a pid in a file. The lock file is never removed:
// removing a lock file while it is locked lets a second process lock a new
// file of the same name.
type InstanceLock struct {
	f  *os.File
	id lockID
}

// ErrInstanceHeld is returned when another process holds a socket's instance
// lock.
var ErrInstanceHeld = errors.New("the socket is already held")

func lockPath(socket string) string { return socket + ".lock" }

// lockID is the inode a record lock belongs to. The zero value means unknown,
// which no real file can be: inode 0 is not allocated.
type lockID struct{ dev, ino uint64 }

// heldLocks records, by inode, the lock files this process already holds.
//
// It exists because closing any descriptor for a file drops every POSIX record
// lock the closing process holds on that file. That makes both of the ways this
// package would otherwise touch a lock file it already holds destructive rather
// than merely useless: opening one to read its holder releases it again as the
// helper returns, and taking one a second time succeeds — a process never
// conflicts with itself — after which the first Release closes a descriptor and
// frees the lock the second caller still believes it has. Either way the socket
// ends up unguarded while a daemon is still serving it, and the next daemon to
// start binds over the top. This registry answers both questions without a
// descriptor whenever the answer is yes. Where it has to fall back to the kernel,
// and so to a descriptor, keptLockFiles is what stops that descriptor being closed
// when it turns out to name a held file after all.
//
// Consulting it is not enough: it has to be held across the whole of each of the
// three operations that reach a lock file, because in every one of them the gap
// between deciding and acting is a gap an acquire can land in.
//
//   - An acquire. The kernel grants a second lock on a file this process already
//     holds, so two acquires that both read the registry before either writes it
//     are both granted, and the first release then drops the other's lock.
//   - A release. Closing the descriptor and forgetting the entry have to be one
//     step, or the order between them has to be chosen rather than made safe.
//   - Reading the holder. Deciding this process is not the holder and then closing
//     a descriptor for the lock file are the same gap again: an acquire that
//     registers between the two is a lock the close drops.
//
// So the check and the syscall it stands in for are one step in each case. The
// price is that nothing reached while the mutex is held may take it again — which
// is why the registry has no self-locking predicate, and why acquireHeld reports a
// competing holder as errLockBusy and leaves naming the process to
// AcquireInstance, which has let go by then.
//
// What the mutex cannot serialise is a path. It orders this process against itself,
// not against whoever else may rename the lock file between one syscall naming that
// path and the next, so the operations here identify a file by its inode wherever
// the answer has to survive a rename. Both operations that open a lock file —
// acquireHeld and holderHeld — therefore ask the descriptor rather than the path
// once they have one, because a check made of a path cannot answer for the file that
// was actually opened.
//
// The key is the inode rather than the path because os.Lstat opens nothing and
// so cannot drop a lock, because one file has many spellings, and because a lock
// file that has been unlinked and recreated is a new inode this process
// genuinely does not hold — which a path key would get backwards. An inode number
// alone is recycled promptly, but not under an entry here: the descriptor holding
// the lock also pins the inode for as long as the entry is meant to exist.
var heldLocks = struct {
	sync.Mutex
	m map[lockID]bool
}{m: map[lockID]bool{}}

// keptLockFile is a descriptor this package opened and may not close, with the
// inode it turned out to name. The inode is recorded here rather than read back
// later because an open descriptor pins its inode, so a second fstat can only
// return the answer the first one gave; the zero id means the inode could not be
// read at all, and such an entry is kept unconditionally.
type keptLockFile struct {
	f  *os.File
	id lockID
}

// keptLockFiles holds the descriptors that may not be closed, because closing one
// would release a lock this process holds. It is guarded by heldLocks, and is empty
// unless a lock file was replaced underneath a path between two syscalls naming it,
// which is the only way either operation that opens one can end up with a descriptor
// for a file the registry says is held.
//
// Entries are never merged, though many of them can name the same inode. A second
// descriptor for a file is not redundant: closing it drops the lock held through the
// first, exactly as closing the first would, so a duplicate is no more closable than
// the original. There is no bound on the set short of the sweep below.
var keptLockFiles []keptLockFile

// releaseKeptLocked closes the kept descriptors whose lock this process has since
// given up, which is every one of them once the lock they belonged to is released.
// It requires heldLocks and is called from Release, so the common case is a nil
// slice and no work at all. An entry whose inode was never readable is kept, on the
// same grounds it was kept in the first place: nothing here may close a descriptor
// it cannot prove is safe to close.
//
// Deciding costs no syscall, which matters because this runs under the registry that
// every acquire and every holder read waits on, and because the set is only large
// when something is replacing a lock file in a loop — so the sweep is slowest exactly
// when the lock is busiest.
func releaseKeptLocked() {
	live := keptLockFiles[:0]
	for _, k := range keptLockFiles {
		if k.id == (lockID{}) || heldLocked(k.id) {
			live = append(live, k)
			continue
		}
		k.f.Close()
	}
	keptLockFiles = live
}

func lockIDOf(st os.FileInfo) (lockID, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return lockID{}, false
	}
	return lockID{dev: uint64(sys.Dev), ino: uint64(sys.Ino)}, true
}

// lockIDAt identifies the lock file at path without opening it. It does not
// follow a symlink, matching openLockFile, which refuses one: a symlink's own
// inode never matches a held file's, so a caller falls through to the open and
// gets that refusal rather than a wrong answer from here.
func lockIDAt(path string) (lockID, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return lockID{}, false
	}
	return lockIDOf(st)
}

// heldLocked reports whether this process holds the lock on id, and requires
// heldLocks: there is deliberately no form that takes the mutex itself, because
// every caller has to go on holding it while it acts on the answer. The zero id is
// never held — it stands for an inode that could not be read, and inode 0 is not
// allocated.
func heldLocked(id lockID) bool {
	return id != (lockID{}) && heldLocks.m[id]
}

// openLockFile opens the lock file without following a symlink, and refuses
// one owned by another user, as the socket may live in a shared directory.
func openLockFile(socket string, create bool) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(lockPath(socket), flags, 0o600)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
			f.Close()
			return nil, fmt.Errorf("%s is owned by uid %d, not this user", lockPath(socket), sys.Uid)
		}
	}
	return f, nil
}

// AcquireInstance takes socket's instance lock without waiting. When another
// process holds it, the error wraps ErrInstanceHeld and names that process; when
// this one already does, it wraps ErrInstanceHeld too, since either way the
// socket is held and the caller must not start a second daemon.
func AcquireInstance(socket string) (*InstanceLock, error) {
	l, err := acquireHeld(socket)
	if errors.Is(err, errLockBusy) {
		// Named out here: InstanceHolder consults the registry, which acquireHeld
		// holds for its whole body.
		holder, _ := InstanceHolder(socket)
		return nil, fmt.Errorf("%w by another shelld (pid %d holds %s, possibly on another host if it is on NFS)", ErrInstanceHeld, holder, lockPath(socket))
	}
	return l, err
}

// errLockBusy reports that another process holds the lock, and is never returned
// to a caller: AcquireInstance replaces it with an error naming that process,
// which it can only find out once the registry is free again.
var errLockBusy = errors.New("another process holds the instance lock")

// acquireHeld performs the whole acquire — the same-process check, the open, the
// F_SETLK and the registration — under heldLocks. See heldLocks for why these
// cannot be separate steps, and for what this may not call.
func acquireHeld(socket string) (*InstanceLock, error) {
	heldLocks.Lock()
	defer heldLocks.Unlock()

	// Before the open, because every path out of openLockFile that fails closes the
	// descriptor it opened, and that alone would drop a lock this process holds. The
	// descriptor is checked again below, which is what actually makes the refusal
	// sound; this one keeps the ordinary second acquire from having to open the file
	// at all, and so from parking a descriptor for every one of them.
	if id, ok := lockIDAt(lockPath(socket)); ok && heldLocked(id) {
		return nil, fmt.Errorf("%w by this process (pid %d already holds %s)", ErrInstanceHeld, os.Getpid(), lockPath(socket))
	}
	f, err := openLockFile(socket, true)
	if err != nil {
		return nil, fmt.Errorf("instance lock: %w", err)
	}
	// Identified from the descriptor, and identified before anything below can close
	// it. The check above was of a path, so it cannot answer for the file that was
	// actually opened: a lock file replaced between the two by a link to one this
	// process holds gets past it, and then every remaining step here is destructive.
	// F_SETLK would be granted, because a process never conflicts with itself, and a
	// second lock on an inode already registered means whichever caller releases
	// first frees it for the other. A failing step would close the descriptor, which
	// releases the lock outright. Both are avoided by asking the descriptor, whose
	// inode no rename can change, and by keeping rather than closing a descriptor
	// that cannot be proved safe to close.
	st, serr := f.Stat()
	if serr != nil {
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f})
		return nil, fmt.Errorf("instance lock %s: identifying the opened file: %w", lockPath(socket), serr)
	}
	id, ok := lockIDOf(st)
	if !ok {
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f})
		return nil, fmt.Errorf("instance lock %s: the opened file reports no inode to record it by", lockPath(socket))
	}
	if heldLocked(id) {
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f, id: id})
		return nil, fmt.Errorf("%w by this process (pid %d already holds the file now at %s)", ErrInstanceHeld, os.Getpid(), lockPath(socket))
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lk); err != nil {
		// Safe to close: the inode is known not to be one this process holds, and the
		// registry has been held ever since, so no acquire can have made it one.
		f.Close()
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("instance lock %s: %w", lockPath(socket), err)
	}
	// Registered under the descriptor's inode rather than the path's, so the lock is
	// recognised as this process's again even if the path has been replaced since.
	// Without that, InstanceHolder would fall back to the open-and-close that
	// releases it, and an unregistered lock is the object this registry exists to
	// abolish.
	heldLocks.m[id] = true
	return &InstanceLock{f: f, id: id}, nil
}

// Release drops the lock. Callers remove the daemon's files before calling
// it, so no other process can lock the socket while they still exist.
func (l *InstanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// Both steps under the registry, which leaves no order to get right. Forgetting
	// before closing would let a concurrent acquire in this process be granted a
	// lock -- the kernel does not conflict with itself -- that the close below then
	// drops underneath it. Closing before forgetting would instead report this
	// process as holding a lock the kernel has already freed, which refuses an
	// acquire that should have succeeded. Holding the registry across both closes
	// each window, rather than picking the less damaging one.
	heldLocks.Lock()
	defer heldLocks.Unlock()
	l.f.Close()
	l.f = nil
	delete(heldLocks.m, l.id)
	// After the entry is gone, so a descriptor kept for this inode is now closable.
	releaseKeptLocked()
}

// InstanceHolder returns the pid holding socket's instance lock, or 0 when
// nobody holds it or there is no lock file. When this process is the holder it
// is named from this package's own record of the lock rather than from the
// kernel, which reports a process's own record lock as free — and which cannot
// be asked without a descriptor whose closing would release that lock.
func InstanceHolder(socket string) (int, error) {
	heldLocks.Lock()
	defer heldLocks.Unlock()
	return holderHeld(socket)
}

// holderHeld answers InstanceHolder's question with heldLocks already held,
// which is what keeps its own descriptor from releasing somebody else's lock.
// Deciding that this process is not the holder and then closing a descriptor for
// the lock file are not separate steps: an acquire in this process that registers
// between the two is a lock the kernel has granted and this close then drops,
// leaving the socket unguarded while the acquirer still believes it is held. The
// registry is what serialises the two, so no acquire can land inside the window —
// which is also why nothing here may call InstanceHolder.
//
// The registry cannot serialise the other half of this, because the other half is
// a path: the Lstat above names one, the open below names it again, and in between
// the lock file can be replaced by a link to a lock file this process does hold —
// belonging to any socket, not only this one. Nothing about the directory rules
// that out. The lock arbitrates between processes of the same user, so "only this
// user can reach it" excludes nobody the lock defends against, and a socket given
// explicitly outside DefaultDir gets a warning rather than a refusal. So the file
// the close would actually reach is identified from the descriptor, which is the
// one name a rename cannot change.
func holderHeld(socket string) (int, error) {
	if id, ok := lockIDAt(lockPath(socket)); ok && heldLocked(id) {
		return os.Getpid(), nil
	}
	f, err := openLockFile(socket, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Identified again, now that there is a descriptor to identify it by. A
	// descriptor for a file this process holds must not be closed, so it is kept
	// instead — and kept referenced, because merely declining to close an *os.File
	// leaves its finalizer to close it once the collector reaches it. A failure to
	// identify it is treated the same way: nothing here may close a descriptor it
	// cannot prove is safe to close.
	st, serr := f.Stat()
	if serr != nil {
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f})
		return 0, fmt.Errorf("instance lock %s: identifying the opened file: %w", lockPath(socket), serr)
	}
	if id, ok := lockIDOf(st); ok && heldLocked(id) {
		keptLockFiles = append(keptLockFiles, keptLockFile{f: f, id: id})
		return os.Getpid(), nil
	}
	defer f.Close()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lk); err != nil {
		return 0, err
	}
	if lk.Type == unix.F_UNLCK {
		return 0, nil
	}
	return int(lk.Pid), nil
}

// LocateDaemon works out whether the daemon serving socket, described by lock,
// is still there. The instance lock's holder is the daemon. With the lock free,
// lock.Info.Pid is checked with sameDaemon, for daemons that don't take the lock
// (older versions, shelld-rs); anything bound to the socket keeps the state
// unverified, and so does a lockfile that would not read, whatever is bound.
func LocateDaemon(socket string, lock LockRead) Located {
	holder, err := InstanceHolder(socket)
	if err != nil {
		return Located{State: DaemonUnverified, Reason: err.Error()}
	}
	if holder > 0 {
		info, alive, perr := processInfo(holder)
		return holderLocated(holder, info, alive, perr)
	}
	// A lockfile that could not be read is answered before anything reads
	// lock.Info, and it answers unverified whether or not the socket is bound:
	// the file that would name the daemon could not be read, so nothing here can
	// be shown either way. This is the same rule the instance lock already
	// follows one file over -- an unreadable lock is unverified, not gone -- and
	// it matters more here, because every caller of DaemonGone goes on to remove
	// files, which would mean deleting the very lockfile it could not read.
	if lock.Err != nil {
		return Located{State: DaemonUnverified, Reason: fmt.Sprintf("cannot read the lockfile at %s: %v", lock.Path, lock.Err)}
	}
	// Bound is the question, not answering /health, and the two part company
	// exactly where it matters: a daemon whose backlog has filled, or that is
	// slower to reply than a probe waits, is up and serving while a round trip to
	// it fails. Calling that gone is not a label that stops at being wrong --
	// every caller of DaemonGone goes on to remove the socket file, so it takes a
	// running daemon's socket away and leaves it serving under a name no client
	// can reach. isBound reads a full backlog as life, and anything it cannot tell
	// apart from life as life; see its comment.
	if lock.Info == nil || lock.Info.Pid <= 0 {
		if isBound(socket) {
			return Located{State: DaemonUnverified, Reason: "something is bound to the socket, but no lockfile names its process"}
		}
		return Located{State: DaemonGone}
	}
	pid := int(lock.Info.Pid)
	info, alive, err := processInfo(pid)
	switch {
	case err != nil:
		return Located{State: DaemonUnverified, Pid: pid, Reason: err.Error()}
	case !alive:
		if isBound(socket) {
			return Located{State: DaemonUnverified, Pid: pid, Reason: "something is bound to the socket, but the lockfile's process has exited"}
		}
		return Located{State: DaemonGone, Pid: pid, Reason: "the process has exited"}
	}
	ok, why := sameDaemon(info, time.Now(), *lock.Info)
	// A live shelld at the lockfile's pid is this socket's daemon only if the
	// lockfile names this socket. Callers read the lockfile before this and act
	// on the pid after it, so the file can have been rewritten in between by a
	// daemon starting on another socket with the same pidfile: verified, that
	// daemon is the one stop or kill would signal. Not gone either, whether or
	// not this socket is bound: callers of DaemonGone remove the lockfile, and
	// this one is a live daemon's.
	if ok && lock.AimsAt(socket) != LockfileAgrees {
		return Located{State: DaemonUnverified, Pid: pid, Reason: fmt.Sprintf("the lockfile at %s names the socket %s, not this one", lock.Path, lock.Info.Socket)}
	}
	if ok {
		return Located{State: DaemonVerified, Pid: pid}
	} else if isBound(socket) {
		return Located{State: DaemonUnverified, Pid: pid, Reason: why}
	} else {
		return Located{State: DaemonGone, Pid: pid, Reason: why}
	}
}

// signalDaemon signals the located daemon. The process is pinned first
// (os.FindProcess holds a pidfd on Linux) and located again, so a pid reused
// in between is never signalled.
func signalDaemon(socket string, lock LockRead, sig syscall.Signal) (int, error) {
	loc := LocateDaemon(socket, lock)
	if loc.State != DaemonVerified {
		return loc.Pid, &NotDaemonError{Located: loc}
	}
	p, err := os.FindProcess(loc.Pid)
	if err != nil {
		return loc.Pid, err
	}
	defer p.Release()
	if again := LocateDaemon(socket, lock); again.State != DaemonVerified || again.Pid != loc.Pid {
		return loc.Pid, &NotDaemonError{Located: again}
	}
	if err := p.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return loc.Pid, err
	}
	return loc.Pid, nil
}

// KillDaemon SIGKILLs the daemon when it is verified.
func KillDaemon(socket string, lock LockRead) (int, error) {
	return signalDaemon(socket, lock, syscall.SIGKILL)
}

// TerminateDaemon SIGTERMs the daemon when it is verified.
func TerminateDaemon(socket string, lock LockRead) (int, error) {
	return signalDaemon(socket, lock, syscall.SIGTERM)
}

// RemoveStale removes the socket and the lockfile at pidfile only while holding
// the instance lock, and only if nothing is bound to the socket, so a live
// daemon's files are never removed. It reports whether it removed them.
//
// Both guards are needed and neither implies the other. The lock covers a daemon
// of this version, which holds it for as long as it runs; a daemon that does not
// take it -- an older shelld, or shelld-rs sharing the path -- leaves it free, so
// what is bound to the socket is all there is to go on there.
// The lockfile is removed only when it names this socket. Both guards above are
// about the socket, and neither says anything about the file at pidfile: that path
// was unlinked because it had been given on the command line, so establishing that
// one socket was free was enough to remove a lockfile belonging to a daemon on
// another one.
// The socket path is checked for being a socket before either guard runs, because
// nothing below could establish it. Both guards ask whether anything is serving
// the path, and every file that is not a socket answers no: a text file at
// --socket is not bound and nobody holds its instance lock, so it read as a stale
// socket and was unlinked. The type is all there is to go on -- a socket file names
// no owner -- and it is enough to separate a path that may hold a stale socket of
// ours from a path that cannot hold one at all.
//
// It is ordered before AcquireInstance rather than beside the unlink because
// taking the lock creates <socket>.lock. Deciding not to touch a path is not
// something to do by writing a file next to it.
func RemoveStale(socket, pidfile string) (bool, error) {
	isSock, sockErr := CheckSocketPath(socket)
	if !isSock {
		// The lock must not be taken here, so the question it would have answered is
		// put to NothingServes instead, which creates nothing.
		if !NothingServes(socket) {
			return false, errors.Join(sockErr, fmt.Errorf("%s is still served, so its files were left in place", socket))
		}
		// A refusal about the socket does not carry over to the lockfile. They are
		// separate files, and the lockfile is the one that makes shelld report a daemon
		// that is not there, so leaving it behind because an unrelated path was the
		// wrong type is a second wrong outcome rather than a cautious one.
		lockErr := RemoveLockfileIfOurs(pidfile, socket)
		return sockErr == nil && lockErr == nil, errors.Join(sockErr, lockErr)
	}
	// Ordered before AcquireInstance for the reason in socketWasServed: that call
	// creates one of the two witnesses it reads.
	if served, why := socketWasServed(socket, pidfile); !served {
		// The lockfile is still attempted, and can only refuse here: the one state
		// RemoveLockfileIfOurs removes in is the lockfile naming this socket, which is
		// the second witness, so reaching this line means it will not remove anything.
		// It is called for its error rather than its effect -- an unreadable lockfile is
		// the arm that reaches here with something left to say.
		return false, errors.Join(why, RemoveLockfileIfOurs(pidfile, socket))
	}
	l, err := AcquireInstance(socket)
	if err != nil {
		return false, err
	}
	defer l.Release()
	if isBound(socket) {
		return false, fmt.Errorf("%s is still served, so its files were left in place", socket)
	}
	// Both are attempted and both refusals are reported: one file being unremovable
	// is not a reason to leave the other in place, and an operator repairing this
	// needs to be told about each file rather than about whichever failed first.
	lockErr := RemoveLockfileIfOurs(pidfile, socket)
	socketErr := removeSocketFile(socket)
	return lockErr == nil && socketErr == nil, errors.Join(lockErr, socketErr)
}

// socketWasServed reports whether anything on disk shows a shelld daemon served
// socket, and when nothing does, says what was looked for.
//
// A socket file names no program that bound it -- CheckSocketPath says so, and it is
// the whole reason the type is all that check establishes -- so "there is a socket
// here and nothing answers on it" is equally true of a stale shelld socket and of
// another program's socket whose process is gone. RemoveStale treated the second as
// the first: it unlinked the file, put nothing in its place, exited 0 and printed
// nothing, so an operator who pointed it at the wrong path was told the daemon was
// gone by a command that had removed somebody else's address instead. That is worse
// than the not-a-socket case beside it, which at least refuses: a socket file is the
// shape this is meant to remove, and every guard here agreed it was removable.
//
// Two witnesses, because neither implementation writes the other's file:
//
//	<socket>.lock	the Go daemon's instance lock. acquireHeld creates it before the
//			bind and nothing ever removes it -- see InstanceLock, which says
//			why removing one is unsafe -- so a Go daemon that got as far as
//			taking it left this behind however it died.
//	the lockfile	{pid, socket, ...}, written once a daemon owns the socket, by
//			this daemon and by shelld-rs, which has no instance-lock file at
//			all. It is what keeps this from refusing to clear up after the
//			other implementation sharing the path, which RemoveStale's own
//			doc says is a case it serves.
//
// Either is enough and neither is necessary: a Go daemon leaves both, a shelld-rs
// daemon only the second, and a Go daemon whose lockfile a later command already
// removed only the first.
//
// Only a successful Lstat counts as the first witness. Anything else -- a permission
// error on the directory, a name too long -- leaves it unknown, and an unknown
// witness is not a witness: what is being decided is whether to unlink a file, and
// the cost of refusing wrongly is a message an operator can act on against a file
// they can still remove by hand.
//
// It is asked before AcquireInstance because AcquireInstance creates the first
// witness. Ordered after it, this would be satisfied by the file its own previous
// refusal had written: the same wrong removal would go through on the second run of
// the same command, and the thing that let it through would be litter this command
// left beside a path it had just declined to touch. So the ordering is not a
// tidiness argument -- it is what makes the check hold more than once.
func socketWasServed(socket, pidfile string) (bool, error) {
	if _, err := os.Lstat(lockPath(socket)); err == nil {
		return true, nil
	}
	if ReadLock(pidfile).AimsAt(socket) == LockfileAgrees {
		return true, nil
	}
	return false, fmt.Errorf("a socket is at %s but nothing shows shelld served it: there is no %s and the lockfile at %s does not name that socket, so it is not shelld's to remove", socket, lockPath(socket), pidfile)
}

// NothingServes reports whether nothing serves socket any more: its instance
// lock is free and nothing is bound to it. A daemon too busy to answer a request
// is still serving, so this waits for it rather than declaring it gone.
func NothingServes(socket string) bool {
	if holder, err := InstanceHolder(socket); err != nil || holder > 0 {
		return false
	}
	return !isBound(socket)
}
