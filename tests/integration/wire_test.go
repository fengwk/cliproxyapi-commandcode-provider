//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

// wireServer is a raw-TCP synthetic upstream. Unlike net/http/httptest it does
// not expose a parsed http.Header map: it captures the request line and every
// header line in the exact order they arrived, which is the only trustworthy
// evidence for header ordering. It never leaves the loopback interface and
// holds no real credentials.
type wireServer struct {
	ln net.Listener

	mu         sync.Mutex
	calls      []wireRequest
	directives []*mockDirective
	probes     int
}

// wireRequest is one captured upstream request.
type wireRequest struct {
	Method      string
	Path        string
	Query       string
	HeaderOrder []string // header names exactly as received, in order
	Header      http.Header
	Body        []byte
	Raw         []byte
}

// mockDirective overrides the next matching response to force a failure, a
// truncated stream or a custom payload without any plugin-internal hook. A
// directive matches the request body by default; a non-empty path matches the
// request path instead, which is required for body-less GETs such as the
// read-only billing probes.
type mockDirective struct {
	match       string
	path        string
	status      int
	contentType string
	encoding    string
	body        []byte
	streaming   bool
	truncate    bool
	consumed    bool
}

// wireResponse is one synthetic upstream response.
type wireResponse struct {
	Status      int
	ContentType string
	Encoding    string
	Body        []byte
	// Streaming writes the body in tiny chunks without Content-Length so the
	// client must reassemble partial lines and multi-byte UTF-8 runes.
	Streaming bool
}

func newWireServer() (*wireServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &wireServer{ln: ln}
	go m.serve()
	return m, nil
}

func (m *wireServer) baseURL() string { return "http://" + m.ln.Addr().String() }

func (m *wireServer) Close() { _ = m.ln.Close() }

func (m *wireServer) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handleConn(conn)
	}
}

func (m *wireServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	req, err := readWireRequest(br)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.calls = append(m.calls, *req)
	m.mu.Unlock()
	writeWireResponse(conn, m.route(req))
}

// readWireRequest parses one HTTP/1.1 request, preserving header order.
func readWireRequest(br *bufio.Reader) (*wireRequest, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed request line %q", line)
	}
	req := &wireRequest{Method: parts[0]}
	target := parts[1]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		req.Path, req.Query = target[:i], target[i+1:]
	} else {
		req.Path = target
	}
	raw := append([]byte(nil), []byte(line+"\r\n")...)
	header := http.Header{}
	for {
		hl, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		raw = append(raw, []byte(hl)...)
		hl = strings.TrimRight(hl, "\r\n")
		if hl == "" {
			break
		}
		ci := strings.IndexByte(hl, ':')
		if ci < 0 {
			continue
		}
		name := strings.TrimSpace(hl[:ci])
		req.HeaderOrder = append(req.HeaderOrder, name)
		header.Add(name, strings.TrimSpace(hl[ci+1:]))
	}
	req.Header = header
	if cl := strings.TrimSpace(header.Get("Content-Length")); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad content-length %q", cl)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, err
		}
		req.Body = body
		raw = append(raw, body...)
	}
	req.Raw = raw
	return req, nil
}

