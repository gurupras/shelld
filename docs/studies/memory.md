# Study: per-window memory against tmux

Scenario: 20 windows, each holding 12,000 coloured 49-character lines, with a 10,000-line
history limit. tmux 3.7c runs the same scenario with `history-limit 10000`. RSS is taken with
nothing else on the benchmark lock.

| | RSS |
|---|---|
| tmux | 65.7 MB |
| shelld, packed history | 112.0 MB (108.7 after `runtime.GC`, 98.9 after `debug.FreeOSMemory`) |

## Where the gap is

History storage itself is at parity. A packed history line is 288 B plus a 32 B row header, so
320 B per line. tmux comes to 315 B per line: 3,062 KB per window at hsize 9,952. tmux trims
10% of the limit at once (`grid_collect_history`), so on average it holds ~5% fewer lines.

Exact live heap per window (`memprofilerate=1`):

| item | per window |
|---|---|
| packed history rows | 2,812 KB |
| raw-bytes Scrollback (byte chunks plus the line ring) | 803 KB |
| viewport and the alternate screen, both allocated up front | 402 KB |
| ring of row headers, preallocated to limit + 1000 | 328 KB |

The other ~10-13 MB is heap the Go runtime has freed but not returned to the OS. With ~90 MB
live, the GC goal reaches 181 MB, RSS stays below it, and the scavenger never runs. Runtime
metadata and stacks add ~5 MB, and extra binary pages ~3 MB.

An empty window costs ~300 KB in shelld and ~30 KB in tmux. tmux creates the alternate screen
on demand, and allocates a line's cells only when the line is written (`grid.c`).

## Candidates and what became of them

| candidate | saving at 20 windows | why not adopted |
|---|---|---|
| drop the raw Scrollback, derive raw capture from the grid | ~16 MB | Default `capture` and `/scroll` serve this raw history, so their output would change for every client |
| `GOGC=50` | 9.5 MB (112.0 → 102.5) | CPU: half the GC headroom means more frequent collections. Measured on ingest: `GOGC=25` costs ~8% CPU, `GOMEMLIMIT=12MiB` costs 2.7x CPU and halves throughput |
| `GOMEMLIMIT=96MiB` | 13.7 MB | Same trade as `GOGC`, and a fixed limit only fits a known workload (the 12 MiB case above). Not measured for CPU at this size |
| line ring entry 8 B | 1.8-3.5 MB | Low risk, not prototyped |
| packed rows in exact-size chunks (no 260 → 288 B rounding) | ~5.6 MB | Medium risk, not prototyped |
| **lazy alternate screen** | **~4 MB (200 KB per window)** | **Not tried.** It is what tmux does, and a full-screen program pays for it only while it runs. It is the cheapest candidate left |
| lazy viewport rows | up to 4 MB | Adds a cost on the write path, not prototyped |
| row header 32 → 16 B, ring not preallocated | ~3.8 MB | Medium risk, not prototyped |
| fewer goroutines per window (4 today) | 1-1.5 MB | Not prototyped |
| drop net/http and crypto from the daemon binary | 1-3 MB of clean, shared file pages | High effort for pages that are shared and clean |

Startup RSS is 7.8 MB, against tmux's ~3.9 MB. About 77% of it is clean binary file pages; the
anonymous memory is only ~0.8 MB more than tmux's.

## What was adopted

Packing history rows, which brought history to parity with tmux. Two CPU fixes for the packed
form are also in the tree:
- an encode rewrite: encode −18 to −26%;
- fewer allocations on reflow. Rewiden reflow went from +133% to −28% against the plain-row
  baseline; grow went from +136% to +75%.

Against plain rows, scrolling into history still costs +17 to +25% CPU. The way to close that,
like tmux, is to encode cells as they are written (`grid_set_cell`). That changes the live grid's
representation and was not attempted. Joining two packed rows without decoding them is the
biggest remaining reflow win, and was not attempted either: it has to get wrap flags, a
wide-character spacer at the seam and trimming right.
