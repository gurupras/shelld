package vt

import (
	"bytes"
	"encoding/binary"
	"time"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/utf8x"
)

// Parser limits, matching vte 0.15.
const (
	maxIntermediates = 2
	maxParams        = 32
	maxOSCParams     = 16
)

// maxOSCRaw bounds a single OSC payload. An OSC that is never terminated would
// otherwise turn the rest of the PTY stream into resident heap inside the
// daemon, which is one process for every window; the synchronized-update buffer
// above bounds its own growth the same way. Past the cap the OSC is abandoned
// and the parser returns to ground, so the stream recovers instead of feeding a
// buffer that nothing will ever dispatch. Well above any real title or OSC 52
// clipboard write.
//
// Deliberately not a reference limit, which is why it sits outside the const
// block above: vte's MAX_OSC_RAW (1024) bounds only its no_std build, and
// alacritty_terminal asks for std, where osc_raw is an unbounded Vec. The cap
// and the abandon are both ours. See the package doc.
const maxOSCRaw = 1 << 20

// Synchronized update (DEC mode 2026) buffering limits, matching vte.
const (
	syncBufferSize = 0x20_0000
	syncEscapeLen  = 8
)

// SyncUpdateTimeout is how long a synchronized update may hold back the
// grid, counted from its BSU; every BSU inside the update moves the deadline,
// as in vte. 150 ms is alacritty's bound and shelld-rs's (G03-01). The
// Processor only records the deadline: ending an expired update is the
// caller's job (StopSync), since a program that stops writing inside an
// update sends nothing that would end it, and until then the update holds
// back whatever it buffered, replies to terminal queries included.
const SyncUpdateTimeout = 150 * time.Millisecond

// bsuWord and esuWord are "\x1b[?2026h" and "\x1b[?2026l" read as
// little-endian uint64s.
var (
	bsuWord = binary.LittleEndian.Uint64([]byte("\x1b[?2026h"))
	esuWord = binary.LittleEndian.Uint64([]byte("\x1b[?2026l"))
)

type parserState uint8

const (
	stateGround parserState = iota
	stateCsiEntry
	stateCsiIgnore
	stateCsiIntermediate
	stateCsiParam
	stateDcsEntry
	stateDcsIgnore
	stateDcsIntermediate
	stateDcsParam
	stateDcsPassthrough
	stateEscape
	stateEscapeIntermediate
	stateOscString
	stateSosPmApcString
)

// params is a fixed-size parameter list with colon subparameters.
type params struct {
	subparams        [maxParams]uint8 // at a param's first slot: its slot count
	values           [maxParams]uint16
	currentSubparams uint8
	len              int
}

func (p *params) isFull() bool { return p.len == maxParams }
func (p *params) clear()       { p.currentSubparams = 0; p.len = 0 }

func (p *params) push(v uint16) {
	p.subparams[p.len-int(p.currentSubparams)] = p.currentSubparams + 1
	p.values[p.len] = v
	p.currentSubparams = 0
	p.len++
}

func (p *params) extend(v uint16) {
	p.subparams[p.len-int(p.currentSubparams)] = p.currentSubparams + 1
	p.values[p.len] = v
	p.currentSubparams++
	p.len++
}

// paramsIter walks parameters; each step yields one parameter with its
// subparameters.
type paramsIter struct {
	p     *params
	index int
}

func (it *paramsIter) next() ([]uint16, bool) {
	if it.index >= it.p.len {
		return nil, false
	}
	n := int(it.p.subparams[it.index])
	v := it.p.values[it.index : it.index+n]
	it.index += n
	return v, true
}

// nextOr is vte's next_param_or: the next parameter's first value, or def
// when absent or zero.
func (it *paramsIter) nextOr(def uint16) uint16 {
	v, ok := it.next()
	if ok && v[0] != 0 {
		return v[0]
	}
	return def
}

// Processor turns a PTY byte stream into Term operations. It is not safe for
// concurrent use; callers serialize Advance with any reads of the Term.
type Processor struct {
	state           parserState
	intermediates   [maxIntermediates]byte
	intermediateIdx int
	params          params
	param           uint16
	oscRaw          []byte
	oscParams       [maxOSCParams][2]int
	oscNumParams    int
	ignoring        bool
	partialUTF8     [4]byte
	partialLen      int

	precedingChar rune
	hasPreceding  bool

	syncPending  bool
	syncBuf      []byte
	syncDeadline time.Time

	// terminated stops advanceUntilTerminated after a BSU so the remaining
	// bytes are buffered.
	terminated bool
}

// NewProcessor returns a parser in the ground state.
func NewProcessor() *Processor { return &Processor{} }

// SyncPending reports whether a synchronized update is buffering output.
func (p *Processor) SyncPending() bool { return p.syncPending }

// SyncDeadline reports when the pending synchronized update expires; ok is
// false when none is pending.
func (p *Processor) SyncDeadline() (deadline time.Time, ok bool) {
	return p.syncDeadline, p.syncPending
}

