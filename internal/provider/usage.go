package provider

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file is a small usage projection layer. The public SDK response
// translators intentionally model only the documented fields of their target
// protocol, so a cross-protocol conversion can drop a bucket that really was
// present upstream (for example a Responses body has no standard cache-write
// field, and Chat exposes reasoning under completion_tokens_details). The
// plugin keeps an authoritative snapshot of the native usage and folds back the
// buckets that really existed, without forking or reimplementing the SDK and
// without inventing zeros for buckets the source never carried.

// usageSnapshot is an authoritative token picture. A nil field means the source
// protocol did not carry that bucket and it must never be invented. For a Chat
// or Responses source, input is the inclusive prompt total and cacheRead /
// cacheWrite are subsets of it; for a Claude source, input is the exclusive,
// cache-independent input_tokens (U) so that U = I - read - write.
type usageSnapshot struct {
	input      *int64
	output     *int64
	total      *int64
	cacheRead  *int64
	cacheWrite *int64
	reasoning  *int64
	claude     bool
}

// present reports whether the snapshot carries any authoritative bucket.
func (s usageSnapshot) present() bool {
	return s.input != nil || s.output != nil || s.total != nil ||
		s.cacheRead != nil || s.cacheWrite != nil || s.reasoning != nil
}

// merge folds a later snapshot over this one without clearing buckets the later
// snapshot omits. Streaming protocols split a single usage across events (for
// example Claude input on message_start and output on message_delta).
func (s *usageSnapshot) merge(o usageSnapshot) {
	if o.input != nil {
		s.input = o.input
	}
	if o.output != nil {
		s.output = o.output
	}
	if o.total != nil {
		s.total = o.total
	}
	if o.cacheRead != nil {
		s.cacheRead = o.cacheRead
	}
	if o.cacheWrite != nil {
		s.cacheWrite = o.cacheWrite
	}
	if o.reasoning != nil {
		s.reasoning = o.reasoning
	}
	if o.claude {
		s.claude = true
	}
}

// usageToken parses an authoritative token count. Fractional or exponent forms
// are rejected rather than silently rounded, and negative counts are refused.
func usageToken(value gjson.Result) *int64 {
	if !value.Exists() {
		return nil
	}
	var raw string
	switch value.Type {
	case gjson.Number:
		raw = strings.TrimSpace(value.Raw)
	case gjson.String:
		raw = strings.TrimSpace(value.String())
	default:
		return nil
	}
	if raw == "" {
		return nil
	}
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '.', 'e', 'E':
			return nil
		}
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

// snapshotUsage extracts the authoritative usage from one native payload. It
// accepts both the non-streaming document and the streaming event that carries
// the usage for each protocol. Naming variants the host pairs with a protocol
// (cache_write_tokens / cache_creation_input_tokens / cache_creation_tokens)
// are all recognized.
func snapshotUsage(format translator.Format, data []byte) usageSnapshot {
	var s usageSnapshot
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return s
	}
	root := gjson.ParseBytes(data)
	switch format {
	case translator.FormatClaude:
		s.claude = true
		usage := root.Get("usage")
		if !usage.IsObject() {
			usage = root.Get("message.usage")
		}
		if !usage.IsObject() {
			return s
		}
		s.input = usageToken(usage.Get("input_tokens"))
		s.output = usageToken(usage.Get("output_tokens"))
		s.cacheRead = usageToken(usage.Get("cache_read_input_tokens"))
		s.cacheWrite = firstToken(
			usage.Get("cache_creation_input_tokens"),
			usage.Get("cache_write_input_tokens"),
			usage.Get("cache_creation_tokens"),
		)
	case translator.FormatOpenAIResponse:
		usage := root.Get("usage")
		if !usage.IsObject() {
			usage = root.Get("response.usage")
		}
		if !usage.IsObject() {
			return s
		}
		s.input = usageToken(usage.Get("input_tokens"))
		s.output = usageToken(usage.Get("output_tokens"))
		s.total = usageToken(usage.Get("total_tokens"))
		s.cacheRead = usageToken(usage.Get("input_tokens_details.cached_tokens"))
		s.cacheWrite = firstToken(
			usage.Get("input_tokens_details.cache_write_tokens"),
			usage.Get("cache_write_tokens"),
			usage.Get("input_tokens_details.cache_creation_tokens"),
		)
		s.reasoning = usageToken(usage.Get("output_tokens_details.reasoning_tokens"))
	default:
		usage := root.Get("usage")
		if !usage.IsObject() {
			return s
		}
		s.input = usageToken(usage.Get("prompt_tokens"))
		s.output = usageToken(usage.Get("completion_tokens"))
		s.total = usageToken(usage.Get("total_tokens"))
		s.cacheRead = usageToken(usage.Get("prompt_tokens_details.cached_tokens"))
		s.cacheWrite = firstToken(
			usage.Get("prompt_tokens_details.cache_write_tokens"),
			usage.Get("prompt_tokens_details.cache_creation_tokens"),
			usage.Get("prompt_tokens_details.cache_creation_input_tokens"),
		)
		s.reasoning = usageToken(usage.Get("completion_tokens_details.reasoning_tokens"))
	}
	return s
}

