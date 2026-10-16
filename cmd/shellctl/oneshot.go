package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/sock"
)

// The one-shot commands. Each is one HTTP request to a route the daemon already
// serves, made by hand over the socket: HTTP/1.0, so the response is never
// chunked and the connection ends with it, and no net/http client, whose
// transport machinery is the larger part of a lean client's startup.
//
// Names follow tmux where tmux has the command (list-sessions, new-window,
// send-keys), with tmux's aliases; <SESSION> <WINDOW> are positional, as for
// attach, and options take the route's own vocabulary (lines, offset,
// max_bytes, enter_delay_ms), so a reader of the HTTP API can read the command
// line. Every route but attach's is reached by exactly one command; capture
// and scroll are also both reached by capture-pane.

// optKind is what an option takes, and so how its value is checked before
// anything is sent.
type optKind int

const (
	optFlag   optKind = iota // no value
	optUint                  // a non-negative integer
	optInt                   // an integer, either sign
	optString                // any text; the last one given wins
	optList                  // any text, repeatable; every one is kept
)

// subOptions lists each subcommand's options and what each takes. A
// subcommand absent from it does not exist.
var subOptions = map[string]map[string]optKind{
	"attach":        {},
	"capture":       {"--visible": optFlag, "--lines": optUint, "--max-bytes": optUint, "--json": optFlag},
	"scroll":        {"--offset": optUint, "--lines": optUint, "--max-bytes": optUint, "--json": optFlag},
	"capture-pane":  {"--visible": optFlag, "--offset": optUint, "-S": optUint, "--lines": optUint, "--max-bytes": optUint, "--json": optFlag},
	"frame":         {"--json": optFlag},
	"send-keys":     {"--no-enter": optFlag, "--enter-delay-ms": optUint, "--key-names": optFlag, "-K": optFlag, "--strict": optFlag, "--hex": optFlag, "-H": optFlag},
	"paste":         {"--mode": optString, "--literal": optFlag},
	"inject":        {"--priority": optInt},
	"inject-queue":  {},
	"health":        {},
	"kill-server":   {},
	"list-sessions": {},
	"new-session":   {},
	"show-session":  {},
	"kill-session":  {},
	"list-windows":  {},
	"new-window": {"--cwd": optString, "-c": optString, "--shell": optString, "--cols": optUint, "--rows": optUint,
		"--env": optList, "-e": optList, "--unset-env": optList, "--paste-mode": optString},
	"kill-window":   {},
	"move-window":   {"--name": optString},
	"resize-window": {},
	"set-colors":    {"--background": optString, "--foreground": optString, "--cursor": optString, "--palette": optList},
}

// shortOptions maps each short option to the long one it spells.
var shortOptions = map[string]string{"-S": "--offset", "-K": "--key-names", "-H": "--hex", "-c": "--cwd", "-e": "--env"}

// aliases are tmux's short names for the commands it has, and shutdown.
var aliases = map[string]string{
	"ls": "list-sessions", "new": "new-session", "has": "show-session", "lsw": "list-windows",
	"neww": "new-window", "killw": "kill-window", "movew": "move-window", "resizew": "resize-window",
	"send": "send-keys", "capturep": "capture-pane", "shutdown": "kill-server",
}

// canonical is the command an alias names, or name itself.
func canonical(name string) string {
	if c, ok := aliases[name]; ok {
		return c
	}
	return name
}

