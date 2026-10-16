# shelld

shelld is a terminal manager: a daemon that runs programs in pseudo-terminals,
keeps their screens in a terminal emulator, and serves them over an HTTP and
WebSocket API on a Unix socket (a named pipe on Windows). Programs are grouped
into **sessions** of named **windows**, much as tmux groups panes. Any number of
clients can read a window at once, each with its own size and scroll position,
while the program keeps running whether or not anyone is watching.

`shellctl` attaches your terminal to a window, and the `shelld` binary is both
the daemon and its control CLI.

## What it does

- **Sessions and windows.** Create, list and delete sessions. Create windows
  running a shell or a given command line, with a working directory,
  environment additions and removals, and an initial size. Move or rename a
  window, including into a session that does not exist yet. Deleting a window
  kills the program's whole process tree, including descendants that called
  `setsid`, and on Linux it also kills everything in the window's cgroup.
  With `SHELLD_KILL_MODE=hangup` the daemon kills a window as tmux does
  instead: it closes the terminal, which hangs up the program, and returns;
  a process that ignores the hangup or left the session keeps running.
- **Input.** Send text with an optional Enter and a delay before it, single raw
  keys, tmux key names (`Up`, `C-c`, `F5`...), and pastes, plain or bracketed,
  with tmux 3.7c's paste semantics. Input is queued per window with a byte
  budget, so a program that stops reading gets 429s, not an unbounded backlog.
- **Injection.** Queue a message to be typed when the program is idle at its
  prompt, with priorities, a stall timeout and a log of what expired and why.
- **Output.** A WebSocket attach streams the rendered screen as ANSI frames and
  diffs. Each client can scroll back or pause on its own view while output
  continues, and observers (`?observer=1`) are read-only and don't affect the
  window's size. Capture the screen or the raw scrollback, page through
  history, or take a one-off frame.
- **Resize.** Each attached client declares its size. The PTY gets the smallest
  size among clients that vote, and clients larger than that see a crop. The
  emulator reflows scrollback on a width change.
- **Terminal emulation.** A full VT emulator (`vt`): wide and combining
  characters, scroll regions, the alternate screen, charsets, SGR attributes and
  underline colours, DEC 2026 synchronized updates, and replies to device and
  colour queries. Colours a program asks about can be set per window
  (`/colors`).
- **Exit status.** A finished window reports its exit code or the signal that
  ended it.
- **Lifecycle.** Run in the foreground or as a detached daemon, with
  `status`, `stop`, `restart`, `kill` and `logs`. A lockfile and an instance
  lock identify the daemon, so `stop` and `kill` only ever signal shelld.
  Shutdown sends attached clients WebSocket close 1001 and gives in-flight
  requests 2 s to finish.
- **Private by default.** The socket is owner-only, clients refuse a daemon run
  by another user, and the default socket, pidfile and logfile live in a
  per-user directory that must not be accessible to anyone else.
  `--shared-access` lets other users' programs reach the daemon.
- **Bounded.** Request bodies, attaches (256), connections (256), stalled
  request bodies and slow WebSocket readers all have limits, so one
  misbehaving client can't exhaust the daemon.
- **Platforms.** Linux, macOS and Windows (ConPTY and named pipes).

## API

All routes live on the daemon socket. Bodies are JSON.

| Route | What it does |
|---|---|
| `GET /health` | Daemon pid, version, uptime, session and window counts |
| `POST /shutdown` | Shut down gracefully |
| `GET /sessions`, `POST /sessions` | List sessions, sorted by name; create one (`{"name"}`) |
| `GET /sessions/:s`, `DELETE /sessions/:s` | Check a session exists; kill it and its windows (404 if there is none, as tmux fails `kill-session`) |
| `GET /sessions/:s/windows`, `POST /sessions/:s/windows` | List windows in index order (a new window takes the lowest free index, as in tmux); create one (`name`, `shell`, `cwd`, `env`, `unsetEnv`, `cols`, `rows`, `pasteMode`) |
| `DELETE /sessions/:s/windows/:w` | Kill a window and its process tree (404 if there is none) |
| `POST …/keys` | Type text (`keys`, `enter`, `enter_delay_ms`) |
| `POST …/raw-key`, `POST …/key-names` | Send one raw key; send tmux key names |
| `POST …/paste` | Paste text, then press Enter (`text`, optional `mode`) |
| `GET …/capture`, `GET …/scroll` | Visible screen or raw scrollback; a page of history |
| `POST …/resize`, `POST …/colors` | Resize the window; set the colours reported to the program |
| `GET …/attach` | WebSocket: frames out, input and control messages in |
| `GET …/frame` | One rendered frame |
| `POST …/inject`, `GET …/inject/queue` | Queue a message for when the program is idle; inspect the queue |
| `POST /move-window` | Move or rename a window (`srcSession`, `srcName`, `dstSession`, `dstName`) |

