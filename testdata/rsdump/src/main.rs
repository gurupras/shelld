//! Op stream (little-endian):
//!   header  u16 cols, u16 rows
//!   'F' u32 len, bytes        advance
//!   'R' u16 cols, u16 rows    resize
//!   'D'                       dump grid + render_frame
//!   'A' u32 offset            render_frame_at
//!   'C' u16 cols, u16 rows    render_frame_cropped
//!   'P' u8 slot, u32 offset, u16 cols, u16 rows
//!                             render_diff_at_cropped against slot, store result
//!   'Q'                       take OSC replies
//!   'K' u16 index, u8 r, g, b set reported color
use alacritty_terminal::grid::Dimensions;
use alacritty_terminal::index::{Column, Line};
use alacritty_terminal::term::cell::Flags;
use alacritty_terminal::vte::ansi::{Color, Rgb};
use shelld_core::{GridSnapshot, VtGrid};
use std::io::{BufWriter, Read, Write};

fn hex(b: &[u8]) -> String {
    let mut s = String::with_capacity(b.len() * 2);
    for x in b {
        s.push_str(&format!("{:02x}", x));
    }
    s
}

fn color(c: Color) -> String {
    match c {
        Color::Named(n) => format!("N{}", n as usize),
        Color::Indexed(i) => format!("I{}", i),
        Color::Spec(Rgb { r, g, b }) => format!("R{:02x}{:02x}{:02x}", r, g, b),
    }
}

struct Rd<'a> {
    b: &'a [u8],
    i: usize,
}
impl<'a> Rd<'a> {
    fn u8(&mut self) -> u8 {
        self.i += 1;
        self.b[self.i - 1]
    }
    fn u16(&mut self) -> u16 {
        let v = u16::from_le_bytes([self.b[self.i], self.b[self.i + 1]]);
        self.i += 2;
        v
    }
    fn u32(&mut self) -> u32 {
        let v = u32::from_le_bytes(self.b[self.i..self.i + 4].try_into().unwrap());
        self.i += 4;
        v
    }
}

fn dump_grid(g: &VtGrid, w: &mut impl Write) {
    let term = g.term();
    let grid = term.grid();
    let cur = &grid.cursor;
    let saved = &grid.saved_cursor;
    writeln!(
        w,
        "grid {} {} hist={} cur={},{},{} saved={},{} mode={:x}",
        grid.columns(),
        grid.screen_lines(),
        grid.history_size(),
        cur.point.line.0,
        cur.point.column.0,
        cur.input_needs_wrap as u8,
        saved.point.line.0,
        saved.point.column.0,
        term.mode().bits() & 0x3ffff
    )
    .unwrap();
    let top = -(grid.history_size() as i32);
    for l in top..grid.screen_lines() as i32 {
        let row = &grid[Line(l)];
        let mut s = String::new();
        for c in 0..grid.columns() {
            let cell = &row[Column(c)];
            let mut flags = cell.flags.bits();
            if cell.zerowidth().map_or(false, |z| !z.is_empty()) {
                flags |= 1 << 15;
            }
            let ul = cell.underline_color().map_or("-".to_string(), color);
            // Omit default cells to keep dumps small.
            if cell.c == ' ' && flags == 0 && ul == "-"
                && cell.fg == Color::Named(alacritty_terminal::vte::ansi::NamedColor::Foreground)
                && cell.bg == Color::Named(alacritty_terminal::vte::ansi::NamedColor::Background)
            {
                continue;
            }
            s.push_str(&format!(
                " {}:{:x}.{}.{}.{:x}.{}",
                c,
                cell.c as u32,
                color(cell.fg),
                color(cell.bg),
                flags & !(Flags::empty().bits()),
                ul
            ));
        }
        writeln!(w, "L{}{}", l, s).unwrap();
    }
    writeln!(w, "frame {}", hex(g.render_frame().as_bytes())).unwrap();
}

fn snap_summary(s: &GridSnapshot) -> String {
    let mut h: u64 = 1469598103934665603;
    for c in &s.cells {
        for v in [c.ch as u64, c.wide as u64, c.wide_spacer as u64] {
            h ^= v;
            h = h.wrapping_mul(1099511628211);
        }
    }
    format!(
        "{}x{} cur={},{} vis={} cells={:x}",
        s.cols, s.rows, s.cursor.0, s.cursor.1, s.cursor_visible as u8, h
    )
}

fn main() {
    let mut input = Vec::new();
    std::io::stdin().read_to_end(&mut input).unwrap();
    let out = std::io::stdout();
    let mut w = BufWriter::new(out.lock());
    let mut r = Rd { b: &input, i: 0 };
    let cols = r.u16();
    let rows = r.u16();
    let mut g = VtGrid::new(cols, rows);
    let mut slots: Vec<GridSnapshot> = (0..4).map(|_| GridSnapshot::empty()).collect();
    while r.i < input.len() {
        match r.u8() {
            b'F' => {
                let n = r.u32() as usize;
                g.advance(&input[r.i..r.i + n]);
                r.i += n;
            }
            b'R' => {
                let (c, rr) = (r.u16(), r.u16());
                g.resize(c, rr);
            }
            b'D' => dump_grid(&g, &mut w),
            b'A' => {
                let off = r.u32();
                writeln!(w, "frame_at {} {}", off, hex(g.render_frame_at(off).as_bytes())).unwrap();
            }
            b'C' => {
                let (c, rr) = (r.u16(), r.u16());
                writeln!(w, "cropped {}x{} {}", c, rr, hex(g.render_frame_cropped(c, rr).as_bytes())).unwrap();
            }
            b'P' => {
                let slot = r.u8() as usize;
                let off = r.u32();
                let (c, rr) = (r.u16(), r.u16());
                let (s, snap) = g.render_diff_at_cropped(&slots[slot], off, c, rr);
                writeln!(w, "diff {} {} {}x{} {} {}", slot, off, c, rr, hex(s.as_bytes()), snap_summary(&snap)).unwrap();
                slots[slot] = snap;
            }
            b'Q' => {
                let o = g.take_osc_replies().unwrap_or_default();
                writeln!(w, "osc {}", hex(&o)).unwrap();
            }
            b'K' => {
                let idx = r.u16() as usize;
                let (rr, gg, bb) = (r.u8(), r.u8(), r.u8());
                g.colors().set(idx, Rgb { r: rr, g: gg, b: bb });
            }
            op => panic!("bad op {op}"),
        }
    }
}