func writeWireResponse(conn net.Conn, resp wireResponse) {
	statusText := http.StatusText(resp.Status)
	if statusText == "" {
		statusText = "OK"
	}
	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", resp.Status, statusText)
	if resp.ContentType != "" {
		fmt.Fprintf(&head, "Content-Type: %s\r\n", resp.ContentType)
	}
	if resp.Encoding != "" {
		fmt.Fprintf(&head, "Content-Encoding: %s\r\n", resp.Encoding)
	}
	head.WriteString("Connection: close\r\n")
	if !resp.Streaming {
		fmt.Fprintf(&head, "Content-Length: %d\r\n", len(resp.Body))
	}
	head.WriteString("\r\n")
	_, _ = conn.Write([]byte(head.String()))
	if resp.Streaming {
		for _, b := range resp.Body {
			if _, err := conn.Write([]byte{b}); err != nil {
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
		return
	}
	_, _ = conn.Write(resp.Body)
}

// failNext makes the next request whose body contains match fail with status.
func (m *wireServer) failNext(match string, status int, code string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": code, "message": "synthetic upstream failure"}})
	m.failNextBody(match, status, string(body))
}

// failNextBody is failNext with an explicit response body so tests can exercise
// upstream error classification without changing production code.
func (m *wireServer) failNextBody(match string, status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directives = append(m.directives, &mockDirective{match: match, status: status, contentType: "application/json", body: []byte(body)})
}

// respondNext makes the next request whose body contains match return a custom
// response. Streaming responses are written in tiny chunks without
// Content-Length.
func (m *wireServer) respondNext(match, contentType, body string, streaming bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directives = append(m.directives, &mockDirective{match: match, status: http.StatusOK, contentType: contentType, body: []byte(body), streaming: streaming})
}

// respondBrotli exercises the actual host HTTP bridge, not a decoder-only hook.
func (m *wireServer) respondBrotli(t *testing.T, match string, body []byte) {
	t.Helper()
	var compressed bytes.Buffer
	writer := brotli.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directives = append(m.directives, &mockDirective{
		match: match, status: http.StatusOK, contentType: "application/x-ndjson",
		encoding: "br", body: compressed.Bytes(),
	})
}

// truncateNext makes the next request whose body contains match return a stream
// that ends without a terminal event.
func (m *wireServer) truncateNext(match string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directives = append(m.directives, &mockDirective{match: match, truncate: true})
}

// respondPath makes the next request to the given path return a custom JSON
// response. It is the path-matched counterpart of respondNext for body-less
// GETs.
func (m *wireServer) respondPath(path, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directives = append(m.directives, &mockDirective{path: path, status: http.StatusOK, contentType: "application/json", body: []byte(body)})
}

// takeDirective consumes the first directive matching the request: by path when
// a path is set, otherwise by request-body substring.
func (m *wireServer) takeDirective(req *wireRequest) *mockDirective {
	m.mu.Lock()
	defer m.mu.Unlock()
	text := string(req.Body)
	for _, d := range m.directives {
		if d.consumed {
			continue
		}
		if d.path != "" {
			if d.path != req.Path {
				continue
			}
		} else if !strings.Contains(text, d.match) {
			continue
		}
		d.consumed = true
		return d
	}
	return nil
}

func (m *wireServer) route(req *wireRequest) wireResponse {
	if d := m.takeDirective(req); d != nil {
		if d.truncate {
			return truncatedResponse(req)
		}
		return wireResponse{Status: d.status, ContentType: d.contentType, Encoding: d.encoding, Body: d.body, Streaming: d.streaming}
	}
	switch {
	case req.Method == http.MethodGet && req.Path == "/provider/v1/models":
		return wireResponse{Status: http.StatusOK, ContentType: "application/json", Body: providerModelsBody()}
	case req.Method == http.MethodPost && req.Path == "/provider/v1/chat/completions":
		return providerResponse(req, providerChatNonStream, providerChatStream)
	case req.Method == http.MethodPost && req.Path == "/provider/v1/messages":
		return providerResponse(req, providerMessagesNonStream, providerMessagesStream)
	case req.Method == http.MethodPost && req.Path == "/provider/v1/responses":
		return providerResponse(req, providerResponsesNonStream, providerResponsesStream)
	case req.Method == http.MethodPost && req.Path == "/alpha/generate":
		return wireResponse{Status: http.StatusOK, ContentType: "application/x-ndjson", Body: cliGenerateNDJSON(nativeModelOf(req)), Streaming: true}
	case req.Method == http.MethodGet && req.Path == "/alpha/billing/credits":
		return wireResponse{Status: http.StatusOK, ContentType: "application/json", Body: quotaCreditsBody()}
	case req.Method == http.MethodGet && req.Path == "/alpha/billing/subscriptions":
		return wireResponse{Status: http.StatusOK, ContentType: "application/json", Body: quotaSubscriptionsBody()}
	default:
		return wireResponse{Status: http.StatusNotFound, ContentType: "application/json", Body: []byte(`{"error":{"message":"not found"}}`)}
	}
}

func providerResponse(req *wireRequest, nonStream, stream func(string) []byte) wireResponse {
	native := nativeModelOf(req)
	if bodyStream(req.Body) {
		return wireResponse{Status: http.StatusOK, ContentType: "text/event-stream", Body: stream(native), Streaming: true}
	}
	return wireResponse{Status: http.StatusOK, ContentType: "application/json", Body: nonStream(native)}
}

func truncatedResponse(req *wireRequest) wireResponse {
	// The same match string chooses the path; truncation always uses the
	// transport of the target route.
	if strings.HasPrefix(req.Path, "/provider/v1/chat") || strings.HasPrefix(req.Path, "/provider/v1/responses") {
		return wireResponse{Status: http.StatusOK, ContentType: "text/event-stream", Body: chatStreamTruncated(nativeModelOf(req)), Streaming: true}
	}
	return wireResponse{Status: http.StatusOK, ContentType: "application/x-ndjson", Body: cliGenerateTruncated(), Streaming: true}
}

// nativeModelOf extracts the requested upstream model id.
func nativeModelOf(req *wireRequest) string {
	var decoded struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(req.Body, &decoded)
	return decoded.Model
}

func bodyStream(body []byte) bool {
	var decoded struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &decoded)
	return decoded.Stream
}

// callsForPath returns a snapshot of captured calls for one path.
func (m *wireServer) callsForPath(path string) []wireRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []wireRequest
	for _, c := range m.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (m *wireServer) allCalls() []wireRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]wireRequest, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *wireServer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
	m.directives = nil
}