On the attach socket, binary frames carry terminal bytes in both directions.
Text frames that start with `\x01{` are control messages: the client sends
`{"resize":{"cols","rows"}}` and `{"scroll":{"op","lines","offset"}}`, and the
server sends `{"status":{…}}` with the client's scroll offset, pause state,
alternate-screen flag and effective size. Any other text frame is typed into
the program.

## CLI

```text
shelld [OPTIONS] [COMMAND]
  run        Serve in the foreground (default)
  daemonize  Start a detached daemon and wait for it to answer /health
  status     Report whether the daemon is healthy
  stop       Shut the daemon down gracefully
  restart    stop, then daemonize
  kill       Kill the daemon (requires --force)
  logs       Print the tail of the log file
  sessions   List sessions
  windows    List windows in a session

shellctl attach <SESSION> <WINDOW>   # Ctrl-] detaches
shellctl <COMMAND> [ARGS]            # one HTTP request, then exit
```

shellctl has a command for every API route, named after tmux's where tmux has
one and taking its aliases: `list-sessions` (`ls`), `new-session`,
`kill-session`, `new-window`, `kill-window`, `move-window`, `send-keys`,
`paste`, `capture-pane` and so on. `shellctl --help` lists them, and
`shellctl <COMMAND> --help` gives a command's options and the route it calls.
These one-shot commands run on Linux and macOS; on Windows only `attach` does.

`--socket`, `--pidfile` and `--logfile` (or `SHELLD_SOCKET`, `SHELLD_PIDFILE`,
`SHELLD_LOGFILE`) choose the daemon's files; `shelld --help` lists every option
and exit code.

## Build and run

```bash
make                      # bin/shelld, bin/shellctl
make test                 # unit + integration tests
bin/shelld daemonize
curl --unix-socket "$(bin/shelld status | awk '/socket:/{print $2}')" \
  -XPOST -H 'Content-Type: application/json' -d '{"name":"work"}' http://shelld/sessions
```

`make` builds static binaries with the `netgo` tag: no dynamic loader or libc
runs at startup, and the daemon answers its first `/health` about 0.5 ms
sooner than a default `go build`. Outside the Makefile, build or install
with the same tag:
`go install -tags netgo github.com/gurupras/shelld/cmd/shelld@latest`.

## Layout

Reusable libraries:

| Package | What it is |
|---|---|
| `vt` | Terminal emulator core: a port of vte 0.15 (Paul Williams parser, ANSI dispatch, DEC 2026 synchronized updates) and alacritty_terminal 0.26 (`Term`, ring-buffer `Grid`, reflowing resize). Pointer-free 20-byte cells. |
| `uniwidth` | `wcwidth` generated from the Rust `unicode-width` 0.2.2 tables alacritty uses (`go generate ./uniwidth`). |
| `ws` | RFC 6455 WebSocket server upgrade and client handshake over any `net.Conn`. |
| `pty` | Pseudo-terminals: `/dev/ptmx` + setsid/controlling tty (Linux, macOS), ConPTY (Windows). |
| `proctree` | Kill a process and all descendants, including ones that `setsid` away. |

Windows and sessions:

| Package | What it is |
|---|---|
| `shelld` (module root) | Windows (PTY + scrollback + VT grid + inject queue), sessions, manager; ANSI frame/diff rendering (`grid.go`). Usable as a library: see the package documentation. |

Daemon:

| Package | What it is |
|---|---|
| `internal/daemon` | Routes, WebSocket attach tick loop, transport, lifecycle (lockfile, probe, daemonize). |
| `cmd/shelld`, `cmd/shellctl` | The binaries. |

Dependencies: `golang.org/x/sys`, `golang.org/x/term`, `github.com/Microsoft/go-winio` (Windows pipes).