// subArgs is each subcommand's positional arguments. All are required, except
// that a last one in brackets may be left out and a last one ending in "..."
// takes one or more.
var subArgs = map[string][]string{
	"attach":        {"<SESSION>", "<WINDOW>"},
	"capture":       {"<SESSION>", "<WINDOW>"},
	"scroll":        {"<SESSION>", "<WINDOW>"},
	"capture-pane":  {"<SESSION>", "<WINDOW>"},
	"frame":         {"<SESSION>", "<WINDOW>"},
	"send-keys":     {"<SESSION>", "<WINDOW>", "<KEYS>..."},
	"paste":         {"<SESSION>", "<WINDOW>", "[TEXT]"},
	"inject":        {"<SESSION>", "<WINDOW>", "<PAYLOAD>"},
	"inject-queue":  {"<SESSION>", "<WINDOW>"},
	"health":        {},
	"kill-server":   {},
	"list-sessions": {},
	"new-session":   {"<NAME>"},
	"show-session":  {"<SESSION>"},
	"kill-session":  {"<SESSION>"},
	"list-windows":  {"<SESSION>"},
	"new-window":    {"<SESSION>", "<NAME>"},
	"kill-window":   {"<SESSION>", "<WINDOW>"},
	"move-window":   {"<SESSION>", "<WINDOW>", "<DST_SESSION>"},
	"resize-window": {"<SESSION>", "<WINDOW>", "<COLS>", "<ROWS>"},
	"set-colors":    {"<SESSION>", "<WINDOW>"},
}

// argBounds is how many positional arguments args allows: max is -1 for no
// limit.
func argBounds(args []string) (min, max int) {
	if len(args) == 0 {
		return 0, 0
	}
	switch last := args[len(args)-1]; {
	case strings.HasPrefix(last, "["):
		return len(args) - 1, len(args)
	case strings.HasSuffix(last, "..."):
		return len(args), -1
	}
	return len(args), len(args)
}