func (p *Processor) armSync() {
	p.syncPending = true
	p.syncDeadline = time.Now().Add(SyncUpdateTimeout)
}

// Advance feeds PTY output into t.
func (p *Processor) Advance(t *Term, data []byte) {
	processed := 0
	for processed != len(data) {
		if p.syncPending {
			processed += p.advanceSync(t, data[processed:])
		} else {
			p.terminated = false
			processed += p.advanceUntilTerminated(t, data[processed:])
		}
	}
}

// StopSync ends a pending synchronized update, applying everything buffered.
func (p *Processor) StopSync(t *Term) {
	if p.syncPending {
		p.stopSyncInternal(t, -1)
	}
}

func (p *Processor) advanceSync(t *Term, data []byte) int {
	if len(p.syncBuf)+len(data) >= syncBufferSize-1 {
		p.stopSyncInternal(t, -1)
		p.terminated = false
		return p.advanceUntilTerminated(t, data)
	}
	p.syncBuf = append(p.syncBuf, data...)
	p.advanceSyncCSI(t, len(data))
	return len(data)
}

// advanceSyncCSI looks for BSU/ESU escapes among newly buffered bytes,
// newest first.
func (p *Processor) advanceSyncCSI(t *Term, newBytes int) {
	bufLen := len(p.syncBuf)
	start := max(bufLen-newBytes-(syncEscapeLen-1), 0)
	end := max(bufLen-(syncEscapeLen-1), 0)
	bsuOffset := -1
	search := p.syncBuf[start:end]
	for {
		idx := bytes.LastIndexByte(search, 0x1b)
		if idx < 0 {
			return
		}
		offset := start + idx
		// Compare the 8-byte escape as one word: most ESCs in a synchronized
		// update start other sequences (SGR, cursor moves).
		esc := binary.LittleEndian.Uint64(p.syncBuf[offset : offset+syncEscapeLen])
		if esc == bsuWord {
			p.armSync()
			bsuOffset = offset
		} else if esc == esuWord {
			p.stopSyncInternal(t, bsuOffset)
			return
		}
		search = search[:idx]
	}
}

// stopSyncInternal applies buffered bytes up to bsuOffset (all of them when
// bsuOffset < 0). A newer BSU keeps the update pending with the bytes after
// it still buffered.
func (p *Processor) stopSyncInternal(t *Term, bsuOffset int) {
	buf := p.syncBuf
	offset := len(buf)
	if bsuOffset >= 0 {
		offset = bsuOffset
	}
	p.advance(t, buf[:offset])
	if bsuOffset >= 0 {
		n := copy(buf, buf[bsuOffset:])
		p.syncBuf = buf[:n]
		return
	}
	p.syncPending = false
	p.syncBuf = buf[:0]
}

// advance runs the state machine over data without stopping at BSU.
func (p *Processor) advance(t *Term, data []byte) {
	i := 0
	if p.partialLen != 0 {
		i += p.advancePartialUTF8(t, data)
	}
	for i != len(data) {
		switch p.state {
		case stateGround:
			i += p.advanceGround(t, data[i:])
		case stateCsiEntry, stateCsiParam:
			i += p.advanceCSIRun(t, data[i:])
		default:
			p.changeState(t, data[i])
			i++
		}
	}
}

func (p *Processor) advanceUntilTerminated(t *Term, data []byte) int {
	i := 0
	if p.partialLen != 0 {
		i += p.advancePartialUTF8(t, data)
	}
	for i != len(data) && !p.terminated {
		switch p.state {
		case stateGround:
			i += p.advanceGround(t, data[i:])
		case stateCsiEntry, stateCsiParam:
			i += p.advanceCSIRun(t, data[i:])
		default:
			p.changeState(t, data[i])
			i++
		}
	}
	return i
}

// advanceCSIRun handles a CSI sequence's parameter digits and separators and
// its final byte in one loop — the bytes of nearly every CSI sequence (SGR
// colors above all) — instead of dispatching each byte through changeState.
// For those bytes the CsiEntry and CsiParam transitions are identical; any
// other byte goes through changeState. It stops after a dispatch so the
// caller sees termination, and consumes at least one byte.
func (p *Processor) advanceCSIRun(t *Term, data []byte) int {
	// The parameter being accumulated lives in a local until the run ends, so
	// the loop does not reload it from memory after every digit.
	param := p.param
	for i, b := range data {
		switch {
		case b >= '0' && b <= '9':
			if p.params.isFull() {
				p.ignoring = true
			} else if v := uint32(param)*10 + uint32(b-'0'); v > 0xffff {
				param = 0xffff
			} else {
				param = uint16(v)
			}
		case b == ';':
			if p.pushParam(param) {
				param = 0
			}
		case b == ':':
			if p.extendParam(param) {
				param = 0
			}
		case b >= 0x40 && b <= 0x7e:
			p.param = param
			p.csiDispatchAction(t, b)
			return i + 1
		default:
			p.param = param
			if i == 0 {
				p.changeState(t, b)
				return 1
			}
			p.state = stateCsiParam
			return i
		}
	}
	p.param = param
	if len(data) > 0 {
		p.state = stateCsiParam
	}
	return len(data)
}

