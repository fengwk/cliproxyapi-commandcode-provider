package provider

import (
	"bytes"
	"testing"
)

// collectEvents feeds raw through a fresh framer in fixed-size chunks and
// returns the dispatched events plus the finish result.
func collectEvents(raw []byte, chunk int) ([]sseEvent, error) {
	framer := &sseEventFramer{}
	var events []sseEvent
	emit := func(event sseEvent) bool {
		events = append(events, sseEvent{Data: bytes.Clone(event.Data), Raw: bytes.Clone(event.Raw)})
		return true
	}
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		if err := framer.push(raw[i:end], emit); err != nil {
			return events, err
		}
	}
	return events, framer.finish(emit)
}

// TestFramerAcceptsLFCRLFAndLoneCR proves all three SSE line terminators yield
// the same two events, per the WHATWG event-stream parsing rules.
func TestFramerAcceptsLFCRLFAndLoneCR(t *testing.T) {
	cases := map[string][]byte{
		"lf":   []byte("data: {\"test\":\"1\"}\n\ndata: [DONE]\n\n"),
		"crlf": []byte("data: {\"test\":\"1\"}\r\n\r\ndata: [DONE]\r\n\r\n"),
		"cr":   []byte("data: {\"test\":\"1\"}\r\rdata: [DONE]\r\r"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			events, err := collectEvents(raw, 3)
			if err != nil {
				t.Fatalf("finish must succeed: %v", err)
			}
			if len(events) != 2 {
				t.Fatalf("expected 2 events, got %d (%q)", len(events), events)
			}
			if string(events[0].Data) != `{"test":"1"}` || string(events[1].Data) != `[DONE]` {
				t.Fatalf("unexpected data payloads: %q %q", events[0].Data, events[1].Data)
			}
		})
	}
}

// TestFramerPreservesRawAcrossSplitCRLF proves a CRLF split at any byte boundary
// keeps both delimiter bytes in the event's raw bytes and is treated as a single
// terminator (no empty stray event).
func TestFramerPreservesRawAcrossSplitCRLF(t *testing.T) {
	raw := []byte("data: hi\r\n\r\ndata: [DONE]\r\n\r\n")
	// Every chunk boundary, including the CR/LF split, must behave identically.
	for chunk := 1; chunk <= len(raw); chunk++ {
		events, err := collectEvents(raw, chunk)
		if err != nil {
			t.Fatalf("chunk=%d finish: %v", chunk, err)
		}
		if len(events) != 2 {
			t.Fatalf("chunk=%d expected 2 events, got %d", chunk, len(events))
		}
		if string(events[0].Raw) != "data: hi\r\n\r\n" {
			t.Fatalf("chunk=%d raw = %q, want the exact CRLF bytes", chunk, events[0].Raw)
		}
	}
}

// TestFramerStripsSingleLeadingBOM proves one leading UTF-8 BOM is removed even
// when it is split byte-by-byte, so the first data frame is not swallowed.
func TestFramerStripsSingleLeadingBOM(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	bom := []byte{0xEF, 0xBB, 0xBF}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"without_bom_control", body},
		{"with_leading_bom", append(append([]byte(nil), bom...), body...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A one-byte chunk size exercises the BOM split across reads.
			events, err := collectEvents(tc.raw, 1)
			if err != nil {
				t.Fatalf("finish: %v", err)
			}
			if len(events) != 2 {
				t.Fatalf("expected 2 data events, got %d (%q)", len(events), events)
			}
			if !bytes.Contains(events[0].Data, []byte("hello")) {
				t.Fatalf("first data frame was lost: %q", events[0].Data)
			}
		})
	}
}

// TestFramerOnlyStripsLeadingBOM proves a BOM that is not at the stream start is
// preserved as ordinary payload bytes.
func TestFramerOnlyStripsLeadingBOM(t *testing.T) {
	body := append([]byte("data: x\n\ndata: "), []byte{0xEF, 0xBB, 0xBF}...)
	body = append(body, []byte("y\n\n")...)
	events, err := collectEvents(body, 4)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	want := append([]byte{0xEF, 0xBB, 0xBF}, 'y')
	if !bytes.Equal(events[1].Data, want) {
		t.Fatalf("a non-leading BOM must be preserved, got %q", events[1].Data)
	}
}

// TestFramerRejectsTruncatedResidual proves an undispatched partial line at EOF
// is reported as truncated rather than silently dropped.
func TestFramerRejectsTruncatedResidual(t *testing.T) {
	framer := &sseEventFramer{}
	if err := framer.push([]byte(`data: {"usage":`), func(sseEvent) bool { return true }); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := framer.finish(func(sseEvent) bool { return true }); err == nil {
		t.Fatal("a residual half-frame at EOF must be a truncation error")
	}
}