## Tracing

Built normally, shelld contains no tracing code: every instrumentation point
is guarded by a constant that is false without the `shelld_trace` build tag,
and the compiler removes it (the hot paths compile to the same machine code
as before instrumentation). A tracing build records only when asked:

```bash
make trace                                     # bin/shelld-traced, bin/shelld-trace
SHELLD_TRACE=/tmp/shelld.trace bin/shelld-traced run --socket /tmp/s.sock
bin/shelld-trace summary /tmp/shelld.trace     # span latencies, canaries, runtime metrics
bin/shelld-trace echo /tmp/shelld.trace        # client keystroke → echo frame, hop by hop
bin/shelld-trace stalls /tmp/shelld.trace      # runtime stalls and what each thread was doing
bin/shelld-trace timeline /tmp/shelld.trace -at 1234.5
bin/shelld-trace chrome /tmp/shelld.trace -o trace.json   # open in ui.perfetto.dev
```

It records spans for PTY reads and writes, VT lock waits and parsing, tick
loop wake-ups (with the reason and time waited), renders (mode, bytes, grid
generation), WebSocket reads and writes and HTTP requests, each with the OS
thread and CPU; and canaries that measure the Go runtime itself — timer
lateness, netpoller wake latency through a socketpair, GC cycles, and
`runtime/metrics` scheduler latency and GC pauses. `SHELLD_TRACE_RUNTIME=file`
also captures a Go execution trace. Details: `internal/tracing`.

## Behaviour worth knowing

- **Programs are looked up as `os/exec` does.** A bare command name is searched
  only in `PATH`, never in the window's working directory, so a file named
  `bash` committed to a repository does not run instead of the real program.
  `./tool` and `bin/tool` resolve against the working directory. A `PATH` entry
  that is `.`, empty or relative fails window creation when the first match
  comes through it.
- **A window with no `cwd` starts where the daemon was started**, as tmux's
  does, not in `$HOME`. A `cwd` that is not a directory is a 400.
- **A failed create says whose fault it is**: 409 only for a name already in
  use (the body says "already exists"), 400 for a bad `cwd`, `shell` or name,
  and 500 for a host failure such as openpty or the fork.
- **Names may hold any character except a control character**, which is
  refused with a 400 naming its code point. Clients should percent-encode
  names in paths; `/`, `?`, `#` and spaces all work.
- **Queries are read strictly.** An unknown query parameter is ignored, so
  version skew degrades rather than fails, but a near miss of a known key
  (`?Observer=1`, `?offset%3d10`) and an unreadable `observer` value are 400s:
  the silent default would have been a writable attach, or the wrong page.
- **The socket probe fails closed.** `run` refuses to replace a socket it
  cannot prove dead; `stop` and `kill` remove files only when nothing serves
  the socket. Periodic temp cleaners (systemd-tmpfiles, macOS) may remove an
  idle default directory while the daemon runs, so use explicit paths for
  long-lived daemons.
- **On Windows**, raw capture and scroll return conhost's re-rendering of the
  screen, not the program's bytes: long lines are split at the console width
  and each resize adds a copy of the screen. Read the visible capture there
  (`capture?mode=visible`).

## Verification

- **Emulator fidelity.** `vtdiff_test.go` runs the same operation stream (byte
  feeds in random splits, resizes, grid dumps, frames, diffs at every offset and
  crop, OSC colour replies) through this emulator and through the Rust
  `alacritty_terminal`-based implementation, shelld-rs (`testdata/rsdump`), and
  requires identical output, over seeded fuzz of wide and combining characters,
  invalid and split UTF-8, scroll regions, the alternate screen, DEC 2026, SGR
  subparameters, charsets and reflow. It needs cargo and a shelld-rs checkout:

  ```bash
  SHELLD_VTDIFF=1 SHELLD_RS_DIR=/path/to/shelld-rs go test . -run TestVtDiff
  ```

- **Wire compatibility.** `tools/compat/compat.mjs` boots a daemon binary and
  records a normalised transcript of about 60 HTTP requests, including
  malformed bodies, and a WebSocket attach session. Diffing two transcripts
  shows where two builds, or shelld and shelld-rs, answer differently.

## License

MIT. See [LICENSE](LICENSE).
