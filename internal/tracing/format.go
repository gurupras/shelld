// Package tracing records what shelld spends its time on: spans and instants
// on the hot paths (PTY reads and writes, VT parsing and locking, the attach
// tick loop, WebSocket reads and writes, HTTP requests), plus runtime
// canaries that measure the Go runtime itself (timer lateness, netpoller wake
// latency, scheduler latency, GC).
//
// Tracing costs nothing unless the binary is built with the shelld_trace tag:
// without it Enabled is the constant false, every call site is written as
//
//	if tracing.Enabled {
//		tracing.Instant(tracing.KindNotify, w.TraceID(), int64(gen), 0)
//	}
//
// and the compiler deletes the whole block. A tagged build records only when
// SHELLD_TRACE names an output file:
//
//	make trace   # CGO_ENABLED=0 go build -tags netgo,shelld_trace -o bin/shelld-traced ./cmd/shelld
//	SHELLD_TRACE=/tmp/shelld.trace bin/shelld-traced run ...
//
// Other variables (tagged builds only): SHELLD_TRACE_RUNTIME=file also
// captures a Go execution trace (go tool trace); SHELLD_TRACE_CANARIES=0
// turns the canaries off; SHELLD_TRACE_THREADS=0 skips recording the OS thread
// and CPU (two system calls per record); SHELLD_TRACE_METRICS=duration sets
// the runtime metrics sampling interval (default 100ms).
//
// Recording is designed not to disturb scheduling: records are appended to a
// buffer under a mutex and a writer goroutine collects them every 50 ms, so
// no record wakes a goroutine.
//
// cmd/shelld-trace reads the output: summaries, per-hop latency of input
// echo, timelines around stalls, and a Chrome/Perfetto JSON export.
package tracing

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Kind identifies an event. Spans are recorded as a Begin and an End record
// sharing a span id; instants as one record.
type Kind uint16

const (
	KindNone Kind = iota

	// Names and bookkeeping. obj is the interned id; the name follows in a
	// name record.
	KindName
	// KindDropped: a = records dropped since the last report (buffer full).
	KindDropped

	// Windows. obj = window id throughout.
	KindWindowCreate // span; a = pid (End)
	KindWindowKill   // span
	KindWindowName   // instant; a = interned name id
	KindPTYRead      // span from before read(2) to its return; End a = bytes
	KindPTYWrite     // span including the PTY lock wait; a = bytes
	KindVTLockWait   // span waiting for the VT lock; a = LockSite
	KindVTAdvance    // span parsing a chunk with the lock held; a = bytes, End b = generation
	KindScrollback   // span appending a chunk to the raw scrollback; a = bytes
	KindNotify       // instant: output made the window dirty; a = generation
	KindProcessExit  // instant; a = exit code, b = 1 when killed by a signal

	// Attach connections. obj = connection id throughout.
	KindAttachOpen  // instant; a = window id, b = cols<<16 | rows
	KindAttachClose // instant
	KindWSRead      // span from before ReadMessage to its return; End a = bytes, b = opcode
	KindWSWrite     // span; a = bytes, b = opcode
	KindTickWake    // instant: the tick loop woke; a = WakeReason, b = nanoseconds waited
	KindRender      // span; End a = bytes, b = generation<<4 | RenderMode
	KindInput       // span writing client input to the PTY; a = bytes
	KindCtrl        // instant: control message; a = 1 resize, 2 scroll
	KindCursorShow  // instant: cursor restored after output settled

	// HTTP. obj = request id.
	KindHTTP // span; Begin a = interned "METHOD /path" id, End a = status

	// Runtime canaries and metrics. obj = 0 unless noted.
	KindTimerLate   // instant: a 1 ms sleep overslept; a = nanoseconds late
	KindNetpollPing // instant: socketpair ping read back through the netpoller; a = latency ns
	KindGC          // instant: a GC cycle finished (finalizer sentinel); a = cycle number
	KindMetric      // instant: obj = interned metric name id, a = value (see metric docs)

	kindCount
)

