package commandcode

import (
	"bytes"
	"encoding/json"
)

// marshalWire renders the final generate body the way the native CLI's
// JavaScript JSON.stringify does on the wire: no HTML escaping, literal
// U+2028/U+2029 separators and no trailing newline, while keeping the payload
// a single valid JSON value. Numbers and object key order inside embedded
// JSON.RawMessage payloads pass through untouched.
func marshalWire(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder appends exactly one newline after the value; the CLI sends
	// the body without it.
	out := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	return unescapeJSONLineSeparators(out), nil
}

// unescapeJSONLineSeparators rewrites the \u2028 and \u2029 escape sequences
// produced by encoding/json into their literal characters. Go always escapes
// those two code points even with SetEscapeHTML(false), while JSON.stringify
// and RFC 8259 both allow them raw inside JSON strings.
//
// The rewrite must respect escape boundaries: a client string that literally
// contains the characters "\u2028" is encoded as "\\u2028" (an escaped
// backslash followed by plain characters), and a blind global replace would
// turn that user text into a real separator. Parsing each string escape by
// escape keeps the pairing, so only escapes that actually encode U+2028/U+2029
// are rewritten.
func unescapeJSONLineSeparators(data []byte) []byte {
	if !bytes.Contains(data, []byte(`\u2028`)) && !bytes.Contains(data, []byte(`\u2029`)) {
		return data
	}
	out := make([]byte, 0, len(data))
	inString := false
	for i := 0; i < len(data); {
		c := data[i]
		switch {
		case c == '"':
			// A quote that is itself escaped is not a boundary; escaped
			// quotes only occur inside strings, so a plain quote toggles.
			inString = !inString
			out = append(out, c)
			i++
		case !inString || c != '\\':
			// Structural content and ordinary characters are copied verbatim.
			out = append(out, c)
			i++
		case i+1 >= len(data):
			// Defensive: keep a truncated trailing escape byte for byte.
			out = append(out, c)
			i++
		default:
			next := data[i+1]
			switch {
			case next == 'u' && i+6 <= len(data):
				code, ok := unescapeHex(data[i+2 : i+6])
				if ok && code == 0x2028 {
					out = append(out, 0xe2, 0x80, 0xa8)
				} else if ok && code == 0x2029 {
					out = append(out, 0xe2, 0x80, 0xa9)
				} else {
					// Unknown or unpaired code point: copy the escape verbatim.
					out = append(out, data[i:i+6]...)
				}
				i += 6
			default:
				// Two-byte escapes (\" \\ \n \r \t) keep their pairing, so a
				// following \u2028 is still understood as a real separator.
				out = append(out, data[i:i+2]...)
				i += 2
			}
		}
	}
	return out
}

// unescapeHex parses exactly four lowercase/uppercase hex digits.
func unescapeHex(digits []byte) (int, bool) {
	if len(digits) != 4 {
		return 0, false
	}
	value := 0
	for _, c := range digits {
		var digit int
		switch {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'f':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = int(c-'A') + 10
		default:
			return 0, false
		}
		value = value<<4 | digit
	}
	return value, true
}
