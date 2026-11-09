package commandcode

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// The wire encoder must render like native JSON.stringify on the
// plugin-controllable parts: raw HTML characters, literal U+2028/U+2029 and no
// trailing newline.
func TestMarshalWireNativeShape(t *testing.T) {
	type doc struct {
		Text string `json:"t"`
	}
	sep8 := string(rune(0x2028))
	sep9 := string(rune(0x2029))
	out, err := marshalWire(doc{Text: "a<b>&c" + sep8 + "d" + sep9})
	if err != nil {
		t.Fatalf("marshalWire: %v", err)
	}
	want := `{"t":"a<b>&c` + sep8 + `d` + sep9 + `"}`
	if !bytes.Equal(out, []byte(want)) {
		t.Fatalf("wire bytes = %q, want %q", out, want)
	}
}

// Escape-layer discrimination: only escapes that actually encode
// U+2028/U+2029 become real separators. A client string that literally
// contains the characters "\u2028" must survive byte for byte, and a real
// backslash directly before a real separator must stay a backslash.
func TestMarshalWireEscapeLayers(t *testing.T) {
	sep := string(rune(0x2028))
	type doc struct {
		Text string `json:"t"`
	}
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "written out escape stays literal text",
			input: sep + "\\u2028" + sep,
			want:  sep + `\\u2028` + sep,
		},
		{
			// input is backslash + real U+2028: the backslash is encoded as
			// an escaped pair and the separator stays a lone escape before
			// the rewrite, so the pair must not swallow the \u2028 escape.
			name:  "backslash before separator keeps pairing",
			input: "\\" + sep,
			want:  `\\` + sep,
		},
		{
			name:  "two literal backslashes then plain digits",
			input: "\\\\u2028",
			want:  `\\\\u2028`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := marshalWire(doc{Text: tc.input})
			if err != nil {
				t.Fatalf("marshalWire: %v", err)
			}
			wantDoc := `{"t":"` + tc.want + `"}`
			if !bytes.Equal(out, []byte(wantDoc)) {
				t.Fatalf("wire bytes = %q, want %q", out, wantDoc)
			}
			var back doc
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatalf("roundtrip unmarshal: %v", err)
			}
			if back.Text != tc.input {
				t.Fatalf("roundtrip text = %q, want %q", back.Text, tc.input)
			}
		})
	}
}

// Numbers and key order inside embedded raw JSON payloads and the compacted
// whitespace removal must not alter the embedded content.
func TestMarshalWireRawPassThrough(t *testing.T) {
	type doc struct {
		Raw json.RawMessage `json:"raw"`
	}
	out, err := marshalWire(doc{Raw: json.RawMessage("{\n  \"nums\" : [1e-7, 9007199254740993], \"order\" : \"z<>&\"}")})
	if err != nil {
		t.Fatalf("marshalWire: %v", err)
	}
	want := `{"raw":{"nums":[1e-7,9007199254740993],"order":"z<>&"}}`
	if !bytes.Equal(out, []byte(want)) {
		t.Fatalf("wire bytes = %q, want %q", out, want)
	}
}

// Arbitrary printable strings must survive one encode/decode roundtrip with
// unchanged characters; this property directly covers the escape-boundary
// logic of the separator rewrite at adversarial inputs.
func FuzzMarshalWireStringRoundTrip(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		string(rune(0x2028)),
		string(rune(0x2029)),
		`a\u2028b`,
		"\\u2028",
		"\\" + string(rune(0x2028)),
		`\"` + string(rune(0x2028)),
		"tab\tnl\nquote\"",
		"<b>&",
		"0123456789012345678901234567890",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	type doc struct {
		Text string `json:"t"`
	}
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			// encoding/json replaces invalid UTF-8 with U+FFFD before the
			// rewrite runs, so equality cannot hold for such input.
			return
		}
		out, err := marshalWire(doc{Text: text})
		if err != nil {
			t.Fatalf("marshalWire(%q): %v", text, err)
		}
		if bytes.HasSuffix(out, []byte{'\n'}) {
			t.Fatalf("trailing newline present: %q", out)
		}
		var back doc
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("roundtrip unmarshal(%q): %v; bytes %q", text, err, out)
		}
		if back.Text != text {
			t.Fatalf("roundtrip text = %q, want %q; bytes %q", back.Text, text, out)
		}
	})
}