// ---------------------------------------------------------------------------
// Raw-wire contract assertions
// ---------------------------------------------------------------------------

// requireClean fails the test if any upstream contract violation was recorded.
func (m *wireServer) requireClean(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		for _, problem := range checkWireCall(c) {
			t.Errorf("upstream %s %s violation: %s", c.Method, c.Path, problem)
		}
	}
}

// checkWireCall reports contract problems for one captured call. It deliberately
// uses the raw header order rather than a parsed map for ordering claims.
func checkWireCall(c wireRequest) []string {
	var problems []string
	switch {
	case c.Path == "/alpha/generate":
		problems = append(problems, checkCLIGenerate(c)...)
	case strings.HasPrefix(c.Path, "/provider/v1/chat/completions") ||
		strings.HasPrefix(c.Path, "/provider/v1/messages") ||
		strings.HasPrefix(c.Path, "/provider/v1/responses"):
		problems = append(problems, checkProviderRequest(c)...)
	}
	return problems
}

func checkCLIGenerate(c wireRequest) []string {
	var problems []string
	if !bytes.HasPrefix(c.Raw, []byte("POST /alpha/generate HTTP/1.1\r\n")) || c.Query != "" {
		problems = append(problems, "generate must use POST /alpha/generate HTTP/1.1 without a query")
	}
	var body struct {
		ThreadID string `json:"threadId"`
		Params   struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		} `json:"params"`
		Config struct {
			WorkingDir string `json:"workingDir"`
		} `json:"config"`
	}
	if err := json.Unmarshal(c.Body, &body); err != nil {
		return []string{"generate body is not valid JSON: " + err.Error()}
	}
	if strings.Contains(body.Params.Model, providerID+"/") {
		problems = append(problems, "params.model must be the native id without the public prefix")
	}
	if body.Params.Model == "" {
		problems = append(problems, "params.model is empty")
	}
	if !body.Params.Stream {
		problems = append(problems, "params.stream must always be true for the generate transport")
	}
	if !cliWorkspacePattern.MatchString(body.Config.WorkingDir) {
		problems = append(problems, fmt.Sprintf("config.workingDir = %q is not a legal synthetic workspace", body.Config.WorkingDir))
	}
	if got := headerValue(c, "x-project-slug"); got != strings.ReplaceAll(strings.TrimPrefix(body.Config.WorkingDir, "/"), "/", "-") {
		problems = append(problems, "x-project-slug must derive from the complete config.workingDir")
	}
	if !isUUIDv4(body.ThreadID) {
		problems = append(problems, fmt.Sprintf("threadId %q is not a v4-format id", body.ThreadID))
	}
	if got := headerValue(c, "x-session-id"); got != body.ThreadID {
		problems = append(problems, fmt.Sprintf("x-session-id %q must equal body threadId %q", got, body.ThreadID))
	}
	if got := headerValue(c, "x-command-code-version"); got != "1.88.5" {
		problems = append(problems, fmt.Sprintf("x-command-code-version = %q, want the pinned CLI version", got))
	}
	if got := headerValue(c, "x-cli-environment"); got != "production" {
		problems = append(problems, fmt.Sprintf("x-cli-environment = %q, want production", got))
	}
	if got := headerValue(c, "x-taste-learning"); got != "false" {
		problems = append(problems, fmt.Sprintf("x-taste-learning = %q, want false (privacy-preserving difference)", got))
	}
	if got := headerValue(c, "user-agent"); got != "cli" {
		problems = append(problems, fmt.Sprintf("User-Agent = %q, want cli", got))
	}
	if _, ok := bearerKey(c); !ok {
		problems = append(problems, "Authorization Bearer credential is missing")
	}
	// Lock the complete ordinary envelope, including key order; no extra
	// mode/promptCache/OAuth fields may slip into the wire fixture.
	decoder := json.NewDecoder(bytes.NewReader(c.Body))
	_, _ = decoder.Token()
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return append(problems, err.Error())
		}
		keys = append(keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return append(problems, err.Error())
		}
	}
	if !slices.Equal(keys, []string{"config", "memory", "taste", "skills", "permissionMode", "threadId", "params"}) {
		problems = append(problems, fmt.Sprintf("ordinary generate envelope keys = %v", keys))
	}
	problems = append(problems, checkCLIHeaderOrder(c)...)
	return problems
}

