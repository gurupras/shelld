package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// S8: SIGTERM, SIGHUP and SIGQUIT end an attach on a terminal as a detach does,
// as they do in shelld-rs: the terminal is put back out of raw mode, and
// "Detached." is printed. By default they ended the process with the terminal
// left raw. Linux only, for the reason console_linux_test.go gives.
func TestAStopSignalDetachesAndRestoresTheTerminal(t *testing.T) {
	sock := bootDaemon(t)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			master, slave := openPTY(t)
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if before.Lflag&unix.ICANON == 0 {
				t.Fatal("setup: the terminal starts out raw, so a restore cannot be told from none")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, shellctlBin, "--socket", sock, "attach", "s", "w")
			c.Stdin, c.Stdout = slave, slave
			var errb bytes.Buffer
			c.Stderr = &errb
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			go drain(master)

			// Raw mode is entered after the stop signals are registered, so once the
			// terminal is raw a signal is one the client is handling.
			deadline := time.Now().Add(10 * time.Second)
			for {
				tio, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err == nil && tio.Lflag&unix.ICANON == 0 {
					break
				}
				if time.Now().After(deadline) {
					_ = c.Process.Kill()
					_ = c.Wait()
					t.Fatalf("the client never put the terminal in raw mode: %s", errb.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := c.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err = c.Wait()
			after, terr := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if terr != nil {
				t.Fatal(terr)
			}
			if err != nil || !strings.Contains(errb.String(), "Detached.") {
				t.Errorf("after %v: %v, stderr %q; want a clean exit printing Detached.", sig, err, errb.String())
			}
			if after.Lflag&unix.ICANON == 0 || after.Lflag&unix.ECHO == 0 {
				t.Errorf("after %v the terminal is still raw (lflag %#x, was %#x)", sig, after.Lflag, before.Lflag)
			}
		})
	}
}

// openPTY returns a pty master and its slave, opened without becoming this
// process's controlling terminal.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx, so there is no terminal to test against: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// drain reads the master so the client's writes to its terminal never block.
func drain(f *os.File) {
	b := make([]byte, 4096)
	for {
		if _, err := f.Read(b); err != nil {
			return
		}
	}
}
