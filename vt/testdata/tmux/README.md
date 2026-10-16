# tmux regress inputs

Inputs from tmux's own regression tests (tmux 3.7c, commit
e476c1230b958df0cb12977517d24b3dc931375b, `regress/`), used here as input streams for
the packed-history tests. tmux's `.result` files are not copied: they record tmux's
behaviour (its width tables, its reflow, the `capture-pane` format), and this terminal's
reference is alacritty, so the tests compare against alacritty and against the unpacked
grid instead.

| file | source |
|---|---|
| `UTF-8-test.txt` | `regress/UTF-8-test.txt`, unchanged (used by `utf8-test.sh`) |
| `cursor-test.txt` | `regress/cursor-test.txt`, unchanged (used by `cursor-test1.sh` and `cursor-test2.sh`) |
| `combine-test.bin` | the bytes the `printf` lines of `regress/combine-test.sh` write |
| `capture-pane-sgr0.bin` | the bytes the `printf` lines of `regress/capture-pane-sgr0.sh` write |

The files hold what the programs wrote, with bare `\n` line ends. A pty's `onlcr` turns
those into `\r\n` before tmux reads them, and the tests apply the same translation.

The cursor-test resize sequences (window sizes and the `resizew` widths) are reproduced
in the test code, which cites the script each one comes from.

## Licences

tmux is distributed under the ISC licence; its notice is in `COPYING.tmux`.

`UTF-8-test.txt` is by Markus Kuhn, "UTF-8 decoder capability and stress test",
2015-08-28, licensed CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/); the
attribution is in the file's own header, which is kept unchanged.