// WakeReason is why the tick loop woke.
type WakeReason int64

const (
	WakeFirst WakeReason = iota
	WakeDirty
	WakeCtrl
	WakeTimer
	WakeStop
)

// RenderMode is what a tick rendered.
type RenderMode int64

const (
	RenderSkipped RenderMode = iota // generation and view unchanged: nothing to do
	RenderDiff
	RenderFull
	RenderPaused
)

// LockSite says who waited for the VT lock.
type LockSite int64

const (
	LockRead   LockSite = iota // the PTY read loop, to parse output
	LockRender                 // a tick loop, to render
	LockOther                  // capture, resize, colors, attach
)

// Phase distinguishes records of a span.
type Phase uint8

const (
	PhaseInstant Phase = iota
	PhaseBegin
	PhaseEnd
)

// KindInfo describes a kind for readers.
type KindInfo struct {
	Name string `json:"name"`
	Span bool   `json:"span"`
	Obj  string `json:"obj"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// Kinds documents every kind; it is written into each trace file's header.
var Kinds = [kindCount]KindInfo{
	KindNone:         {Name: "none"},
	KindName:         {Name: "name", Obj: "id"},
	KindDropped:      {Name: "dropped", A: "records"},
	KindWindowCreate: {Name: "window.create", Span: true, Obj: "window", A: "pid"},
	KindWindowKill:   {Name: "window.kill", Span: true, Obj: "window"},
	KindWindowName:   {Name: "window.name", Obj: "window", A: "name id"},
	KindPTYRead:      {Name: "pty.read", Span: true, Obj: "window", A: "bytes"},
	KindPTYWrite:     {Name: "pty.write", Span: true, Obj: "window", A: "bytes"},
	KindVTLockWait:   {Name: "vt.lock_wait", Span: true, Obj: "window", A: "site"},
	KindVTAdvance:    {Name: "vt.advance", Span: true, Obj: "window", A: "bytes", B: "generation"},
	KindScrollback:   {Name: "scrollback.write", Span: true, Obj: "window", A: "bytes"},
	KindNotify:       {Name: "window.notify", Obj: "window", A: "generation"},
	KindProcessExit:  {Name: "process.exit", Obj: "window", A: "code", B: "signaled"},
	KindAttachOpen:   {Name: "attach.open", Obj: "conn", A: "window", B: "cols<<16|rows"},
	KindAttachClose:  {Name: "attach.close", Obj: "conn"},
	KindWSRead:       {Name: "ws.read", Span: true, Obj: "conn", A: "bytes", B: "opcode"},
	KindWSWrite:      {Name: "ws.write", Span: true, Obj: "conn", A: "bytes", B: "opcode"},
	KindTickWake:     {Name: "tick.wake", Obj: "conn", A: "reason", B: "waited ns"},
	KindRender:       {Name: "tick.render", Span: true, Obj: "conn", A: "bytes", B: "generation<<4|mode"},
	KindInput:        {Name: "attach.input", Span: true, Obj: "conn", A: "bytes"},
	KindCtrl:         {Name: "attach.ctrl", Obj: "conn", A: "1 resize, 2 scroll"},
	KindCursorShow:   {Name: "tick.cursor_show", Obj: "conn"},
	KindHTTP:         {Name: "http", Span: true, Obj: "request", A: "route name id / status"},
	KindTimerLate:    {Name: "canary.timer_late", A: "ns late"},
	KindNetpollPing:  {Name: "canary.netpoll", A: "ns"},
	KindGC:           {Name: "runtime.gc", A: "cycle"},
	KindMetric:       {Name: "runtime.metric", Obj: "metric name id", A: "value"},
}

func (k Kind) String() string {
	if int(k) < len(Kinds) && Kinds[k].Name != "" {
		return Kinds[k].Name
	}
	return fmt.Sprintf("kind(%d)", k)
}

// Record is one trace record. On disk it is RecordSize bytes, little endian,
// in field order.
type Record struct {
	Time  int64 // wall clock, Unix nanoseconds
	Kind  Kind
	Phase Phase
	_     uint8
	CPU   uint16 // CPU the recording thread ran on
	_     uint16
	TID   uint32 // OS thread id of the recording goroutine at the time
	_     uint32
	Obj   uint64 // window, connection or request id
	Span  uint64 // pairs Begin and End; 0 for instants
	A, B  int64
}

// RecordSize is the encoded size of a Record.
const RecordSize = 56

const magic = "SHELLDTR1\n"

// Header precedes the records in a trace file.
type Header struct {
	Version int        `json:"version"`
	Pid     int        `json:"pid"`
	Start   int64      `json:"start"` // Unix ns
	Kinds   []KindInfo `json:"kinds"`
}

// A name record carries its text in Span (length) followed by that many
// bytes padded to 8.

func writeHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, magic); err != nil {
		return err
	}
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func encode(dst []byte, r *Record) {
	binary.LittleEndian.PutUint64(dst[0:], uint64(r.Time))
	binary.LittleEndian.PutUint16(dst[8:], uint16(r.Kind))
	dst[10] = byte(r.Phase)
	dst[11] = 0
	binary.LittleEndian.PutUint16(dst[12:], r.CPU)
	binary.LittleEndian.PutUint16(dst[14:], 0)
	binary.LittleEndian.PutUint32(dst[16:], r.TID)
	binary.LittleEndian.PutUint32(dst[20:], 0)
	binary.LittleEndian.PutUint64(dst[24:], r.Obj)
	binary.LittleEndian.PutUint64(dst[32:], r.Span)
	binary.LittleEndian.PutUint64(dst[40:], uint64(r.A))
	binary.LittleEndian.PutUint64(dst[48:], uint64(r.B))
}

// Trace is a decoded trace file.
type Trace struct {
	Header  Header
	Records []Record
	Names   map[uint64]string
}

// Read decodes a trace file.
func Read(r io.Reader) (*Trace, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	m := make([]byte, len(magic))
	if _, err := io.ReadFull(br, m); err != nil || string(m) != magic {
		return nil, errors.New("tracing: not a shelld trace")
	}
	var n [4]byte
	if _, err := io.ReadFull(br, n[:]); err != nil {
		return nil, err
	}
	hb := make([]byte, binary.LittleEndian.Uint32(n[:]))
	if _, err := io.ReadFull(br, hb); err != nil {
		return nil, err
	}
	t := &Trace{Names: map[uint64]string{}}
	if err := json.Unmarshal(hb, &t.Header); err != nil {
		return nil, err
	}
	buf := make([]byte, RecordSize)
	for {
		if _, err := io.ReadFull(br, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return t, nil // a crashed writer can leave a partial tail
			}
			return nil, err
		}
		rec := Record{
			Time:  int64(binary.LittleEndian.Uint64(buf[0:])),
			Kind:  Kind(binary.LittleEndian.Uint16(buf[8:])),
			Phase: Phase(buf[10]),
			CPU:   binary.LittleEndian.Uint16(buf[12:]),
			TID:   binary.LittleEndian.Uint32(buf[16:]),
			Obj:   binary.LittleEndian.Uint64(buf[24:]),
			Span:  binary.LittleEndian.Uint64(buf[32:]),
			A:     int64(binary.LittleEndian.Uint64(buf[40:])),
			B:     int64(binary.LittleEndian.Uint64(buf[48:])),
		}
		if rec.Kind == KindName {
			text := make([]byte, (rec.Span+7)&^7)
			if _, err := io.ReadFull(br, text); err != nil {
				return t, nil
			}
			t.Names[rec.Obj] = string(text[:rec.Span])
			continue
		}
		t.Records = append(t.Records, rec)
	}
}
