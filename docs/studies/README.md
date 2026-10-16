# Studies

Investigations into shelld's cost against tmux, kept for their measurements. A study is either
adopted, and then the code is in the tree, or written down here so the same ground is not
covered twice.

- [memory.md](memory.md): per-window memory against tmux. Packed history was adopted; the
  candidates that were not, and why.
- [near-idle.md](near-idle.md): CPU per keystroke. Damage-stamped frames (B) were adopted; direct
  PTY writes (A), inline rendering (C), raw-syscall reads (D) and their combinations were not.
- [read-wait.md](read-wait.md): waiting 100 µs after a short PTY read to batch a redraw. Not
  adopted.
