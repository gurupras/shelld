// Package cgroup places each window's shell in a cgroup v2 directory of its own,
// so that closing a window can kill by MEMBERSHIP rather than by discovery.
//
// Discovery is what proctree does, and it is a union of two /proc relations —
// parent links and session id — because neither one alone survives the ways a
// process can leave a tree (see proctree's package comment). Membership is a
// different kind of answer: a process placed in a cgroup at fork is in it no
// matter how it reparents, calls setsid, or is orphaned, and `cgroup.kill` reaches
// every member in one write. Measured on the F3 fixture: 2 of 4 members had
// reparented to `systemd --user` and both were still in the window's cgroup; a
// descendant that had both setsid'd and been orphaned — the one residual the /proc
// union cannot reach from root — was also still there, and `cgroup.kill` left 0 of
// 4 alive.
//
// The two are CO-PRIMARY, not one behind the other, and each covers a case the
// other misses. A process in a delegated subtree can write its own pid into any
// cgroup.procs it can open, and every ancestor of ours qualifies: migration out of
// the window cgroup to the per-daemon base, to a sibling window, or to the
// daemon's own scope were all measured to succeed with no error. So the cgroup can
// lose a process, and what it loses is exactly one process — migration moves the
// writer and leaves its already-forked children behind — which is a process the
// /proc union still holds by parent link or session. Conversely the union loses the
// setsid'd orphan, which the cgroup holds. Neither set is a subset of the other.
//
// What this package deliberately does not do: enable a single controller. Only the
// core interface files (`cgroup.kill`, `cgroup.procs`, `cgroup.events`) are used,
// so nothing here needs `cgroup.subtree_control` and nothing here can run into the
// no-internal-processes rule, which constrains controllers rather than cgroups.
package cgroup

import "errors"

// ErrUnsupported means this platform has no cgroup v2 placement. It is not an
// error to handle so much as a mode to run in: the caller keeps the /proc union,
// which is the arm that works everywhere.
var ErrUnsupported = errors.New("cgroup: unsupported platform")