func (p *Processor) changeState(t *Term, b byte) {
	switch p.state {
	case stateCsiEntry:
		p.advanceCsiEntry(t, b)
	case stateCsiIgnore:
		p.advanceCsiIgnore(t, b)
	case stateCsiIntermediate:
		p.advanceCsiIntermediate(t, b)
	case stateCsiParam:
		p.advanceCsiParam(t, b)
	case stateDcsEntry:
		p.advanceDcsEntry(b)
	case stateDcsIgnore:
		p.advanceIgnoredString(t, b)
	case stateDcsIntermediate:
		p.advanceDcsIntermediate(b)
	case stateDcsParam:
		p.advanceDcsParam(b)
	case stateDcsPassthrough:
		p.advanceDcsPassthrough(t, b)
	case stateEscape:
		p.advanceEsc(t, b)
	case stateEscapeIntermediate:
		p.advanceEscIntermediate(t, b)
	case stateOscString:
		p.advanceOscString(t, b)
	case stateSosPmApcString:
		p.advanceIgnoredString(t, b)
	}
}

// isExecutable is 0x00..=0x17 | 0x19 | 0x1C..=0x1F.
func isExecutable(b byte) bool {
	return b <= 0x17 || b == 0x19 || (b >= 0x1c && b <= 0x1f)
}

func (p *Processor) advanceCsiEntry(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
		p.state = stateCsiIntermediate
	case b >= 0x30 && b <= 0x39:
		p.paramNext(b)
		p.state = stateCsiParam
	case b == 0x3a:
		p.subparam()
		p.state = stateCsiParam
	case b == 0x3b:
		p.paramSep()
		p.state = stateCsiParam
	case b >= 0x3c && b <= 0x3f:
		p.collect(b)
		p.state = stateCsiParam
	case b >= 0x40 && b <= 0x7e:
		p.csiDispatchAction(t, b)
	default:
		p.anywhere(t, b)
	}
}

func (p *Processor) advanceCsiIgnore(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x3f:
	case b >= 0x40 && b <= 0x7e:
		p.state = stateGround
	case b == 0x7f:
	default:
		p.anywhere(t, b)
	}
}

func (p *Processor) advanceCsiIntermediate(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
	case b >= 0x30 && b <= 0x3f:
		p.state = stateCsiIgnore
	case b >= 0x40 && b <= 0x7e:
		p.csiDispatchAction(t, b)
	default:
		p.anywhere(t, b)
	}
}

func (p *Processor) advanceCsiParam(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
		p.state = stateCsiIntermediate
	case b >= 0x30 && b <= 0x39:
		p.paramNext(b)
	case b == 0x3a:
		p.subparam()
	case b == 0x3b:
		p.paramSep()
	case b >= 0x3c && b <= 0x3f:
		p.state = stateCsiIgnore
	case b >= 0x40 && b <= 0x7e:
		p.csiDispatchAction(t, b)
	case b == 0x7f:
	default:
		p.anywhere(t, b)
	}
}

func (p *Processor) advanceDcsEntry(b byte) {
	switch {
	case isExecutable(b):
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
		p.state = stateDcsIntermediate
	case b >= 0x30 && b <= 0x39:
		p.paramNext(b)
		p.state = stateDcsParam
	case b == 0x3a:
		p.subparam()
		p.state = stateDcsParam
	case b == 0x3b:
		p.paramSep()
		p.state = stateDcsParam
	case b >= 0x3c && b <= 0x3f:
		p.collect(b)
		p.state = stateDcsParam
	case b >= 0x40 && b <= 0x7e:
		p.hook()
	case b == 0x7f:
	default:
		p.anywhereNoTerm(b)
	}
}

func (p *Processor) advanceDcsIntermediate(b byte) {
	switch {
	case isExecutable(b):
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
	case b >= 0x30 && b <= 0x3f:
		p.state = stateDcsIgnore
	case b >= 0x40 && b <= 0x7e:
		p.hook()
	case b == 0x7f:
	default:
		p.anywhereNoTerm(b)
	}
}

func (p *Processor) advanceDcsParam(b byte) {
	switch {
	case isExecutable(b):
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
		p.state = stateDcsIntermediate
	case b >= 0x30 && b <= 0x39:
		p.paramNext(b)
	case b == 0x3a:
		p.subparam()
	case b == 0x3b:
		p.paramSep()
	case b >= 0x3c && b <= 0x3f:
		p.state = stateDcsIgnore
	case b >= 0x40 && b <= 0x7e:
		p.hook()
	case b == 0x7f:
	default:
		p.anywhereNoTerm(b)
	}
}

