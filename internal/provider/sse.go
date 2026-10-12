package provider

import (
	"bytes"
	"errors"
)

// maxSSEEventBytes bounds a single reassembled SSE event, including the raw
// bytes of every field and comment line, so a malformed or hostile upstream
// cannot exhaust plugin memory.
const maxSSEEventBytes = 8 << 20

// maxSSEStreamBytes bounds the total decoded bytes of one upstream stream so a
// hostile upstream cannot stream unbounded data through the plugin.
const maxSSEStreamBytes = 64 << 20

// errEmitStopped reports that the downstream consumer asked to stop; it is not
// an upstream framing failure.
var errEmitStopped = errors.New("sse emit stopped")

// errSSEMalformed reports an oversized, malformed, or truncated event.
var errSSEMalformed = errors.New("malformed sse event")

// sseBOM is the UTF-8 byte order mark the SSE spec strips from the very start
// of an event stream.
var sseBOM = []byte{0xEF, 0xBB, 0xBF}

// sseEvent is one complete Server-Sent Event.
type sseEvent struct {
	// Data is the SSE-spec data payload: consecutive data lines joined with "\n"
	// after stripping a single leading space. It is nil when the event carries no
	// data, e.g. a comment/heartbeat event.
	Data []byte
	// Raw is the exact event bytes, preserving every physical line and its
	// original delimiter, terminated by the blank line that ends the event. It is
	// a private copy so callers may retain it.
	Raw []byte
}

// sseEventFramer reassembles complete Server-Sent Events from arbitrarily split
// upstream reads. It accepts LF, CRLF and lone CR line terminators, strips one
// leading UTF-8 BOM, tolerates comment lines (": ..."), UTF-8 payloads split at
// any byte boundary, and joins multiple consecutive data lines of one event
// with "\n" per the SSE specification.
type sseEventFramer struct {
	buf   []byte
	data  [][]byte
	size  int
	raw   []byte
	any   bool
	total int

	// started is set once the leading-BOM question is resolved: the BOM was
	// matched, clearly absent, or contradicted. While false, bomN counts the BOM
	// bytes matched so far so a BOM split across reads is still consumed.
	started bool
	bomN    int
}

// push feeds raw upstream bytes. It invokes emit once per dispatched event. It
// returns errEmitStopped when emit returns false and errSSEMalformed on an
// oversized or malformed event.
func (f *sseEventFramer) push(chunk []byte, emit func(sseEvent) bool) error {
	if len(chunk) == 0 {
		return nil
	}
	f.total += len(chunk)
	if f.total > maxSSEStreamBytes {
		return errSSEMalformed
	}
	if !f.started {
		chunk = f.stripBOM(chunk)
		if len(chunk) == 0 {
			return nil
		}
	}
	f.buf = append(f.buf, chunk...)
	for {
		index, width := indexLineEnd(f.buf, true)
		if index < 0 {
			break
		}
		physical := f.buf[:index+width]
		f.buf = f.buf[index+width:]
		if err := f.line(physical, width, emit); err != nil {
			return err
		}
	}
	if len(f.buf) > maxSSEEventBytes {
		return errSSEMalformed
	}
	return nil
}

// stripBOM consumes exactly one leading UTF-8 BOM. It tolerates the BOM being
// split at any byte boundary: matched bytes are held until the sequence is
// confirmed or contradicted, and a contradiction replays the held bytes so no
// content is lost.
func (f *sseEventFramer) stripBOM(chunk []byte) []byte {
	for len(chunk) > 0 && f.bomN < len(sseBOM) {
		if chunk[0] != sseBOM[f.bomN] {
			f.started = true
			held := append([]byte(nil), sseBOM[:f.bomN]...)
			f.bomN = 0
			return append(held, chunk...)
		}
		f.bomN++
		chunk = chunk[1:]
	}
	if f.bomN == len(sseBOM) {
		f.started = true
	}
	return chunk
}

// finish resolves the upstream EOF. A trailing CR that stayed ambiguous during
// reads is a valid lone-CR terminator and is flushed through emit (which may
// complete one final event); any other trailing partial line or undispatched
// event means the stream was truncated.
func (f *sseEventFramer) finish(emit func(sseEvent) bool) error {
	for len(f.buf) > 0 {
		index, width := indexLineEnd(f.buf, false)
		if index < 0 {
			break
		}
		physical := f.buf[:index+width]
		f.buf = f.buf[index+width:]
		if err := f.line(physical, width, emit); err != nil {
			return err
		}
	}
	if len(f.buf) > 0 || len(f.raw) > 0 || (!f.started && f.bomN > 0) {
		return errSSEMalformed
	}
	return nil
}

// indexLineEnd returns the offset and byte width of the next line terminator in
// buf, or (-1, 0) when there is none. A line may end with LF, CRLF or lone CR.
// When waitCRLF is true a buffer-final CR is left unprocessed because a LF may
// still arrive (a CRLF split across reads); the EOF path passes false so the CR
// is treated as a terminator.
func indexLineEnd(buf []byte, waitCRLF bool) (int, int) {
	for i := 0; i < len(buf); i++ {
		switch buf[i] {
		case '\n':
			return i, 1
		case '\r':
			if i+1 < len(buf) {
				if buf[i+1] == '\n' {
					return i, 2
				}
				return i, 1
			}
			if waitCRLF {
				return -1, 0
			}
			return i, 1
		}
	}
	return -1, 0
}

// line consumes one physical line including its delimiter, whose width is the
// number of delimiter bytes. The raw bytes are always retained; only "data"
// fields contribute to the joined payload.
func (f *sseEventFramer) line(physical []byte, width int, emit func(sseEvent) bool) error {
	f.raw = append(f.raw, physical...)
	if len(f.raw) > maxSSEEventBytes {
		return errSSEMalformed
	}
	content := physical[:len(physical)-width]
	if len(content) == 0 {
		return f.dispatch(emit)
	}
	f.any = true
	if content[0] == ':' {
		return nil
	}
	colon := bytes.IndexByte(content, ':')
	if colon < 0 || !bytes.Equal(content[:colon], []byte("data")) {
		return nil
	}
	value := content[colon+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	f.data = append(f.data, value)
	f.size += len(value)
	if f.size > maxSSEEventBytes {
		return errSSEMalformed
	}
	return nil
}

func (f *sseEventFramer) dispatch(emit func(sseEvent) bool) error {
	raw := append([]byte(nil), f.raw...)
	var joined []byte
	if len(f.data) > 0 {
		joined = bytes.Join(f.data, []byte("\n"))
	}
	stray := !f.any && len(f.data) == 0
	f.raw = f.raw[:0]
	f.data = f.data[:0]
	f.size = 0
	f.any = false
	if stray {
		// A blank line with no preceding field is not an event; drop it.
		return nil
	}
	if !emit(sseEvent{Data: joined, Raw: raw}) {
		return errEmitStopped
	}
	return nil
}

// isSSEDone reports whether a data payload is the terminal chat sentinel.
func isSSEDone(payload []byte) bool {
	return bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]"))
}