var subUsage = map[string]string{
	"attach": `Attach to a window: stdin/stdout piped over WebSocket

Usage: shellctl attach <SESSION> <WINDOW>
`,
	"capture": `Print a window's output: raw history, or the visible screen

GET /sessions/<SESSION>/windows/<WINDOW>/capture. Prints the "output" field as
text, or the whole JSON response with --json.

Usage: shellctl capture [OPTIONS] <SESSION> <WINDOW>

Options:
      --visible          The visible screen instead of raw history (mode=visible)
      --lines <N>        Lines to return, newest last (default 50)
      --max-bytes <N>    Byte bound on raw history (default: the daemon's)
      --json             Print the JSON response instead of its output
`,
	"scroll": `Print a page of a window's raw history, counted from the oldest line

GET /sessions/<SESSION>/windows/<WINDOW>/scroll. Prints the "output" field as
text, or the whole JSON response with --json.

Usage: shellctl scroll [OPTIONS] <SESSION> <WINDOW>

Options:
      --offset <N>       First line, 0 being the oldest retained (default 0)
      --lines <N>        Lines to return (default 50)
      --max-bytes <N>    Byte bound (default: the daemon's)
      --json             Print the JSON response instead of its output
`,
	"capture-pane": `Print a window's output: capture, or scroll with an offset (alias: capturep)

GET /sessions/<SESSION>/windows/<WINDOW>/capture, or .../scroll when --offset
is given. Prints the "output" field as text, or the whole JSON response with
--json. Unlike tmux's -S, the offset counts from the oldest line retained.

Usage: shellctl capture-pane [OPTIONS] <SESSION> <WINDOW>

Options:
      --visible          The visible screen instead of raw history (mode=visible)
  -S, --offset <N>       Page from line N, 0 being the oldest retained (scroll)
      --lines <N>        Lines to return (default 50)
      --max-bytes <N>    Byte bound on raw history (default: the daemon's)
      --json             Print the JSON response instead of its output
`,
	"frame": `Print a window's screen as the escape sequences that repaint it

GET /sessions/<SESSION>/windows/<WINDOW>/frame. Prints the "frame" field, or
the whole JSON response (frame, cols, rows) with --json.

Usage: shellctl frame [OPTIONS] <SESSION> <WINDOW>

Options:
      --json             Print the JSON response instead of the frame
`,
	"send-keys": `Type text, tmux key names or raw bytes into a window (alias: send)

POST /sessions/<SESSION>/windows/<WINDOW>/keys. By default KEYS is one argument
sent as text, then Enter unless --no-enter. Unlike tmux send-keys, that text is
typed as it is: a key name such as C-c or Enter is not looked up, and KEYS must
be valid UTF-8. Put -- before a KEYS that begins with '-'.

With -K, each KEYS is a tmux key name (Enter, C-c, Up, F5) or, if it is not
one, text, as tmux send-keys reads them: POST .../key-names. No Enter is added.

With -H, each KEYS is hex (1b, or 1b5b41) and the bytes are written as they
are, as tmux send-keys -H writes them: POST .../raw-key. No Enter is added. The
route carries a JSON string, so the bytes must be valid UTF-8.

Usage: shellctl send-keys [OPTIONS] <SESSION> <WINDOW> <KEYS>...

Options:
      --no-enter               Do not press Enter after the text
      --enter-delay-ms <MS>    Pause between the text and Enter
  -K, --key-names              Each KEYS is a tmux key name
      --strict                 With -K, refuse an unknown or lossy name and send nothing
  -H, --hex                    Each KEYS is hex bytes
`,
	"paste": `Paste text into a window, then press Enter

POST /sessions/<SESSION>/windows/<WINDOW>/paste. TEXT is the argument, or
stdin when it is left out, read to its end and sent whole, a final newline
included; it must be valid UTF-8.

Usage: shellctl paste [OPTIONS] <SESSION> <WINDOW> [TEXT]

Options:
      --mode <MODE>      auto, bracketed or raw (default: the window's pasteMode)
      --literal          Write the bytes unchanged instead of sanitizing them
`,
	"inject": `Queue a payload to be typed when the window is next idle

POST /sessions/<SESSION>/windows/<WINDOW>/inject. Prints the JSON response
(id, position, queueLength).

Usage: shellctl inject [OPTIONS] <SESSION> <WINDOW> <PAYLOAD>

Options:
      --priority <N>     Higher is delivered first (default 0)
`,
	"inject-queue": `Print a window's inject queue (GET .../inject/queue, JSON)

Usage: shellctl inject-queue <SESSION> <WINDOW>
`,
	"health": `Print the daemon's health report (GET /health, JSON)

Usage: shellctl health
`,
	"kill-server": `Shut the daemon down (POST /shutdown) (alias: shutdown)

Usage: shellctl kill-server
`,
	"list-sessions": `List sessions and their windows (GET /sessions, JSON) (alias: ls)

Usage: shellctl list-sessions
`,
	"new-session": `Create a session (POST /sessions) (alias: new)

Usage: shellctl new-session <NAME>
`,
	"show-session": `Print whether a session exists (GET /sessions/<SESSION>, JSON) (alias: has)

Exits 1 when it does not, as tmux has-session does.

Usage: shellctl show-session <SESSION>
`,
	"kill-session": `Delete a session and its windows (DELETE /sessions/<SESSION>)

It fails for a session that does not exist, as tmux's kill-session does.

Usage: shellctl kill-session <SESSION>
`,
	"list-windows": `List a session's windows (GET /sessions/<SESSION>/windows, JSON) (alias: lsw)

Usage: shellctl list-windows <SESSION>
`,
	"new-window": `Create a window (POST /sessions/<SESSION>/windows) (alias: neww)

Prints the JSON response (ok, pid).

Usage: shellctl new-window [OPTIONS] <SESSION> <NAME>

Options:
  -c, --cwd <DIR>             Start directory (default: the daemon's)
      --shell <CMD>           Command line to run (default: the daemon's --shell)
      --cols <N>              Width (default: the daemon's)
      --rows <N>              Height (default: the daemon's)
  -e, --env <KEY=VALUE>       Set a variable; repeatable
      --unset-env <KEY>       Remove an inherited variable; repeatable
      --paste-mode <MODE>     auto, bracketed or raw (default auto)
`,
	"kill-window": `Delete a window (DELETE /sessions/<SESSION>/windows/<WINDOW>) (alias: killw)

It fails for a window that does not exist, as tmux's kill-window does.

Usage: shellctl kill-window <SESSION> <WINDOW>
`,
	"move-window": `Move a window to another session (POST /move-window) (alias: movew)

DST_SESSION is created if it does not exist.

Usage: shellctl move-window [OPTIONS] <SESSION> <WINDOW> <DST_SESSION>

Options:
      --name <NAME>      The window's name there (default: its name now)
`,
	"resize-window": `Resize a window (POST /sessions/<SESSION>/windows/<WINDOW>/resize) (alias: resizew)

Usage: shellctl resize-window <SESSION> <WINDOW> <COLS> <ROWS>
`,
	"set-colors": `Set a window's colours (POST /sessions/<SESSION>/windows/<WINDOW>/colors)

Each colour is #rrggbb or rrggbb. The daemon applies those it can read and
refuses the request only when it can read none.

Usage: shellctl set-colors [OPTIONS] <SESSION> <WINDOW>

Options:
      --background <HEX>      Default background
      --foreground <HEX>      Default foreground
      --cursor <HEX>          Cursor colour
      --palette <N=HEX>       Palette entry N (0-255); repeatable
`,
}

