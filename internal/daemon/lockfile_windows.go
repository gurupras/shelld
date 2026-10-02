//go:build windows

package daemon

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Replacing or deleting a file fails while another process has it open
// without FILE_SHARE_DELETE (Go's os.Open, for one; antivirus scanning a new
// file, too), and on NTFS without POSIX rename semantics even with it. Since
// the daemon refuses to start without its lockfile, these retry with backoff
// for up to five seconds, logging each retry.
const busyBudget = 5 * time.Second

func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func retryBusyFile(op, path string, f func() error) error {
	return retryBusy(busyBudget, isBusy, f, func(attempt int, wait time.Duration, err error) {
		slog.Warn("lockfile busy, retrying", "op", op, "path", path, "attempt", attempt, "wait", wait, "err", err)
	}, time.Sleep)
}

func replaceFile(tmp, path string) error {
	return retryBusyFile("rename", path, func() error { return os.Rename(tmp, path) })
}

func removeFile(path string) error {
	return retryBusyFile("remove", path, func() error { return os.Remove(path) })
}

// readFileShared reads path while letting the daemon replace or delete it.
func readFileShared(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}
