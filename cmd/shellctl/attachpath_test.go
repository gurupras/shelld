package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// attachNamed is runAttach with the two names as arguments instead of "s" and "w",
// and with stderr returned rather than logged, because a failure to reach the
// window is the thing being measured here.
func attachNamed(t *testing.T, sock, session, window string, d time.Duration) (string, string) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), d+5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shellctlBin, "--socket", sock, "attach", session, window)
	c.Stdin = pr
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	time.Sleep(d)
	_ = c.Process.Kill()
	// c.Wait rather than Process.Wait, so the goroutine copying into these
	// buffers is joined before they are read; see runAttach's note.
	_ = c.Wait()
	return out.String(), errb.String()
}

// `shellctl attach` builds its path by interpolating the session and window names,
// and neither is a path segment until it is made one: shelld restricts neither, so
// a slash in either asks a different route and a space reaches ws.Client, which
// assembles the request line by hand.
//
// Both names are varied together because they are two separate interpolations and
// one of them being escaped would pass a test that only varied the other.
//
// Attaching is the assertion rather than an error string: stdout carries what the
// window drew, so a non-empty stdout means the client reached the right window on
// the right session. A wrong route answers 404 and the handshake fails, leaving
// stdout empty.
func TestAttachReachesAWindowWhoseNamesAreNotPathSegments(t *testing.T) {
	sock := bootDaemon(t)
	for _, name := range []string{
		"my project",
		"a/b",
		"a?b",
		"a%b",
		"~/src/shelld",
	} {
		t.Run(name, func(t *testing.T) {
			post(t, sock, "/sessions", `{"name":`+strconv.Quote(name)+`}`)
			post(t, sock, "/sessions/"+url.PathEscape(name)+"/windows",
				`{"name":`+strconv.Quote(name)+`,"cols":80,"rows":24,"shell":`+strconv.Quote(catShell())+`}`)

			out, errs := attachNamed(t, sock, name, name, 2*time.Second)
			if out == "" {
				t.Errorf("attach %q %q wrote nothing to stdout, so it never "+
					"reached the window; stderr: %s", name, name, errs)
			}
			// The two ways the unescaped path failed: a route that did not match,
			// and a path ws.Client now refuses to put in a request line.
			for _, frag := range []string{"not found", "must be URL-escaped", "handshake"} {
				if strings.Contains(errs, frag) {
					t.Errorf("attach %q %q: stderr holds %q: %s", name, name, frag, errs)
				}
			}
		})
	}
}