func firstToken(values ...gjson.Result) *int64 {
	for _, value := range values {
		if parsed := usageToken(value); parsed != nil {
			return parsed
		}
	}
	return nil
}

// inclusiveInput returns the inclusive prompt total when the source carries it
// directly or it is fully derivable from a Claude source. It is nil when a
// bucket needed for the derivation is missing, so the caller never guesses.
func (s usageSnapshot) inclusiveInput() *int64 {
	if !s.claude {
		return s.input
	}
	if s.input == nil || s.cacheRead == nil || s.cacheWrite == nil {
		return nil
	}
	return sumTokens(*s.input, *s.cacheRead, *s.cacheWrite)
}

// exclusiveInput returns the Anthropic input_tokens (U). A Claude source already
// carries it; a Chat/Responses source must expose both cache subsets so the
// subtraction stays exact.
func (s usageSnapshot) exclusiveInput() *int64 {
	if s.claude {
		return s.input
	}
	if s.input == nil || s.cacheRead == nil || s.cacheWrite == nil {
		return nil
	}
	if *s.cacheRead+*s.cacheWrite > *s.input {
		return nil
	}
	value := *s.input - *s.cacheRead - *s.cacheWrite
	return &value
}

// totalOrDerived returns the total when explicit, otherwise the sum of the
// inclusive input and output when both are authoritative.
func (s usageSnapshot) totalOrDerived(inclusive *int64) *int64 {
	if s.total != nil {
		return s.total
	}
	if inclusive == nil || s.output == nil {
		return nil
	}
	return sumTokens(*inclusive, *s.output)
}

// boundedReasoning returns reasoning tokens only while they remain a subset of
// the output total, keeping the projection mathematically self-consistent.
func (s usageSnapshot) boundedReasoning() *int64 {
	if s.reasoning == nil {
		return nil
	}
	if s.output != nil && *s.reasoning > *s.output {
		return nil
	}
	return s.reasoning
}

// boundedCache keeps cache read and write within the inclusive input total when
// it is known, dropping any bucket that would break the accounting identity.
func (s usageSnapshot) boundedCache(inclusive *int64) (read, write *int64) {
	read, write = s.cacheRead, s.cacheWrite
	if inclusive == nil {
		return read, write
	}
	if read != nil && (*read < 0 || *read > *inclusive) {
		read = nil
	}
	if write != nil {
		limit := *inclusive
		if read != nil {
			limit -= *read
		}
		if *write < 0 || *write > limit {
			write = nil
		}
	}
	return read, write
}

func sumTokens(values ...int64) *int64 {
	var total int64
	for _, value := range values {
		if value < 0 {
			return nil
		}
		if total > 1<<62-value {
			return nil
		}
		total += value
	}
	return &total
}

// usageProjector owns the running native snapshot for one logical response and
// projects it onto every cross-protocol client frame.
type usageProjector struct {
	source translator.Format
	client translator.Format
	snap   usageSnapshot
}

func (p *usageProjector) enabled() bool {
	return p.source != "" && p.client != "" && p.source != p.client
}

// observe folds one native payload into the running snapshot.
func (p *usageProjector) observe(data []byte) {
	if !p.enabled() {
		return
	}
	p.snap.merge(snapshotUsage(p.source, data))
}

