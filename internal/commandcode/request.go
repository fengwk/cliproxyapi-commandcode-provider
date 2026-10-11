package commandcode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Synthetic workspace facts. The plugin never reads the host filesystem, git
// state, or machine identity; it sends a minimal fixed project description.
const (
	syntheticWorkingDir  = "/workspace/cpa/cpa-project"
	syntheticProjectSlug = "cpa-project"
	syntheticEnvironment = "linux"
	defaultMaxTokens     = 64000
)

type cliCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type cliOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// cliBlock is the union of the wire content-block shapes. Only fields relevant
// to Type are populated; omitempty keeps unrelated keys off the wire.
type cliBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	ToolCallID   string           `json:"toolCallId,omitempty"`
	ToolName     string           `json:"toolName,omitempty"`
	Input        json.RawMessage  `json:"input,omitempty"`
	Output       *cliOutput       `json:"output,omitempty"`
	Image        string           `json:"image,omitempty"`
	MimeType     string           `json:"mimeType,omitempty"`
	CacheControl *cliCacheControl `json:"cache_control,omitempty"`
}

type cliMessage struct {
	Role    string     `json:"role"`
	Content []cliBlock `json:"content"`
}

type cliTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type cliConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type cliParams struct {
	Model           string       `json:"model"`
	Messages        []cliMessage `json:"messages"`
	Tools           []cliTool    `json:"tools"`
	System          []cliBlock   `json:"system"`
	MaxTokens       int64        `json:"max_tokens"`
	Stream          bool         `json:"stream"`
	Temperature     *float64     `json:"temperature,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
}

type cliRequest struct {
	Config         cliConfig `json:"config"`
	Memory         *struct{} `json:"memory"`
	Taste          *struct{} `json:"taste"`
	Skills         *struct{} `json:"skills"`
	PermissionMode string    `json:"permissionMode"`
	ThreadID       string    `json:"threadId"`
	Params         cliParams `json:"params"`
}

// Anthropic-shaped input after the public SDK translator has run. The codec
// understands the documented block union and rejects anything else rather than
// silently dropping semantics.
type antPayload struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system"`
	Messages    []antMessage    `json:"messages"`
	Tools       []antTool       `json:"tools"`
	MaxTokens   json.RawMessage `json:"max_tokens"`
	Temperature *json.Number    `json:"temperature"`
}

type antMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Thinking     string          `json:"thinking"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	Source       *antImageSource `json:"source"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type antImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// BuildRequest converts a canonical Anthropic payload into the 1.79.2
// /alpha/generate body. It uses only client-provided content and no CLI system
// prompt, and validates ThreadID, max_tokens, temperature and reasoning effort.
func BuildRequest(anthropicPayload []byte, opts RequestOptions) ([]byte, error) {
	if !isValidUUID(opts.ThreadID) {
		return nil, invalidRequest("threadId must be a canonical UUID")
	}
	if opts.Model == "" {
		return nil, invalidRequest("model is required")
	}

	trimmed := bytes.TrimSpace(anthropicPayload)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(trimmed) || !json.Valid(trimmed) {
		return nil, invalidRequest("anthropic payload must be a single JSON object")
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var payload antPayload
	if err := dec.Decode(&payload); err != nil {
		return nil, invalidRequest("anthropic payload is not a valid object")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	maxTokens, err := resolveMaxTokens(payload.MaxTokens, opts.Capabilities)
	if err != nil {
		return nil, err
	}

	system, err := buildSystem(payload.System)
	if err != nil {
		return nil, err
	}

	tools, err := buildTools(payload.Tools)
	if err != nil {
		return nil, err
	}

	messages, err := buildMessages(payload.Messages, opts.Capabilities)
	if err != nil {
		return nil, err
	}

	params := cliParams{
		Model:     opts.Model,
		Messages:  messages,
		Tools:     tools,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    true,
	}

	if payload.Temperature != nil {
		temp, err := payload.Temperature.Float64()
		if err != nil || temp < 0 || temp > 2 {
			return nil, invalidRequest("temperature must be a number between 0 and 2")
		}
		params.Temperature = &temp
	}

	if opts.ReasoningEffort != "" {
		if !containsString(opts.Capabilities.ReasoningEfforts, opts.ReasoningEffort) {
			return nil, &Error{
				Code:       CodeReasoningEffort,
				Message:    "reasoning effort is not supported by the selected model",
				HTTPStatus: 400,
			}
		}
		params.ReasoningEffort = opts.ReasoningEffort
	}

	req := cliRequest{
		Config: cliConfig{
			WorkingDir:    syntheticWorkingDir,
			Date:          now.UTC().Format("2006-01-02"),
			Environment:   syntheticEnvironment,
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory:         nil,
		Taste:          nil,
		Skills:         nil,
		PermissionMode: "standard",
		ThreadID:       opts.ThreadID,
		Params:         params,
	}

	out, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return nil, responseStateError("failed to serialize generate request")
	}
	return out, nil
}

func resolveMaxTokens(raw json.RawMessage, caps Capabilities) (int64, error) {
	requested := int64(0)
	if len(raw) > 0 && string(raw) != "null" {
		value, ok := jsonInt64(raw)
		if !ok || value < 0 {
			return 0, invalidRequest("max_tokens must be a non-negative integer")
		}
		requested = value
	}
	chosen := requested
	if chosen == 0 {
		chosen = defaultMaxTokens
	}
	if caps.MaxOutputTokens > 0 {
		if requested > 0 && requested > caps.MaxOutputTokens {
			return 0, invalidRequest("max_tokens exceeds the model output limit")
		}
		if chosen > caps.MaxOutputTokens {
			chosen = caps.MaxOutputTokens
		}
	}
	return chosen, nil
}

func buildSystem(raw json.RawMessage) ([]cliBlock, error) {
	blocks := []cliBlock{}
	if len(raw) == 0 || string(raw) == "null" {
		return blocks, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, invalidRequest("system must be text or text blocks")
		}
		return []cliBlock{{Type: "text", Text: text}}, nil
	}
	var parts []antBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, invalidRequest("system must be text or text blocks")
	}
	for _, part := range parts {
		if part.Type != "text" {
			return nil, unsupported("system block type is not supported")
		}
		block := cliBlock{Type: "text", Text: part.Text}
		cc, err := parseCacheControl(part.CacheControl)
		if err != nil {
			return nil, err
		}
		block.CacheControl = cc
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func buildTools(raw []antTool) ([]cliTool, error) {
	tools := []cliTool{}
	for _, tool := range raw {
		if tool.Name == "" {
			return nil, invalidRequest("tool name is required")
		}
		if !isJSONObjectOrEmpty(tool.InputSchema) {
			return nil, invalidRequest("tool input_schema must be a JSON object")
		}
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, cliTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
	}
	return tools, nil
}

// buildMessages mirrors the CLI's wire mapping: tool results become separate
// "tool" role messages, remaining user parts stay a "user" message, and every
// assistant tool_use is a full "tool-call" block. toolName for a tool_result is
// resolved from the tool_use id found in the same payload.
func buildMessages(messages []antMessage, caps Capabilities) ([]cliMessage, error) {
	names := map[string]string{}
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		blocks, err := decodeBlocks(msg.Content)
		if err != nil {
			return nil, err
		}
		for _, block := range blocks {
			if block.Type == "tool_use" {
				if block.ID == "" || block.Name == "" {
					return nil, invalidRequest("assistant tool_use requires id and name")
				}
				if _, exists := names[block.ID]; exists {
					return nil, invalidRequest("duplicate tool_use id in history")
				}
				names[block.ID] = block.Name
			}
		}
	}

	out := []cliMessage{}
	for _, msg := range messages {
		blocks, err := decodeBlocks(msg.Content)
		if err != nil {
			return nil, err
		}
		switch msg.Role {
		case "assistant":
			wire := []cliBlock{}
			for _, block := range blocks {
				cacheControl, err := parseCacheControl(block.CacheControl)
				if err != nil {
					return nil, err
				}
				switch block.Type {
				case "text":
					wire = append(wire, cliBlock{Type: "text", Text: block.Text, CacheControl: cacheControl})
				case "thinking":
					wire = append(wire, cliBlock{Type: "reasoning", Text: block.Thinking, CacheControl: cacheControl})
				case "redacted_thinking":
					return nil, unsupported("redacted_thinking cannot be represented")
				case "tool_use":
					if !isJSONObjectOrEmpty(block.Input) {
						return nil, invalidRequest("tool_use input must be a JSON object")
					}
					input := block.Input
					if len(input) == 0 {
						input = json.RawMessage(`{}`)
					}
					wire = append(wire, cliBlock{Type: "tool-call", ToolCallID: block.ID, ToolName: block.Name, Input: input, CacheControl: cacheControl})
				default:
					return nil, unsupported("assistant block type is not supported")
				}
			}
			out = append(out, cliMessage{Role: "assistant", Content: wire})
		case "user":
			userBlocks := []cliBlock{}
			toolBlocks := []cliBlock{}
			for _, block := range blocks {
				cacheControl, err := parseCacheControl(block.CacheControl)
				if err != nil {
					return nil, err
				}
				switch block.Type {
				case "text":
					userBlocks = append(userBlocks, cliBlock{Type: "text", Text: block.Text, CacheControl: cacheControl})
				case "image":
					img, err := buildImage(block.Source, caps)
					if err != nil {
						return nil, err
					}
					img.CacheControl = cacheControl
					userBlocks = append(userBlocks, img)
				case "tool_result":
					tool, err := buildToolResult(block, names)
					if err != nil {
						return nil, err
					}
					tool.CacheControl = cacheControl
					toolBlocks = append(toolBlocks, tool)
				default:
					return nil, unsupported("user block type is not supported")
				}
			}
			if len(toolBlocks) > 0 {
				out = append(out, cliMessage{Role: "tool", Content: toolBlocks})
			}
			if len(userBlocks) > 0 {
				out = append(out, cliMessage{Role: "user", Content: userBlocks})
			}
		default:
			return nil, invalidRequest("unsupported message role")
		}
	}
	return out, nil
}

func decodeBlocks(raw json.RawMessage) ([]antBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, invalidRequest("message content is malformed")
		}
		return []antBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []antBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, invalidRequest("message content must be text or blocks")
	}
	return blocks, nil
}