func (p *Processor) advanceDcsPassthrough(t *Term, b byte) {
	switch {
	case isExecutable(b) || (b >= 0x1c && b <= 0x7e):
		// put: DCS payloads are ignored.
	case b == 0x18 || b == 0x1a:
		p.execute(t, b)
		p.state = stateGround
	case b == 0x1b:
		p.resetParams()
		p.state = stateEscape
	case b == 0x9c:
		p.state = stateGround
	}
}

// anywhereNoTerm is anywhere for DCS states, which have no Term at hand for
// CAN/SUB; those bytes are C0 controls with no handler besides SUB (a no-op).
func (p *Processor) anywhereNoTerm(b byte) {
	switch b {
	case 0x18, 0x1a:
		p.state = stateGround
	case 0x1b:
		p.resetParams()
		p.state = stateEscape
	}
}

func (p *Processor) advanceEsc(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
		p.state = stateEscapeIntermediate
	case b >= 0x30 && b <= 0x4f:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x50:
		p.resetParams()
		p.state = stateDcsEntry
	case b >= 0x51 && b <= 0x57:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x58:
		p.state = stateSosPmApcString
	case b == 0x59 || b == 0x5a:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x5b:
		p.resetParams()
		p.state = stateCsiEntry
	case b == 0x5c:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x5d:
		p.oscRaw = p.oscRaw[:0]
		p.oscNumParams = 0
		p.state = stateOscString
	case b == 0x5e || b == 0x5f:
		p.state = stateSosPmApcString
	case b >= 0x60 && b <= 0x7e:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x18 || b == 0x1a:
		p.execute(t, b)
		p.state = stateGround
	}
}

func (p *Processor) advanceEscIntermediate(t *Term, b byte) {
	switch {
	case isExecutable(b):
		p.execute(t, b)
	case b >= 0x20 && b <= 0x2f:
		p.collect(b)
	case b >= 0x30 && b <= 0x7e:
		p.escDispatch(t, b)
		p.state = stateGround
	case b == 0x7f:
	default:
		p.anywhere(t, b)
	}
}

func (p *Processor) advanceOscString(t *Term, b byte) {
	switch {
	case b <= 0x06 || (b >= 0x08 && b <= 0x17) || b == 0x19 || (b >= 0x1c && b <= 0x1f):
	case b == 0x07:
		p.oscEnd(t, b)
		p.state = stateGround
	case b == 0x18 || b == 0x1a:
		p.oscEnd(t, b)
		p.execute(t, b)
		p.state = stateGround
	case b == 0x1b:
		p.oscEnd(t, b)
		p.resetParams()
		p.state = stateEscape
	case b == 0x3b:
		p.oscPutParam()
	default:
		if len(p.oscRaw) >= maxOSCRaw {
			p.abandonOSC()
			return
		}
		p.oscRaw = append(p.oscRaw, b)
	}
}

// abandonOSC drops an oversized OSC payload and resynchronizes at ground. The
// backing array is released rather than truncated: oscEnd keeps its slice for
// reuse, which is the right trade for ordinary payloads but would pin the peak
// for the Processor's life here.
func (p *Processor) abandonOSC() {
	p.oscRaw = nil
	p.oscNumParams = 0
	p.state = stateGround
}

func (p *Processor) anywhere(t *Term, b byte) {
	switch b {
	case 0x18, 0x1a:
		p.execute(t, b)
		p.state = stateGround
	case 0x1b:
		p.resetParams()
		p.state = stateEscape
	}
}

// advanceIgnoredString consumes a string whose payload is discarded: SOS, PM,
// APC, and a DCS the parser gave up on. Only a terminator matters, and 8-bit ST
// is one — without it the string never ends and every later byte, meaning all
// program output, is silently eaten until some later ESC resynchronizes the
// parser. advanceDcsPassthrough already terminates on this byte; anywhere does
// not, and must not, because the CSI states share it.
//
// This is a divergence from the reference rather than a restoration of it, and the
// package doc lists it as the fifth. vte does route exactly these two states, and
// only these two, to its own anywhere (lib.rs:175 and :182) — but that function
// does not carry a 0x9C arm: its arms are 0x18 | 0x1A, 0x1B and _ => ()
// (lib.rs:438-450), and advance dispatches a non-ground byte straight to
// change_state without any C1 handling of its own (lib.rs:117-127). So in vte
// 0.15.0 a bare 8-bit ST never ends a SOS, PM, APC or abandoned DCS. The one 0x9C
// in that crate's source is advance_dcs_passthrough's (lib.rs:331), which is the
// state this is not: passthrough is the DCS the parser kept, this is the DCS it
// gave up on.
//
// The helper of the same name here is a narrower one that the CSI states also use,
// so the arm goes on a new function rather than on anywhere.
func (p *Processor) advanceIgnoredString(t *Term, b byte) {
	if b == 0x9c {
		p.state = stateGround
		return
	}
	p.anywhere(t, b)
}

func (p *Processor) csiDispatchAction(t *Term, b byte) {
	p.pushParam(p.param)
	p.csiDispatch(t, b)
	p.state = stateGround
}

func (p *Processor) hook() {
	p.pushParam(p.param)
	p.state = stateDcsPassthrough
}

