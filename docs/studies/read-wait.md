# Study: waiting 100 µs after a short PTY read

Status: **not adopted.** The code is not in the tree; this records what was measured.

## The idea

tmux reads a pane with libevent and usually finds a whole redraw in one read. shelld's reader
used to read each time the terminal became readable. When a program draws a screen in several
small writes, shelld often saw it as two reads. The live framer then sent the second half in a
later frame, one 8 ms pacing slot after the first.

The candidate added `pty.ReadEach`, a read loop on the raw fd. After a read shorter than 2048 B,
it slept 100 µs and read again, so the pieces of a redraw arrived in one read.

## What it bought (measured)

Interactive latency: a raw-mode program answers a key with a redraw of several writes. n=300 per
arm and scenario, arms interleaved.

| redraw | base p50 | with the wait p50 | tmux p50 | base frames split ≥7.5 ms | with the wait |
|---|---|---|---|---|---|
| 1 write | 0.196 ms | 0.189 ms | 0.154 ms | – | – |
| 6 writes, 30 µs apart | 8.448 ms | 0.343 ms | 0.266 ms | 286/300 | 2/300 |
| 3 writes, 50 µs apart | 8.422 ms | 0.327 ms | 0.249 ms | 290/300 | 4/300 |
| 2 writes, 150 µs apart | 0.340 ms | 0.351 ms | 0.294 ms | – | – |

Throughput, 20k lines to one client, 30 reps:
- reads dropped 94% (9372 → 564);
- daemon CPU dropped 36% (61.1 → 39.4 ms);
- voluntary context switches dropped 48%.

## What it cost (measured)

100 windows, each writing a little output often, 3 rounds:

| output | daemon CPU, base → with the wait | voluntary context switches/s | OS threads |
|---|---|---|---|
| idle | 0.0 → 0.0 | 0 → 0 | unchanged |
| 20 B every 50 ms | 6.0% → 6.8% of a core | 7403 → 11773 | +6 |
| 20 B every 1 ms | 133% → 161% | 151759 → 198317 | +15 to +20, peak +44 |

When writes are further apart than the wait, there is nothing to batch, so the wait is pure
cost: +13 to +21% daemon CPU. `unix.Nanosleep` is a blocking syscall. Each window asleep in it
holds an OS thread, and the runtime starts another M to keep the P busy.

## Correctness defects found in the candidate as first written

- **Close could block forever.** `FD.Close` waits for every reference to the fd to drop.
  `RawConn.Read` holds one reference for as long as its callback keeps returning true. Against a
  program that never stops writing, `Close` never returned, so a hangup kill never hung the
  terminal up.
  - Fix: a `closing` flag checked before every read.
  - Pinned by a test that fails 5/5 without the flag.
- **The window could miss its program's exit.** Sometimes the last bytes and the slave's hangup
  arrived in one edge-triggered wakeup. After a 2048-4095 B read the loop parked without first
  reading to EAGAIN, and no further edge came. The EIO was never read, and the window stayed
  alive after its program exited.
  - Failure rate: 2 of 4 runs hung at the PTY level, and both 500-window runs failed, at windows 259 and 277.
  - Fix: after any read that returned data, read until EAGAIN or EIO.
  - With the fix: 0 in 1000 windows and 0 in 600 targeted cases.

Kill latency was the same with and without the fixed loop: DELETE p50 19.0 vs 18.8 ms for a tree
kill, and 5.0 vs 4.8 ms for a hangup kill (n=40).

## Why it was not adopted

The win is real but narrow: bursts of small writes less than ~100 µs apart. It is paid for in CPU
and threads by every window whose output trickles, and the fixed loop is Linux-only. The two
defects show how easily a raw read loop loses EOF or blocks Close.

The kill tests written for it do not depend on it, so they are in the tree
(`killhangup_writer_linux_test.go`). A future change to the read loop has to keep them green.