// usageErr is a command line the subcommand refuses; it exits 2, as clap does.
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

// textField is the field a command prints as text unless --json is given.
// Commands absent from it print the JSON response, or, when quiet, nothing.
var textField = map[string]string{"capture": "output", "scroll": "output", "capture-pane": "output", "frame": "frame"}

// quiet lists the commands whose response is only {"ok": true}: they print
// nothing, and their exit code is the answer.
var quiet = map[string]bool{
	"send-keys": true, "paste": true, "kill-server": true, "new-session": true, "kill-session": true,
	"kill-window": true, "move-window": true, "resize-window": true, "set-colors": true,
}

// oneShot runs a one-shot subcommand and writes its answer to w; stdin is
// paste's text when no TEXT is given. A refusal by the daemon is returned as
// the error, naming the route and status.
func oneShot(w io.Writer, stdin io.Reader, socket, cmd string, pos, opts []string) error {
	vals := map[string][]string{}
	for i := 0; i < len(opts); i++ {
		name, val, hasVal := strings.Cut(opts[i], "=")
		kind := subOptions[cmd][name]
		if kind != optFlag && !hasVal {
			i++
			val = opts[i]
		} else if kind == optFlag && hasVal {
			return usageErr{fmt.Sprintf("unexpected value '%s' for '%s'", val, name)}
		}
		if _, err := strconv.ParseUint(val, 10, 64); kind == optUint && err != nil {
			return usageErr{fmt.Sprintf("invalid value '%s' for '%s': not a non-negative integer", val, name)}
		}
		if _, err := strconv.ParseInt(val, 10, 64); kind == optInt && err != nil {
			return usageErr{fmt.Sprintf("invalid value '%s' for '%s': not an integer", val, name)}
		}
		if long, ok := shortOptions[name]; ok {
			name = long
		}
		vals[name] = append(vals[name], val)
	}
	has := func(opt string) bool { _, ok := vals[opt]; return ok }
	// last is an option's value, the last one given winning as with any
	// option parser.
	last := func(opt string) string {
		if v := vals[opt]; len(v) > 0 {
			return v[len(v)-1]
		}
		return ""
	}
	var method, path string
	var body any
	q := url.Values{}
	num := func(opt, key string) {
		if has(opt) {
			q.Set(key, last(opt))
		}
	}
	sess := func() string { return "/sessions/" + url.PathEscape(pos[0]) }
	win := func() string { return sess() + "/windows/" + url.PathEscape(pos[1]) }
	// text refuses what a JSON string cannot carry: encoding/json would turn each
	// invalid byte into U+FFFD, and the window would be typed bytes nobody gave.
	text := func(arg, s string) error {
		if !utf8.ValidString(s) {
			return usageErr{fmt.Sprintf("invalid value for '%s': not valid UTF-8", arg)}
		}
		return nil
	}
	if cmd == "capture-pane" {
		if !has("--offset") {
			cmd = "capture"
		} else if has("--visible") {
			return usageErr{"'--visible' cannot be used with '--offset': the visible screen has no offset"}
		} else {
			cmd = "scroll"
		}
	}
	switch cmd {
	case "health":
		method, path = "GET", "/health"
	case "kill-server":
		method, path = "POST", "/shutdown"
	case "list-sessions":
		method, path = "GET", "/sessions"
	case "new-session":
		method, path = "POST", "/sessions"
		body = map[string]string{"name": pos[0]}
	case "show-session":
		method, path = "GET", sess()
	case "kill-session":
		method, path = "DELETE", sess()
	case "list-windows":
		method, path = "GET", sess()+"/windows"
	case "new-window":
		method, path = "POST", sess()+"/windows"
		req := map[string]any{"name": pos[1]}
		for opt, key := range map[string]string{"--cwd": "cwd", "--shell": "shell", "--paste-mode": "pasteMode"} {
			if has(opt) {
				req[key] = last(opt)
			}
		}
		for opt, key := range map[string]string{"--cols": "cols", "--rows": "rows"} {
			if has(opt) {
				req[key] = json.Number(last(opt))
			}
		}
		for _, kv := range vals["--env"] {
			if !strings.Contains(kv, "=") {
				return usageErr{fmt.Sprintf("invalid value '%s' for '--env': want KEY=VALUE", kv)}
			}
		}
		if has("--env") {
			req["env"] = vals["--env"]
		}
		if has("--unset-env") {
			req["unsetEnv"] = vals["--unset-env"]
		}
		body = req
	case "kill-window":
		method, path = "DELETE", win()
	case "move-window":
		method, path = "POST", "/move-window"
		req := map[string]string{"srcSession": pos[0], "srcName": pos[1], "dstSession": pos[2]}
		if has("--name") {
			req["dstName"] = last("--name")
		}
		body = req
	case "resize-window":
		method, path = "POST", win()+"/resize"
		for i, arg := range []string{"<COLS>", "<ROWS>"} {
			if _, err := strconv.ParseUint(pos[2+i], 10, 64); err != nil {
				return usageErr{fmt.Sprintf("invalid value '%s' for '%s': not a non-negative integer", pos[2+i], arg)}
			}
		}
		body = map[string]json.Number{"cols": json.Number(pos[2]), "rows": json.Number(pos[3])}
	case "set-colors":
		method, path = "POST", win()+"/colors"
		req := map[string]any{}
		for _, key := range []string{"background", "foreground", "cursor"} {
			if has("--" + key) {
				req[key] = last("--" + key)
			}
		}
		if has("--palette") {
			palette := map[string]string{}
			for _, e := range vals["--palette"] {
				n, hex, ok := strings.Cut(e, "=")
				if !ok {
					return usageErr{fmt.Sprintf("invalid value '%s' for '--palette': want N=HEX", e)}
				}
				palette[n] = hex
			}
			req["palette"] = palette
		}
		body = req
	case "capture":
		method, path = "GET", win()+"/capture"
		if has("--visible") {
			q.Set("mode", "visible")
		}
		num("--lines", "lines")
		num("--max-bytes", "max_bytes")
	case "scroll":
		method, path = "GET", win()+"/scroll"
		num("--offset", "offset")
		num("--lines", "lines")
		num("--max-bytes", "max_bytes")
	case "frame":
		method, path = "GET", win()+"/frame"
	case "send-keys":
		keys := pos[2:]
		switch {
		case has("--key-names") && has("--hex"):
			return usageErr{"'--key-names' cannot be used with '--hex'"}
		case (has("--key-names") || has("--hex")) && (has("--no-enter") || has("--enter-delay-ms")):
			return usageErr{"'--no-enter' and '--enter-delay-ms' apply only to text: -K and -H add no Enter"}
		case has("--strict") && !has("--key-names"):
			return usageErr{"'--strict' applies only with '--key-names'"}
		case has("--key-names"):
			method, path = "POST", win()+"/key-names"
			for _, k := range keys {
				if err := text("<KEYS>", k); err != nil {
					return err
				}
			}
			body = map[string]any{"names": keys, "strict": has("--strict")}
		case has("--hex"):
			method, path = "POST", win()+"/raw-key"
			var raw []byte
			for _, k := range keys {
				b, err := hex.DecodeString(k)
				if err != nil {
					return usageErr{fmt.Sprintf("invalid value '%s' for '<KEYS>': not hex bytes", k)}
				}
				raw = append(raw, b...)
			}
			if err := text("<KEYS>", string(raw)); err != nil {
				return err
			}
			body = map[string]string{"key": string(raw)}
		default:
			method, path = "POST", win()+"/keys"
			if len(keys) > 1 {
				return usageErr{fmt.Sprintf("unexpected argument '%s' found: text is one argument; quote it, or use -K", keys[1])}
			}
			if err := text("<KEYS>", keys[0]); err != nil {
				return err
			}
			req := struct {
				Keys  string  `json:"keys"`
				Enter bool    `json:"enter"`
				Delay *uint64 `json:"enter_delay_ms,omitempty"`
			}{Keys: keys[0], Enter: !has("--no-enter")}
			if has("--enter-delay-ms") {
				d, _ := strconv.ParseUint(last("--enter-delay-ms"), 10, 64)
				req.Delay = &d
			}
			body = req
		}
	case "paste":
		method, path = "POST", win()+"/paste"
		var s string
		if len(pos) > 2 {
			s = pos[2]
		} else {
			b, err := io.ReadAll(stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			s = string(b)
		}
		if err := text("[TEXT]", s); err != nil {
			return err
		}
		req := map[string]any{"text": s}
		if has("--mode") {
			req["mode"] = last("--mode")
		}
		if has("--literal") {
			req["literal"] = true
		}
		body = req
	case "inject":
		method, path = "POST", win()+"/inject"
		if err := text("<PAYLOAD>", pos[2]); err != nil {
			return err
		}
		req := map[string]any{"payload": pos[2]}
		if has("--priority") {
			req["priority"] = json.Number(last("--priority"))
		}
		body = req
	case "inject-queue":
		method, path = "GET", win()+"/inject/queue"
	default:
		return usageErr{fmt.Sprintf("unrecognized subcommand '%s'", cmd)}
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	status, resp, err := roundTrip(socket, method, path, raw)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		msg := strings.TrimSpace(string(resp))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(resp, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, msg)
	}
	field, isText := textField[cmd]
	switch {
	case quiet[cmd]:
		return nil
	case !isText || has("--json"):
		return writeLine(w, resp)
	}
	var out struct {
		Truncated bool `json:"truncated"`
	}
	var fields map[string]json.RawMessage
	var s string
	if json.Unmarshal(resp, &fields) != nil || json.Unmarshal(resp, &out) != nil ||
		fields[field] == nil || string(fields[field]) == "null" || json.Unmarshal(fields[field], &s) != nil {
		return fmt.Errorf("%s %s: response has no %q: %.200q", method, path, field, resp)
	}
	if err := writeLine(w, []byte(s)); err != nil {
		return err
	}
	if out.Truncated {
		// Text has nowhere to say it, so the bound is named where a caller
		// reading only stdout would otherwise take a short read as the whole.
		fmt.Fprintln(os.Stderr, "shellctl: output truncated at the byte bound; raise --max-bytes")
	}
	return nil
}

// oneShotUnsupported refuses the one-shot commands on a platform their tests
// have not run on. linux and darwin have; windows reaches the daemon over a
// named pipe the tests do not cover yet. attach, which these commands do not
// change, runs everywhere.
func oneShotUnsupported(cmd, goos string) error {
	if goos != "windows" {
		return nil
	}
	return fmt.Errorf("shellctl %s is not supported on %s yet; use the HTTP API", cmd, goos)
}

// writeLine writes b, ending it with a newline if it is not empty and has none,
// so the next prompt starts on a line of its own.
func writeLine(w io.Writer, b []byte) error {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	_, err := w.Write(b)
	return err
}

// oneShotTimeout bounds a one-shot exchange once connected. The daemon answers
// these routes without waiting on the window's program, so only a wedged
// daemon reaches it, and without it the command would wait as long as the
// daemon did.
var oneShotTimeout = 30 * time.Second

// roundTrip sends one HTTP/1.0 request and returns the status and body.
//
// The request is assembled by hand, as ws.Client assembles the attach request,
// so path must already be escaped: the session and window names are, through
// url.PathEscape, and the query through url.Values. A control character left in
// it would end the request line; the check below refuses one rather than trusting
// every caller.
func roundTrip(socket, method, path string, body []byte) (int, []byte, error) {
	if i := strings.IndexFunc(path, func(r rune) bool { return r <= ' ' || r == 0x7f }); i >= 0 {
		return 0, nil, fmt.Errorf("path contains %q at byte %d", path[i], i)
	}
	nc, err := sock.Dial(context.Background(), socket, 5*time.Second)
	if err != nil {
		return 0, nil, fmt.Errorf("connect to %s: %w", socket, err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(oneShotTimeout))
	req := make([]byte, 0, 128+len(path)+len(body))
	req = append(req, method...)
	req = append(req, ' ')
	req = append(req, path...)
	req = append(req, " HTTP/1.0\r\nHost: shelld\r\n"...)
	if body != nil {
		req = append(req, "Content-Type: application/json\r\nContent-Length: "...)
		req = strconv.AppendInt(req, int64(len(body)), 10)
		req = append(req, "\r\n"...)
	}
	req = append(req, "\r\n"...)
	req = append(req, body...)
	if _, err := nc.Write(req); err != nil {
		return 0, nil, fmt.Errorf("send request: %w", timedOut(err))
	}
	status, b, err := readResponse(bufio.NewReader(nc))
	return status, b, timedOut(err)
}

// timedOut names the bound when err is the connection's deadline.
//
// It asks the error whether it is a timeout rather than comparing it with
// os.ErrDeadlineExceeded, because a Windows pipe does not return that: go-winio
// reports a passed deadline as winio.ErrTimeout, a type of its own whose
// Timeout method says so.
func timedOut(err error) error {
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return fmt.Errorf("%w: the daemon did not answer within %v", err, oneShotTimeout)
	}
	return err
}

// readResponse reads an HTTP/1.x response to a 1.0 request: a status line,
// headers, and a body that Content-Length bounds or the connection's end does.
func readResponse(br *bufio.Reader) (int, []byte, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	proto, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	code, _, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(code)
	if !strings.HasPrefix(proto, "HTTP/1.") || err != nil || status < 100 || status > 999 {
		return 0, nil, fmt.Errorf("read response: malformed status line %q", line)
	}
	length := int64(-1)
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, fmt.Errorf("read response headers: %w", err)
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, _ := strings.Cut(h, ":")
		v = strings.TrimSpace(v)
		switch {
		case strings.EqualFold(k, "Content-Length"):
			if length, err = strconv.ParseInt(v, 10, 64); err != nil || length < 0 {
				return 0, nil, fmt.Errorf("read response: bad Content-Length %q", v)
			}
		case strings.EqualFold(k, "Transfer-Encoding") && !strings.EqualFold(v, "identity"):
			// Not sent in answer to HTTP/1.0; reading it as the body would print
			// chunk sizes into the output.
			return 0, nil, fmt.Errorf("read response: unsupported Transfer-Encoding %q", v)
		}
	}
	// Read rather than allocated up front, so a Content-Length is never trusted
	// with the size of an allocation.
	body := io.Reader(br)
	if length >= 0 {
		body = io.LimitReader(br, length)
	}
	b, err := io.ReadAll(body)
	if err == nil && length >= 0 && int64(len(b)) < length {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read response body: %w", err)
	}
	return status, b, nil
}