func buildToolResult(block antBlock, names map[string]string) (cliBlock, error) {
	name, ok := names[block.ToolUseID]
	if !ok || name == "" {
		return cliBlock{}, invalidRequest("tool_result references an unknown tool_use id")
	}
	var text string
	if len(block.Content) > 0 && string(block.Content) != "null" {
		if block.Content[0] == '"' {
			if err := json.Unmarshal(block.Content, &text); err != nil {
				return cliBlock{}, invalidRequest("tool_result content is malformed")
			}
		} else {
			var parts []antBlock
			if err := json.Unmarshal(block.Content, &parts); err != nil {
				return cliBlock{}, invalidRequest("tool_result content is malformed")
			}
			lines := []string{}
			for _, part := range parts {
				if part.Type != "text" {
					return cliBlock{}, unsupported("tool_result content type is not supported")
				}
				lines = append(lines, part.Text)
			}
			text = joinLines(lines)
		}
	}
	return cliBlock{
		Type:       "tool-result",
		ToolCallID: block.ToolUseID,
		ToolName:   name,
		Output:     &cliOutput{Type: "text", Value: text},
	}, nil
}

func buildImage(source *antImageSource, caps Capabilities) (cliBlock, error) {
	if source == nil || source.Type != "base64" {
		return cliBlock{}, unsupported("only inline base64 images are supported")
	}
	if !caps.Vision {
		return cliBlock{}, unsupported("the selected model does not support image content")
	}
	if source.MediaType == "" || source.Data == "" {
		return cliBlock{}, invalidRequest("image source requires media_type and data")
	}
	if err := validateInlineImage(source.MediaType, source.Data); err != nil {
		return cliBlock{}, err
	}
	return cliBlock{
		Type:     "image",
		Image:    "data:" + source.MediaType + ";base64," + source.Data,
		MimeType: source.MediaType,
	}, nil
}

// maxInlineImageBytes bounds the decoded inline image payload accepted from a
// client. Larger images are rejected locally instead of being forwarded.
const maxInlineImageBytes = 8 << 20

// validateInlineImage checks an inline base64 image without fetching anything:
// the media type must be a legal image type, the payload must decode as base64
// and the decoded size must stay within the cap.
func validateInlineImage(mime, data string) error {
	if !validImageMime(mime) {
		return unsupported("image mime type is not supported")
	}
	decoded, err := decodeBase64(data)
	if err != nil {
		return invalidRequest("image data is not valid base64")
	}
	if len(decoded) > maxInlineImageBytes {
		return unsupported("inline image exceeds the maximum size")
	}
	return nil
}

func validImageMime(mime string) bool {
	slash := strings.IndexByte(mime, '/')
	if slash <= 0 || slash == len(mime)-1 {
		return false
	}
	if !strings.EqualFold(mime[:slash], "image") {
		return false
	}
	return isMimeToken(mime[:slash]) && isMimeToken(mime[slash+1:])
}

func isMimeToken(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$&^_.+-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func decodeBase64(payload string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(payload)
}

func parseCacheControl(raw json.RawMessage) (*cliCacheControl, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var cc cliCacheControl
	if err := json.Unmarshal(raw, &cc); err != nil || cc.Type == "" {
		return nil, invalidRequest("cache_control is malformed")
	}
	if cc.Type != "ephemeral" {
		return nil, unsupported("cache_control type is not supported")
	}
	switch cc.TTL {
	case "", "5m", "1h":
	default:
		return nil, unsupported("cache_control ttl is not supported")
	}
	return &cc, nil
}

func isJSONObjectOrEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	return raw[0] == '{'
}

func joinLines(lines []string) string {
	var buf bytes.Buffer
	for i, line := range lines {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
	}
	return buf.String()
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// sortedKeys is only used for deterministic diagnostics.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
