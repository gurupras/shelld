# Study: the cost of a keystroke in a near-idle window

Typing into an otherwise idle window costs shelld more CPU per key than tmux. Four variants were
prototyped against that cost and vetted separately. Each got correctness tests with mutants, then
paired, interleaved benchmark rounds.

The bar for adopting a variant:
- a win whose 95% CI excludes zero;
- no regression elsewhere whose CI excludes zero.

| variant | what it does | outcome |
|---|---|---|
| B | the live framer copies only the rows damaged since a client's last frame | **adopted** |
| A | a keystroke written straight to the PTY when nothing is queued | dropped |
| C | the PTY reader renders and writes parked clients' frames inline | dropped |
| D | raw-syscall reads (`RawSyscall` read loop on the PTY and WebSocket fds) | dropped |
| CD, CD1, CD2 | combinations of C and D | dropped; CD2 missed by one 8-client p90 |

Method:
- Per key: shellbench's echo probe, 22-26 rounds per size, arm order rotated each round, every
  round under one benchmark lock. us/key = cpu_pct × 250. Figures are the median per-round
  difference with a paired bootstrap 95% CI.
- Multi-client: 8 clients at 120x40 and 16 at 300x100, each client timing how long a state takes
  to reach it.

## B: damage-stamped frames (adopted)

A frame used to snapshot the whole screen. Now each line carries the epoch at which it was last
written. A consumer remembers the epoch of its last capture and copies only the lines changed
since. Nothing is cleared, so a slow client cannot lose another client's damage; this was
checked with 5 consumers at different paces.

- 200x50 client: −21.7 µs/key, CI [−25.0, −14.5], about 15%.
- 80x24: below the harness's ~6 µs resolution. The micro-benchmark predicts ~2.9 µs.

A +6% in one tick benchmark was reproduced without B by 96 bytes of padding, so it is
function alignment, not a cost. B is on unconditionally.

## A: direct PTY write (dropped)

| size | CPU µs/key | echo p50 | p99 | vcsw/s |
|---|---|---|---|---|
| 80x24 | −11.1 [−14.3, −7.6] | −10 µs [−12, −8] | CI includes 0 | −12 |
| 200x50 | **+3.6 [+0.9, +7.8]** | −4.5 µs [−8, −2] | −28.5 µs [−44, −8] | +77 |

The CPU result is mixed: a saving at 80x24 and a cost at 200x50. A also adds a second input path
that has to keep ordering with the queued one. The ordering held under four mutants, but that
path is complexity paid for a few microseconds of echo. It works on Linux only.

## C: inline render on the PTY reader (dropped)

| size | CPU µs/key | vcsw/s |
|---|---|---|
| 80x24 | −1.4 [−5.8, +2.5], null | +57 |
| 200x50 | **+4.1 [+1.75, +8.4]**, a cost | +105 |

Multi-client tail, C on vs off:
- 8 clients at 120x40: p90 +3.05 ms [+3.02, +3.09].
- 16 clients at 300x100: p90 +2.92 ms, p99 +2.88 ms.

The reader renders and writes each parked client in turn, so the last client to attach waits
for all the others. tmux never makes the pane reader wait on a client (`tty.c`). C trades
microseconds of single-client echo for milliseconds of multi-client tail.

While vetting C, its first version showed the same lost-EOF defect as the read-wait study
(`read-wait.md`): a window never ended after its program exited (1 in 12 windows under load).

## D and the C+D combinations (dropped)

D on its own was within noise of the baseline: +14 µs/key, sign test p=0.69. Combined with C it
is a large per-key win, at both sizes:

| arm − baseline | 80x24 CPU µs/key | 200x50 CPU µs/key |
|---|---|---|
| CD | −31.0 [−38.25, −23.5] | −30.9 [−32.9, −23.75] |
| CD1 (C only with one client attached) | −27.0 [−31.75, −22.75] | −24.75 [−28.5, −19.0] |

Each combination still had a regression with a CI clear of zero:

- **CD** keeps C's tail: p90 +3.03 ms at 8@120x40, p99 +2.54 ms at 16@300x100.
- **CD1** removes C's tail at 8 clients. At 16@300x100 it still adds p99 +0.93 ms
  [+0.62, +1.03], in 21 of 22 rounds. Attribution runs put that residual on D: D alone has
  p99 +0.67 ms there, C1 alone is null.
- **D's raw reads** cost under flood (n=10):
  - GC pause p50: +18.4 µs [+16.4, +20.5];
  - scheduler latency p99: +16 µs;
  - read throughput: −17 MB/s, about −7%.

  A raw read holds its P instead of parking in the netpoller.
- **CD2** came closest; see below.

So the per-key CPU gain belongs to D, and D carries the multi-client tail. No combination met
the bar.

## CD2: raw reads only with one client (dropped)

CD2 is CD1 with D's raw PTY reads switched per read: the read loop reads raw only while exactly
one client is attached (an atomic count kept at register and unregister), and plainly otherwise.

Correctness held:
- byte-exact PTY bursts of ~29 MB across ~3,400 raw/plain switches;
- 840 of 840 windows read every byte then EOF across switches, raw or plain last;
- clients attaching and leaving 1→2→1→2→1, typing and flooding, with every line back in order;
- four mutants, all red: stays raw on 1→2, latches plain on 2→1, decides once at entry, and a
  dropped-byte control.

Per key, 22 rounds, CD2 − baseline:

| size | CPU µs/key |
|---|---|
| 80x24 | −27.75 [−33.62, −21.38], lower in 22/22 rounds |
| 200x50 | −28.50 [−34.25, −21.00], lower in 22/22 rounds |

Multi-client, 22 rounds, baseline, CD1 and CD2 interleaved:
- 16@300x100: p50, p90 and p99 all null (p99 −0.052 ms). CD1 in the same rounds reproduced its
  p99 +0.447 ms [+0.35, +0.79].
- 8@120x40: p50 −0.014 ms and p99 null, but **p90 +0.036 ms [+0.019, +0.045]**, higher in 18 of
  22 rounds. At 8 clients CD2's PTY reads were plain (23,790 plain, 2 raw, during the attach
  ramp), so the cost is elsewhere; D's WebSocket raw reads and writes stay on with any number
  of clients. The cause was not isolated.
- GC under flood with two clients matches the baseline. The one-client raw path still shows
  D's pause cost: p50 +20.5 µs, p99 +49 µs.

Dropped: the p90 regression is a con with a CI clear of zero. CD2 also needs two runtime
switches on one client count (inline-or-wake per notification, raw-or-plain per read).
Untested: many clients typing at once.
