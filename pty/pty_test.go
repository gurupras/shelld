//go:build linux || darwin

package pty

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readUntil(t *testing.T, p *PTY, want string) string {
	t.Helper()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 4096)
		for {
			n, err := p.Read(b)
			buf.Write(b[:n])
			if strings.Contains(buf.String(), want) || err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q; got %q", want, buf.String())
	}
	return buf.String()
}

func TestStartEchoAndSize(t *testing.T) {
	p, err := Start(Command{Path: "/bin/sh", Args: []string{"sh", "-c", "stty size; echo marker-$((40+2))"}, Env: os.Environ()}, Size{Cols: 91, Rows: 17})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out := readUntil(t, p, "marker-42")
	if !strings.Contains(out, "17 91") {
		t.Fatalf("stty size not 17 91: %q", out)
	}
	_, _ = p.Wait()
}

func TestResizeAndCloseUnblocksRead(t *testing.T) {
	p, err := Start(Command{Path: "sh", Args: []string{"sh"}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Resize(Size{Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, p, "40 120")

	readErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, p)
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = p.Kill()
	_ = p.Close()
	select {
	case <-readErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock a pending Read")
	}
	_, _ = p.Wait()
}

func TestWaitReportsExitCodeAndSignal(t *testing.T) {
	p, err := Start(Command{Path: "/bin/sh", Args: []string{"sh", "-c", "exit 3"}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if st, err := p.Wait(); err != nil || st != (ExitStatus{Code: 3}) {
		t.Fatalf("exit 3: %+v %v", st, err)
	}

	p, err = Start(Command{Path: "/bin/sh", Args: []string{"sh", "-c", "kill -TERM $$; sleep 5"}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if st, err := p.Wait(); err != nil || st != (ExitStatus{Code: -1, Signal: "SIGTERM"}) {
		t.Fatalf("killed: %+v %v", st, err)
	}
}

// H35: a signal the platform has no name for is "signal N", as shelld-rs
// reports it, rather than an empty name that drops the key from exitStatus.
// 36 is a realtime signal on Linux and past the last signal on darwin, so
// neither names it; a named one keeps its name.
func TestWaitNamesAnUnnamedSignalByNumber(t *testing.T) {
	if got := signalName(syscall.Signal(36)); got != "signal 36" {
		t.Errorf("signalName(36) = %q, want %q", got, "signal 36")
	}
	if got := signalName(syscall.SIGIO); got != "SIGIO" {
		t.Errorf("signalName(SIGIO) = %q, want SIGIO", got)
	}
	if runtime.GOOS != "linux" {
		return // darwin cannot send a signal number it does not have
	}
	p, err := Start(Command{Path: "/bin/sh", Args: []string{"sh", "-c", "exec sleep 30"}, Env: os.Environ()}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := syscall.Kill(p.Pid(), syscall.Signal(36)); err != nil {
		t.Fatal(err)
	}
	if st, err := p.Wait(); err != nil || st != (ExitStatus{Code: -1, Signal: "signal 36"}) {
		t.Fatalf("killed by 36: %+v %v", st, err)
	}
}
