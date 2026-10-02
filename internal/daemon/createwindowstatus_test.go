package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/pty"
)

// The classification reaches its types through the wrapping NewWindow adds
// ("spawn shell: %w"), and anything it does not recognise is the host's: a
// 500, the side a client retries, never the 400 that tells it to give up.
func TestCreateWindowStatusSplitsByFault(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"name in use", &shelld.NameInUseError{Name: "w"}, http.StatusConflict},
		{"cwd", &shelld.CwdError{Cwd: "/x", Err: errors.New("not a directory")}, http.StatusBadRequest},
		{"program, wrapped", fmt.Errorf("spawn shell: %w", &pty.ProgramError{Err: errors.New("no such file")}), http.StatusBadRequest},
		{"empty command", shelld.ErrEmptyCommand, http.StatusBadRequest},
		{"session killed during the spawn", shelld.ErrSessionKilled, http.StatusNotFound},
		{"host", fmt.Errorf("spawn shell: %w", errors.New("open /dev/ptmx: too many open files")), http.StatusInternalServerError},
	} {
		if got := createWindowStatus(tc.err); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
