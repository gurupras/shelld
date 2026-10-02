//go:build shelld_trace

package tracing

import (
	"bufio"
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"runtime"
	rtrace "runtime/trace"
	"sync"
	"sync/atomic"
	"time"
)

// Enabled is true in shelld_trace builds; recording still needs SHELLD_TRACE.
const Enabled = true

// maxBuffered bounds records waiting for the writer; beyond it records are
// dropped and counted.
const maxBuffered = 1 << 20

var (
	on      atomic.Bool
	ids     atomic.Uint64
	dropped atomic.Uint64
	threads = true

	// Records go into a slice the writer swaps out on its own timer, so
	// recording never wakes a goroutine: a channel send would make the writer
	// runnable on every record, changing the very scheduling being measured.
	bufMu sync.Mutex
	buf   = make([]Record, 0, 1<<16)

	namesMu sync.Mutex
	names   = map[string]uint64{}
	pending []nameRec

	closeReq = make(chan chan struct{})
)

type nameRec struct {
	id   uint64
	text string
}

func init() {
	path := os.Getenv("SHELLD_TRACE")
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		slog.Error("tracing: cannot create trace file", "path", path, "err", err)
		return
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := writeHeader(w, Header{Version: 1, Pid: os.Getpid(), Start: time.Now().UnixNano(), Kinds: Kinds[:]}); err != nil {
		slog.Error("tracing: cannot write header", "err", err)
		return
	}
	if rt := os.Getenv("SHELLD_TRACE_RUNTIME"); rt != "" {
		if rf, err := os.Create(rt); err == nil && rtrace.Start(rf) == nil {
			runtimeTrace = rf
		}
	}
	threads = os.Getenv("SHELLD_TRACE_THREADS") != "0"
	on.Store(true)
	go writer(f, w)
	if os.Getenv("SHELLD_TRACE_CANARIES") != "0" {
		go timerCanary()
		go netpollCanary()
		armGCSentinel()
	}
	interval := 100 * time.Millisecond
	if d, err := time.ParseDuration(os.Getenv("SHELLD_TRACE_METRICS")); err == nil && d > 0 {
		interval = d
	}
	go sampleMetrics(interval)
	slog.Info("tracing enabled", "file", path)
}

var runtimeTrace *os.File

// NewID returns a process-unique id for a traced object.
func NewID() uint64 { return ids.Add(1) }

func emit(r Record) {
	r.Time = time.Now().UnixNano()
	if threads {
		r.TID = threadID()
		r.CPU = cpuID()
	}
	bufMu.Lock()
	if len(buf) < maxBuffered {
		buf = append(buf, r)
	} else {
		dropped.Add(1)
	}
	bufMu.Unlock()
}

// Begin opens a span and returns its id for End.
func Begin(k Kind, obj uint64, a int64) uint64 {
	if !on.Load() {
		return 0
	}
	id := ids.Add(1)
	emit(Record{Kind: k, Phase: PhaseBegin, Obj: obj, Span: id, A: a})
	return id
}

// End closes a span opened by Begin.
func End(k Kind, obj, span uint64, a, b int64) {
	if !on.Load() || span == 0 {
		return
	}
	emit(Record{Kind: k, Phase: PhaseEnd, Obj: obj, Span: span, A: a, B: b})
}

// Instant records a point event.
func Instant(k Kind, obj uint64, a, b int64) {
	if !on.Load() {
		return
	}
	emit(Record{Kind: k, Phase: PhaseInstant, Obj: obj, A: a, B: b})
}

// Intern returns a stable id for s, written to the trace once.
func Intern(s string) uint64 {
	if !on.Load() {
		return 0
	}
	namesMu.Lock()
	defer namesMu.Unlock()
	if id, ok := names[s]; ok {
		return id
	}
	id := ids.Add(1)
	names[s] = id
	pending = append(pending, nameRec{id, s})
	return id
}

// Close flushes buffered records and stops recording.
func Close() {
	if !on.Swap(false) {
		return
	}
	ack := make(chan struct{})
	closeReq <- ack
	<-ack
	if runtimeTrace != nil {
		rtrace.Stop()
		runtimeTrace.Close()
	}
}

func writer(f *os.File, w *bufio.Writer) {
	enc := make([]byte, RecordSize)
	spare := make([]Record, 0, 1<<16)
	flush := time.NewTicker(50 * time.Millisecond)
	defer flush.Stop()
	drain := func() {
		bufMu.Lock()
		batch := buf
		buf = spare[:0]
		bufMu.Unlock()
		for i := range batch {
			encode(enc, &batch[i])
			w.Write(enc)
		}
		spare = batch
	}
	writeNames := func() {
		namesMu.Lock()
		p := pending
		pending = nil
		namesMu.Unlock()
		for _, n := range p {
			encode(enc, &Record{Kind: KindName, Obj: n.id, Span: uint64(len(n.text))})
			w.Write(enc)
			pad := make([]byte, (len(n.text)+7)&^7)
			copy(pad, n.text)
			w.Write(pad)
		}
	}
	for {
		select {
		case <-flush.C:
			drain()
			writeNames()
			if n := dropped.Swap(0); n > 0 {
				encode(enc, &Record{Time: time.Now().UnixNano(), Kind: KindDropped, A: int64(n)})
				w.Write(enc)
			}
			w.Flush()
		case ack := <-closeReq:
			drain()
			writeNames()
			w.Flush()
			f.Sync()
			f.Close()
			close(ack)
			return
		}
	}
}

// timerCanary sleeps 1 ms at a time and records how late each wake-up was:
// timer and scheduler stalls show up directly.
func timerCanary() {
	for on.Load() {
		start := time.Now()
		time.Sleep(time.Millisecond)
		Instant(KindTimerLate, 0, int64(time.Since(start)-time.Millisecond), 0)
	}
}

// netpollCanary sends a timestamp through a socketpair every 2 ms and records
// how long the reading goroutine, parked in the netpoller like every
// connection and PTY reader, takes to receive it.
func netpollCanary() {
	a, b, err := socketpair()
	if err != nil {
		slog.Warn("tracing: netpoll canary unavailable", "err", err)
		return
	}
	go func() {
		var msg [8]byte
		for on.Load() {
			if _, err := readFull(b, msg[:]); err != nil {
				return
			}
			sent := int64(binary.LittleEndian.Uint64(msg[:]))
			Instant(KindNetpollPing, 0, time.Now().UnixNano()-sent, 0)
		}
	}()
	var msg [8]byte
	for on.Load() {
		binary.LittleEndian.PutUint64(msg[:], uint64(time.Now().UnixNano()))
		if _, err := a.Write(msg[:]); err != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

type gcSentinel struct{ _ [16]byte }

var gcCycles atomic.Int64

// armGCSentinel records one KindGC per collection: a finalizer on an
// unreachable object runs after each cycle and re-arms itself.
func armGCSentinel() {
	s := &gcSentinel{}
	runtime.SetFinalizer(s, func(*gcSentinel) {
		if !on.Load() {
			return
		}
		Instant(KindGC, 0, gcCycles.Add(1), 0)
		armGCSentinel()
	})
}