func (p *Processor) collect(b byte) {
	if p.intermediateIdx == maxIntermediates {
		p.ignoring = true
	} else {
		p.intermediates[p.intermediateIdx] = b
		p.intermediateIdx++
	}
}

func (p *Processor) subparam() {
	if p.extendParam(p.param) {
		p.param = 0
	}
}

func (p *Processor) paramSep() {
	if p.pushParam(p.param) {
		p.param = 0
	}
}

// pushParam ends a parameter with value v; when the list is full it marks the
// sequence ignored and reports false.
func (p *Processor) pushParam(v uint16) bool {
	if p.params.isFull() {
		p.ignoring = true
		return false
	}
	p.params.push(v)
	return true
}

// extendParam ends a subparameter with value v, like pushParam.
func (p *Processor) extendParam(v uint16) bool {
	if p.params.isFull() {
		p.ignoring = true
		return false
	}
	p.params.extend(v)
	return true
}

func (p *Processor) paramNext(b byte) {
	if p.params.isFull() {
		p.ignoring = true
		return
	}
	v := uint32(p.param) * 10
	if v > 0xffff {
		v = 0xffff
	}
	v += uint32(b - '0')
	if v > 0xffff {
		v = 0xffff
	}
	p.param = uint16(v)
}

func (p *Processor) oscPutParam() {
	idx := len(p.oscRaw)
	switch p.oscNumParams {
	case 0:
		p.oscParams[0] = [2]int{0, idx}
	case maxOSCParams:
		return
	default:
		p.oscParams[p.oscNumParams] = [2]int{p.oscParams[p.oscNumParams-1][1], idx}
	}
	p.oscNumParams++
}

func (p *Processor) oscEnd(t *Term, b byte) {
	p.oscPutParam()
	var list [maxOSCParams][]byte
	for i := 0; i < p.oscNumParams; i++ {
		r := p.oscParams[i]
		list[i] = p.oscRaw[r[0]:r[1]]
	}
	p.oscDispatch(t, list[:p.oscNumParams], b == 0x07)
	p.oscRaw = p.oscRaw[:0]
	p.oscNumParams = 0
}

func (p *Processor) resetParams() {
	p.intermediateIdx = 0
	p.ignoring = false
	p.param = 0
	p.params.clear()
}

// advanceGround handles text up to the next ESC.
func (p *Processor) advanceGround(t *Term, data []byte) int {
	numBytes := len(data)
	plain := bytes.IndexByte(data, 0x1b)
	if plain < 0 {
		plain = numBytes
	}
	if plain == 0 {
		p.state = stateEscape
		p.resetParams()
		return 1
	}

	valid, errLen, ok := utf8x.Check(data[:plain])
	if ok {
		p.groundDispatch(t, data[:plain])
		processed := plain
		if processed < numBytes {
			p.state = stateEscape
			p.resetParams()
			processed++
		}
		return processed
	}

	p.groundDispatch(t, data[:valid])
	if errLen > 0 {
		// A stray byte prints U+FFFD whatever its value. vte executes a lone
		// 0x80-0x9F here as an 8-bit C1 control, which nothing implements, so the
		// byte vanished; tmux and shelld-rs (substitute_c1) draw U+FFFD, and so
		// does this. A C1 encoded as valid UTF-8 is still executed, in
		// groundDispatch, as vte and shelld-rs do.
		p.print(t, utf8.RuneError)
		return valid + errLen
	}
	if plain < numBytes {
		p.print(t, utf8.RuneError)
		p.state = stateEscape
		p.resetParams()
		return plain + 1
	}
	extra := numBytes - valid
	copy(p.partialUTF8[p.partialLen:], data[valid:numBytes])
	p.partialLen += extra
	return numBytes
}

func (p *Processor) advancePartialUTF8(t *Term, data []byte) int {
	old := p.partialLen
	toCopy := min(len(data), len(p.partialUTF8)-old)
	copy(p.partialUTF8[old:], data[:toCopy])
	p.partialLen += toCopy

	buf := p.partialUTF8[:p.partialLen]
	valid, errLen, ok := utf8x.Check(buf)
	if ok {
		r, size := utf8.DecodeRune(buf)
		p.print(t, r)
		p.partialLen = 0
		return size - old
	}
	if valid > 0 {
		// Only the first rune is printed, so only its bytes are consumed:
		// valid can cover further runes that this call has not printed.
		r, size := utf8.DecodeRune(buf)
		p.print(t, r)
		p.partialLen = 0
		return size - old
	}
	if errLen > 0 {
		p.print(t, utf8.RuneError)
		p.partialLen = 0
		return errLen - old
	}
	return toCopy
}