var cliWorkspacePattern = regexp.MustCompile(`^/home/[a-z]+/proj/[a-z0-9]+(?:-[a-z0-9]+)*$`)

// The complete 1.88.5 order is case-sensitive and includes host-owned fields.
// The intentional host-policy difference is one connection: close, not native
// keep-alive. TLS/connection reuse cannot be inferred from this HTTP fixture.
var cliHeaderSequence = []string{
	"host",
	"connection",
	"content-type",
	"User-Agent",
	"x-command-code-version",
	"x-cli-environment",
	"x-project-slug",
	"x-taste-learning",
	"x-session-id",
	"Authorization",
	"x-cmd-zdr",
	"traceparent",
	"accept",
	"accept-language",
	"sec-fetch-mode",
	"accept-encoding",
	"content-length",
}

func checkCLIHeaderOrder(c wireRequest) []string {
	var problems []string
	required := make([]string, 0, len(cliHeaderSequence))
	for _, name := range cliHeaderSequence {
		if name == "x-cmd-zdr" && len(c.Header.Values(name)) == 0 {
			continue
		}
		required = append(required, name)
		if len(c.Header.Values(name)) != 1 {
			problems = append(problems, name+" must occur exactly once")
		}
	}
	if !slices.Equal(c.HeaderOrder, required) {
		problems = append(problems, fmt.Sprintf("raw header order = %v, want exactly %v", c.HeaderOrder, required))
	}
	if headerValue(c, "host") == "" || headerValue(c, "content-length") != strconv.Itoa(len(c.Body)) {
		problems = append(problems, "host/content-length must match the actual authority/body")
	}
	if got := headerValue(c, "connection"); got != "close" {
		problems = append(problems, fmt.Sprintf("connection = %q, want single close", got))
	}
	if got := headerValue(c, "content-type"); got != "application/json, application/json" {
		problems = append(problems, fmt.Sprintf("content-type = %q, want the captured merged value", got))
	}
	if got := headerValue(c, "accept"); got != "*/*" {
		problems = append(problems, fmt.Sprintf("accept = %q, want */*", got))
	}
	if got := headerValue(c, "accept-language"); got != "*" {
		problems = append(problems, fmt.Sprintf("accept-language = %q, want *", got))
	}
	if got := headerValue(c, "sec-fetch-mode"); got != "cors" {
		problems = append(problems, fmt.Sprintf("sec-fetch-mode = %q, want cors", got))
	}
	if got := headerValue(c, "accept-encoding"); got != "br, gzip, deflate" {
		problems = append(problems, fmt.Sprintf("accept-encoding = %q, want br, gzip, deflate", got))
	}
	// A failure that asked for ZDR must send exactly the opt-in marker.
	if got := headerValue(c, "x-cmd-zdr"); got != "" && got != "1" {
		problems = append(problems, fmt.Sprintf("x-cmd-zdr = %q, want 1 when present", got))
	}
	return problems
}