// project corrects one client frame in place, rebuilding an SSE frame only when
// the projection actually changed the usage object.
func (p *usageProjector) project(frame []byte) []byte {
	if !p.enabled() || !p.snap.present() {
		return frame
	}
	return overlayFrameUsage(p.client, frame, p.snap)
}

// overlayFrameUsage projects the snapshot onto one client frame. A frame that
// carries no usage object is returned unchanged, so no extra usage is ever
// emitted after completion and native passthrough is never rewritten from
// scratch.
func overlayFrameUsage(client translator.Format, frame []byte, s usageSnapshot) []byte {
	event, ok := parseCanonicalFrame(frame)
	if !ok || len(event.Data) == 0 {
		return frame
	}
	patched := projectUsageJSON(client, event.Data, s)
	if bytes.Equal(patched, event.Data) {
		return frame
	}
	if name := sseEventName(event.Raw); name != "" {
		out := make([]byte, 0, len(name)+len(patched)+16)
		out = append(out, "event: "...)
		out = append(out, name...)
		out = append(out, "\ndata: "...)
		out = append(out, patched...)
		out = append(out, "\n\n"...)
		return out
	}
	return patched
}

// sseEventName returns the event name of a raw SSE frame, or "" when the frame
// carries no name.
func sseEventName(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "event:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			break
		}
	}
	return ""
}

// projectUsageJSON overlays the authoritative buckets onto the usage object a
// client payload already carries, dispatching on the client protocol.
func projectUsageJSON(client translator.Format, payload []byte, s usageSnapshot) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	switch client {
	case translator.FormatClaude:
		return projectClaudeUsage(payload, s)
	case translator.FormatOpenAIResponse:
		return projectResponsesUsage(payload, s)
	default:
		return projectChatUsage(payload, s)
	}
}

func setToken(out []byte, path string, value *int64) []byte {
	if value == nil {
		return out
	}
	if updated, err := sjson.SetBytes(out, path, *value); err == nil {
		return updated
	}
	return out
}

func projectChatUsage(payload []byte, s usageSnapshot) []byte {
	if !gjson.GetBytes(payload, "usage").IsObject() {
		return payload
	}
	inclusive := s.inclusiveInput()
	read, write := s.boundedCache(inclusive)
	out := payload
	out = setToken(out, "usage.prompt_tokens", inclusive)
	out = setToken(out, "usage.completion_tokens", s.output)
	out = setToken(out, "usage.total_tokens", s.totalOrDerived(inclusive))
	out = setToken(out, "usage.prompt_tokens_details.cached_tokens", read)
	out = setToken(out, "usage.prompt_tokens_details.cache_write_tokens", write)
	out = setToken(out, "usage.completion_tokens_details.reasoning_tokens", s.boundedReasoning())
	return out
}

func projectResponsesUsage(payload []byte, s usageSnapshot) []byte {
	base := "usage"
	if gjson.GetBytes(payload, "response.usage").IsObject() {
		base = "response.usage"
	} else if !gjson.GetBytes(payload, "usage").IsObject() {
		return payload
	}
	inclusive := s.inclusiveInput()
	read, write := s.boundedCache(inclusive)
	out := payload
	out = setToken(out, base+".input_tokens", inclusive)
	out = setToken(out, base+".output_tokens", s.output)
	out = setToken(out, base+".total_tokens", s.totalOrDerived(inclusive))
	out = setToken(out, base+".input_tokens_details.cached_tokens", read)
	out = setToken(out, base+".input_tokens_details.cache_write_tokens", write)
	out = setToken(out, base+".output_tokens_details.reasoning_tokens", s.boundedReasoning())
	return out
}

func projectClaudeUsage(payload []byte, s usageSnapshot) []byte {
	base := "usage"
	if !gjson.GetBytes(payload, "usage").IsObject() {
		if gjson.GetBytes(payload, "message.usage").IsObject() {
			base = "message.usage"
		} else {
			return payload
		}
	}
	out := payload
	out = setToken(out, base+".input_tokens", s.exclusiveInput())
	out = setToken(out, base+".output_tokens", s.output)
	out = setToken(out, base+".cache_read_input_tokens", s.cacheRead)
	out = setToken(out, base+".cache_creation_input_tokens", s.cacheWrite)
	return out
}