// groundDispatch prints or executes every character of valid UTF-8 text.
func (p *Processor) groundDispatch(t *Term, s []byte) {
	i := 0
	for i < len(s) {
		b := s[i]
		if b >= 0x20 && b < 0x7f {
			j := i + 1
			for j < len(s) && s[j] >= 0x20 && s[j] < 0x7f {
				j++
			}
			g := t.grid
			if t.mode&ModeInsert == 0 && g.Cursor.Charsets[t.activeCharset] == CharsetASCII {
				t.inputASCII(s[i:j])
			} else {
				for _, c := range s[i:j] {
					t.input(rune(c))
				}
			}
			p.precedingChar, p.hasPreceding = rune(s[j-1]), true
			i = j
			continue
		}
		if b < 0x80 {
			if b < 0x20 {
				p.execute(t, b)
			} else {
				p.print(t, rune(b))
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r >= 0x80 && r <= 0x9f {
			p.execute(t, byte(r))
		} else {
			p.print(t, r)
		}
		i += size
	}
}

func (p *Processor) print(t *Term, c rune) {
	t.input(c)
	p.precedingChar, p.hasPreceding = c, true
}

func (p *Processor) execute(t *Term, b byte) {
	switch b {
	case 0x09:
		t.putTab(1)
	case 0x08:
		t.backspace()
	case 0x0d:
		t.carriageReturn()
	case 0x0a, 0x0b, 0x0c:
		t.linefeed()
	case 0x0f:
		t.activeCharset = 0
	case 0x0e:
		t.activeCharset = 1
	}
	// BEL and SUB have no grid effect.
}

func (p *Processor) oscDispatch(t *Term, params [][]byte, bellTerminated bool) {
	if len(params) == 0 || len(params[0]) == 0 {
		return
	}
	terminator := "\x1b\\"
	if bellTerminated {
		terminator = "\x07"
	}
	switch string(params[0]) {
	case "4":
		if len(params) <= 1 || len(params)%2 == 0 {
			return
		}
		for i := 1; i+1 < len(params); i += 2 {
			index, ok := parseOSCNumber(params[i])
			if !ok {
				continue
			}
			if string(params[i+1]) == "?" {
				t.dynamicColorSequence("4;"+itoa(int(index)), int(index), terminator)
			}
		}
	case "10", "11", "12":
		if len(params) < 2 {
			return
		}
		code, ok := parseOSCNumber(params[0])
		if !ok {
			return
		}
		dyn := int(code)
		for _, param := range params[1:] {
			index := int(Foreground) + dyn - 10
			if index > int(CursorColor) {
				break
			}
			if string(param) == "?" {
				t.dynamicColorSequence(itoa(dyn), index, terminator)
			}
			dyn++
		}
	}
	// Titles, hyperlinks, palette changes, cursor shape and clipboard do not
	// affect the grid.
}

func parseOSCNumber(b []byte) (uint8, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 255 {
			return 0, false
		}
	}
	return uint8(n), true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (p *Processor) csiDispatch(t *Term, action byte) {
	if p.ignoring || p.intermediateIdx > 2 {
		return
	}
	ints := p.intermediates[:p.intermediateIdx]
	it := paramsIter{p: &p.params}
	noInts := len(ints) == 0
	oneInt := func(c byte) bool { return len(ints) == 1 && ints[0] == c }

	// Only 'c' accepts any intermediates. Every other final byte dispatches
	// either with none (the switch on the final byte below, a jump table) or
	// with the specific intermediates handled here.
	if action != 'c' && !noInts {
		switch {
		case action == 'h' && oneInt('?'):
			for v, ok := it.next(); ok; v, ok = it.next() {
				if v[0] == 2026 {
					p.armSync()
					p.terminated = true
				}
				t.setPrivateMode(v[0], true)
			}
		case action == 'l' && oneInt('?'):
			for v, ok := it.next(); ok; v, ok = it.next() {
				t.setPrivateMode(v[0], false)
			}
		case action == 'p' && oneInt('$'):
			t.reportMode(it.nextOr(0))
		case action == 'p' && len(ints) == 2 && ints[0] == '?' && ints[1] == '$':
			t.reportPrivateMode(it.nextOr(0))
		case action == 'q' && oneInt(' '):
			switch id := it.nextOr(0); {
			case id == 0:
				t.cursorBlink = false
			case id <= 6:
				t.cursorBlink = id%2 == 1
			}
		}
		return
	}

	switch action {
	case '@':
		t.insertBlank(int(it.nextOr(1)))
	case 'A':
		t.moveUp(int(it.nextOr(1)))
	case 'B', 'e':
		t.moveDown(int(it.nextOr(1)))
	case 'b':
		if p.hasPreceding {
			n := int(it.nextOr(1))
			for range n {
				t.input(p.precedingChar)
			}
		}
	case 'C', 'a':
		t.moveForward(int(it.nextOr(1)))
	case 'c':
		if it.nextOr(0) == 0 {
			var first byte
			if len(ints) > 0 {
				first = ints[0]
			}
			t.identifyTerminal(first)
		}
	case 'D':
		t.moveBackward(int(it.nextOr(1)))
	case 'd':
		t.gotoLine(int(it.nextOr(1)) - 1)
	case 'E':
		t.moveDownAndCR(int(it.nextOr(1)))
	case 'F':
		t.moveUpAndCR(int(it.nextOr(1)))
	case 'G', '`':
		t.gotoCol(int(it.nextOr(1)) - 1)
	// CSI ? 5 W (set_tabs) is a no-op in alacritty's Term.
	case 'g':
		switch it.nextOr(0) {
		case 0:
			t.clearTabs(false)
		case 3:
			t.clearTabs(true)
		}
	case 'H', 'f':
		y := int(it.nextOr(1))
		x := int(it.nextOr(1))
		t.goTo(y-1, x-1)
	case 'h':
		for v, ok := it.next(); ok; v, ok = it.next() {
			t.setMode(v[0], true)
		}
	case 'I':
		t.moveForwardTabs(int(it.nextOr(1)))
	case 'J':
		switch it.nextOr(0) {
		case 0:
			t.clearScreen(clearBelow)
		case 1:
			t.clearScreen(clearAbove)
		case 2:
			t.clearScreen(clearScreenAll)
		case 3:
			t.clearScreen(clearSaved)
		}
	case 'K':
		switch it.nextOr(0) {
		case 0:
			t.clearLine(clearRight)
		case 1:
			t.clearLine(clearLeft)
		case 2:
			t.clearLine(clearAll)
		}
	case 'L':
		t.insertBlankLines(int(it.nextOr(1)))
	case 'l':
		for v, ok := it.next(); ok; v, ok = it.next() {
			t.setMode(v[0], false)
		}
	case 'M':
		t.deleteLines(int(it.nextOr(1)))
	case 'm':
		if p.params.len == 0 {
			t.resetAttributes()
		} else {
			sgr(t, &it)
		}
	case 'n':
		t.deviceStatus(int(it.nextOr(0)))
	case 'P':
		t.deleteChars(int(it.nextOr(1)))
	case 'r':
		top := int(it.nextOr(1))
		bottom := 0
		if v, ok := it.next(); ok {
			bottom = int(v[0])
		}
		t.setScrollingRegion(top, bottom)
	case 'S':
		t.scrollUp(int(it.nextOr(1)))
	case 's':
		t.saveCursorPosition()
	case 'T':
		t.scrollDown(int(it.nextOr(1)))
	case 't':
		if it.nextOr(1) == 18 {
			t.textAreaSizeChars()
		}
	case 'u':
		t.restoreCursorPosition()
	case 'X':
		t.eraseChars(int(it.nextOr(1)))
	case 'Z':
		t.moveBackwardTabs(int(it.nextOr(1)))
	}
}

func (p *Processor) escDispatch(t *Term, b byte) {
	ints := p.intermediates[:p.intermediateIdx]
	noInts := len(ints) == 0
	charsetIndex := func() (int, bool) {
		if len(ints) != 1 {
			return 0, false
		}
		switch ints[0] {
		case '(':
			return 0, true
		case ')':
			return 1, true
		case '*':
			return 2, true
		case '+':
			return 3, true
		}
		return 0, false
	}
	switch {
	case b == 'B':
		if i, ok := charsetIndex(); ok {
			t.grid.Cursor.Charsets[i] = CharsetASCII
		}
	case b == 'D' && noInts:
		t.linefeed()
	case b == 'E' && noInts:
		t.linefeed()
		t.carriageReturn()
	case b == 'H' && noInts:
		t.setHorizontalTabstop()
	case b == 'M' && noInts:
		t.reverseIndex()
	case b == 'Z' && noInts:
		t.identifyTerminal(0)
	case b == 'c' && noInts:
		t.resetState()
	case b == '0':
		if i, ok := charsetIndex(); ok {
			t.grid.Cursor.Charsets[i] = CharsetLineDrawing
		}
	case b == '7' && noInts:
		t.saveCursorPosition()
	case b == '8' && len(ints) == 1 && ints[0] == '#':
		t.decaln()
	case b == '8' && noInts:
		t.restoreCursorPosition()
	case b == '=' && noInts:
		t.mode |= ModeAppKeypad
	case b == '>' && noInts:
		t.mode &^= ModeAppKeypad
	}
}

func (t *Term) decaln() {
	g := t.grid
	for line := 0; line < g.lines; line++ {
		cells := g.raw.row(line).span(0, g.columns)
		for i := range cells {
			cells[i] = Cell{ch: 'E' ^ ' '}
		}
	}
}

func (t *Term) resetAttributes() {
	tmpl := &t.grid.Cursor.Template
	tmpl.fg, tmpl.bg, tmpl.flags, tmpl.ul = 0, 0, 0, NoColor
}

// sgr applies SGR parameters (vte's attrs_from_sgr_parameters).
func sgr(t *Term, it *paramsIter) {
	tmpl := &t.grid.Cursor.Template
	setUnderline := func(f Flags) {
		tmpl.flags &^= AllUnderlines
		tmpl.flags |= f
	}
	for {
		param, ok := it.next()
		if !ok {
			return
		}
		first := param[0]
		if len(param) > 1 {
			switch first {
			case 4:
				sub := param[1]
				if len(param) > 2 {
					sub = 1 // [4, ..] with extra subparameters is a plain underline
				}
				switch sub {
				case 0:
					tmpl.flags &^= AllUnderlines
				case 2:
					setUnderline(FlagDoubleUnderline)
				case 3:
					setUnderline(FlagUndercurl)
				case 4:
					setUnderline(FlagDottedUnderline)
				case 5:
					setUnderline(FlagDashedUnderline)
				default:
					setUnderline(FlagUnderline)
				}
			case 38:
				if c, ok := colonColor(param[1:]); ok {
					tmpl.SetFg(c)
				}
			case 48:
				if c, ok := colonColor(param[1:]); ok {
					tmpl.SetBg(c)
				}
			case 58:
				if c, ok := colonColor(param[1:]); ok {
					tmpl.ul = c
				}
			}
			continue
		}
		switch {
		case first == 0:
			t.resetAttributes()
		case first == 1:
			tmpl.flags |= FlagBold
		case first == 2:
			tmpl.flags |= FlagDim
		case first == 3:
			tmpl.flags |= FlagItalic
		case first == 4:
			setUnderline(FlagUnderline)
		case first == 7:
			tmpl.flags |= FlagInverse
		case first == 8:
			tmpl.flags |= FlagHidden
		case first == 9:
			tmpl.flags |= FlagStrikeout
		case first == 21:
			tmpl.flags &^= FlagBold
		case first == 22:
			tmpl.flags &^= FlagBold | FlagDim
		case first == 23:
			tmpl.flags &^= FlagItalic
		case first == 24:
			tmpl.flags &^= AllUnderlines
		case first == 27:
			tmpl.flags &^= FlagInverse
		case first == 28:
			tmpl.flags &^= FlagHidden
		case first == 29:
			tmpl.flags &^= FlagStrikeout
		case first >= 30 && first <= 37:
			tmpl.SetFg(NewNamedColor(NamedColor(first - 30)))
		case first == 38:
			if c, ok := it.sgrColor(); ok {
				tmpl.SetFg(c)
			}
		case first == 39:
			tmpl.fg = 0
		case first >= 40 && first <= 47:
			tmpl.SetBg(NewNamedColor(NamedColor(first - 40)))
		case first == 48:
			if c, ok := it.sgrColor(); ok {
				tmpl.SetBg(c)
			}
		case first == 49:
			tmpl.bg = 0
		case first == 58:
			if c, ok := it.sgrColor(); ok {
				tmpl.ul = c
			}
		case first == 59:
			tmpl.ul = NoColor
		case first >= 90 && first <= 97:
			tmpl.SetFg(NewNamedColor(BrightBlack + NamedColor(first-90)))
		case first >= 100 && first <= 107:
			tmpl.SetBg(NewNamedColor(BrightBlack + NamedColor(first-100)))
		}
	}
}

// nextFirst is the next parameter's first value, as
// `params.map(|param| param[0])` yields for semicolon-separated colors.
func (it *paramsIter) nextFirst() (uint16, bool) {
	if it.index >= it.p.len {
		return 0, false
	}
	v := it.p.values[it.index]
	it.index += int(it.p.subparams[it.index])
	return v, true
}

// nextByte is nextFirst limited to 0..255; like vte it consumes the parameter
// even when it is out of range.
func (it *paramsIter) nextByte() (uint8, bool) {
	v, ok := it.nextFirst()
	if !ok || v > 255 {
		return 0, false
	}
	return uint8(v), true
}

// sgrColor is parse_sgr_color over the remaining parameters' first values.
func (it *paramsIter) sgrColor() (Color, bool) {
	kind, ok := it.nextFirst()
	if !ok {
		return 0, false
	}
	switch kind {
	case 2:
		r, ok := it.nextByte()
		if !ok {
			return 0, false
		}
		g, ok := it.nextByte()
		if !ok {
			return 0, false
		}
		b, ok := it.nextByte()
		if !ok {
			return 0, false
		}
		return NewRGBColor(r, g, b), true
	case 5:
		i, ok := it.nextByte()
		if !ok {
			return 0, false
		}
		return NewIndexedColor(i), true
	}
	return 0, false
}

// colonColor is handle_colon_rgb over a parameter's subparameters: the color
// space id, then the values from index 1, or 2 when a colorspace id is
// present.
func colonColor(sub []uint16) (Color, bool) {
	rgbStart := 1
	if len(sub) > 4 {
		rgbStart = 2
	}
	rest := sub[rgbStart:]
	switch sub[0] {
	case 2:
		if len(rest) < 3 || rest[0] > 255 || rest[1] > 255 || rest[2] > 255 {
			return 0, false
		}
		return NewRGBColor(uint8(rest[0]), uint8(rest[1]), uint8(rest[2])), true
	case 5:
		if len(rest) < 1 || rest[0] > 255 {
			return 0, false
		}
		return NewIndexedColor(uint8(rest[0])), true
	}
	return 0, false
}