func checkProviderRequest(c wireRequest) []string {
	var problems []string
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(c.Body, &body); err != nil {
		return []string{"provider body is not valid JSON: " + err.Error()}
	}
	if strings.Contains(body.Model, providerID+"/") {
		problems = append(problems, "provider model must be the native id without the public prefix")
	}
	if _, ok := bearerKey(c); !ok {
		problems = append(problems, "Authorization Bearer credential is missing")
	}
	// The previous provider's session header must never leak into this plugin.
	if got := headerValue(c, "x-opencode-session"); got != "" {
		problems = append(problems, "x-opencode-session must never be sent by this plugin")
	}
	if c.Path == "/provider/v1/messages" {
		if got := headerValue(c, "anthropic-version"); got == "" {
			problems = append(problems, "Anthropic messages endpoint requires the anthropic-version header")
		}
	}
	return problems
}

// headerValue returns the first value for a case-insensitive header name.
func headerValue(c wireRequest, name string) string {
	return c.Header.Get(name)
}

// bearerKey extracts the credential from the Authorization header.
func bearerKey(c wireRequest) (string, bool) {
	raw := headerValue(c, "authorization")
	raw = strings.TrimSpace(raw)
	if len(raw) < 7 || !strings.EqualFold(raw[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(raw[7:]), true
}

// isUUIDv4 checks both the version and RFC variant; derivation isn't randomness.
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return s[14] == '4' && strings.ContainsRune("89abAB", rune(s[19]))
}

// The golden checker itself must reject the old subsequence loopholes.
func TestStrictCLIHeaderFixture(t *testing.T) {
	base := wireRequest{Body: []byte("{}"), Header: http.Header{}}
	for _, name := range cliHeaderSequence {
		if name != "x-cmd-zdr" {
			base.HeaderOrder = append(base.HeaderOrder, name)
			base.Header.Set(name, "fixture")
		}
	}
	for name, value := range map[string]string{
		"host": "127.0.0.1", "content-length": "2", "connection": "close",
		"content-type": "application/json, application/json", "accept": "*/*",
		"accept-language": "*", "sec-fetch-mode": "cors", "accept-encoding": "br, gzip, deflate",
	} {
		base.Header.Set(name, value)
	}
	if problems := checkCLIHeaderOrder(base); len(problems) != 0 {
		t.Fatalf("valid golden rejected: %v", problems)
	}
	for _, mutation := range []string{"extra", "duplicate", "casing", "order", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			call := base
			call.HeaderOrder = slices.Clone(base.HeaderOrder)
			call.Header = base.Header.Clone()
			switch mutation {
			case "extra":
				call.HeaderOrder = append(call.HeaderOrder, "x-extra")
			case "duplicate":
				call.HeaderOrder = append(call.HeaderOrder, "connection")
				call.Header.Add("connection", "close")
			case "casing":
				call.HeaderOrder[3] = "user-agent"
			case "order":
				call.HeaderOrder[0], call.HeaderOrder[1] = call.HeaderOrder[1], call.HeaderOrder[0]
			case "missing":
				call.HeaderOrder = call.HeaderOrder[1:]
			}
			if len(checkCLIHeaderOrder(call)) == 0 {
				t.Fatal("invalid complete wire profile was accepted")
			}
		})
	}
}

// All client formats must consume Brotli upstream data, in buffered and
// streamed execution, without changing the request fingerprint or usage.
func TestBrotliClientProtocols(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "brotli"); status != http.StatusOK {
		t.Fatal("import keys failed")
	}
	for _, client := range allClients() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", client.name, stream), func(t *testing.T) {
				session := fmt.Sprintf("brotli-%s-%t", client.name, stream)
				h.wire.reset()
				h.wire.respondBrotli(t, markerFor(session), cliGenerateNDJSON(familyChatResp))
				obs := client.observe(t, h, familyChatResp, session, stream)
				if obs.Text != fixtureText || (stream && !obs.Terminal) {
					t.Errorf("incomplete decoded response: text=%q terminal=%t", obs.Text, obs.Terminal)
				}
				assertUsage(t, obs.Usage, expectedUsage("/alpha/generate", client.kind), "Brotli response")
				h.wire.requireClean(t)
			})
		}
	}
}
