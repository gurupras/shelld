package shelld

import (
	"fmt"

	"github.com/gurupras/shelld/vt"
)

// MaxCols and MaxRows bound a window's size: the size it is created with,
// POST …/resize pins, and every attached client's declared size.
//
// The grid is allocated eagerly, so the bound is what keeps one request from
// allocating an arbitrary amount of memory. A vt.Cell is 20 bytes, and both
// the screen and its 10 000 lines of history are stored at full width:
//
//	history + screen   cols × (10 000 + rows) × 20 B   1024 × 11 024 × 20 B ≈ 215 MiB
//	alternate screen   cols × rows × 20 B              1024 × 1024 × 20 B   ≈  20 MiB
//	each snapshot      cols × rows × 16 B (CellSlot)   1024 × 1024 × 16 B   ≈  16 MiB
//
// so a full window at the maximum holds about 250 MiB with one snapshot
// (251.7 MiB measured), plus two snapshots, about 32 MiB, per attached client
// (the last frame sent and a scratch copy). Reflowing that history to fewer
// columns keeps the rows' capacity for widening again, so memory stays near
// the wide size until the history turns over; it does not grow past it.
// Attached views are browser-sized, well below 1024 columns, and both limits
// fit ConPTY's int16 COORD.
const (
	MaxCols = 1024
	MaxRows = 1024
)

// MinCols is the narrowest window: a wide character takes two columns
// (vt.MinColumns, which the grid also enforces). A request for one column is
// refused rather than silently widened; zero still means the default where a
// default exists.
//
// There is deliberately no matching minimum for rows, and the asymmetry is the
// behaviour rather than an oversight. One row is a usable terminal: every cell
// is addressable, the grid scrolls as normal, and the history keeps what
// scrolls off, so there is nothing to refuse. One column is not, because a
// single wide character has nowhere to go. Zero rows is not an error either --
// vt.Term.Resize raises it to one -- which is what lets a client that has not
// sized itself yet send a size at all.
//
// Passing CheckSize is not the same as getting the size asked for. A window
// created at one row is one row, but a *pin* of one row settles at
// minEffectiveRows, because the effective PTY size is floored separately and at
// a much higher 20x5 (Window.RecomputeEffectiveSize, the same floor and the same
// numbers as the reference). So this constant is about what a size request may
// say, not about what the PTY ends up being. TestOneRowAllowed pins both halves
// and TestOneRowWindowWorks pins that a real one-row window runs.
const MinCols = vt.MinColumns

// CheckSize reports a size above MaxCols or MaxRows, or a width of one
// column. NewWindow and Window.Resize return its error; the daemon checks
// requests with it first and answers 400.
//
// The bounds are this port's own hardening: the reference validates size
// nowhere, so nothing here is constrained by parity and a caller that works
// against shelld-rs cannot be broken by a bound being too tight. They exist
// because the grid is allocated eagerly (see MaxCols) and because a
// one-column grid cannot hold the characters it would be asked to.
func CheckSize(cols, rows uint16) error {
	if cols != 0 && cols < MinCols {
		return fmt.Errorf("cols must be at least %d", MinCols)
	}
	if cols > MaxCols {
		return fmt.Errorf("cols must be at most %d", MaxCols)
	}
	if rows > MaxRows {
		return fmt.Errorf("rows must be at most %d", MaxRows)
	}
	return nil
}
