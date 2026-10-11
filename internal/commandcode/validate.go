package commandcode

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ValidateClientRequest inspects a raw client request (OpenAI Chat, Anthropic
// Messages or OpenAI Responses / codex) before the public SDK translator runs.
// It rejects semantics the CLI generate wire cannot express losslessly so the
// SDK never silently drops them. It performs no network or filesystem access.
func ValidateClientRequest(body []byte, protocol string, caps Capabilities) error {
	obj, err := decodeRequestObject(body)
	if err != nil {
		return err
	}
	switch normalizeProtocol(protocol) {
	case "openai":
		return validateOpenAIChat(obj, caps)
	case "claude":
		return validateAnthropic(obj, caps)
	case "openai-response":
		return validateResponses(obj, caps)
	default:
		return invalidRequest("unsupported client protocol")
	}
}

func normalizeProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "chat", "openai-chat", "openai_chat":
		return "openai"
	case "claude", "anthropic", "anthropic-messages", "messages":
		return "claude"
	case "openai-response", "openai-responses", "responses", "codex":
		return "openai-response"
	default:
		return ""
	}
}

func decodeRequestObject(body []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalidRequest("request body must be a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, invalidRequest("request body must be a JSON object")
	}
	if obj == nil {
		return nil, invalidRequest("request body must be a JSON object")
	}
	return obj, nil
}

func rejectUnknownKeys(obj map[string]json.RawMessage, allowed map[string]bool) error {
	for _, key := range sortedKeys(obj) {
		if !allowed[key] {
			return unsupported("request field is not supported by the CLI wire")
		}
	}
	return nil
}

func keySet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return set
}

// checkDeclaredLimit rejects an explicit output limit above the model cap.
func checkDeclaredLimit(obj map[string]json.RawMessage, caps Capabilities, keys ...string) error {
	if caps.MaxOutputTokens <= 0 {
		return nil
	}
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok || !present(&raw) {
			continue
		}
		value, ok := jsonInt64(raw)
		if !ok || value < 0 {
			return invalidRequest("output token limit must be a non-negative integer")
		}
		if value > caps.MaxOutputTokens {
			return invalidRequest("output token limit exceeds the model limit")
		}
	}
	return nil
}

func validateOpenAIChat(obj map[string]json.RawMessage, caps Capabilities) error {
	if err := rejectUnknownKeys(obj, keySet(
		"model", "messages", "max_tokens", "max_completion_tokens", "temperature",
		"stream", "tools", "tool_choice", "response_format", "n",
		"reasoning_effort", "stream_options", "parallel_tool_calls",
	)); err != nil {
		return err
	}
	if err := checkDeclaredLimit(obj, caps, "max_tokens", "max_completion_tokens"); err != nil {
		return err
	}
	if raw, ok := obj["n"]; ok && present(&raw) {
		value, okInt := jsonInt64(raw)
		if !okInt || value != 1 {
			return unsupported("only n=1 is supported")
		}
	}
	if raw, ok := obj["response_format"]; ok && present(&raw) {
		if err := validateTextResponseFormat(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["tool_choice"]; ok && present(&raw) {
		if err := validateToolChoice(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["parallel_tool_calls"]; ok && present(&raw) {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err != nil {
			return invalidRequest("parallel_tool_calls must be a boolean")
		}
		if !enabled {
			return unsupported("disabling parallel tool calls is not supported")
		}
	}
	if raw, ok := obj["reasoning_effort"]; ok && present(&raw) {
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil {
			return invalidRequest("reasoning_effort must be a string")
		}
		if !containsString(caps.ReasoningEfforts, effort) {
			return &Error{Code: CodeReasoningEffort, Message: "reasoning effort is not supported by the selected model", HTTPStatus: 400}
		}
	}
	if raw, ok := obj["tools"]; ok && present(&raw) {
		if err := validateChatTools(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["messages"]; ok && present(&raw) {
		return validateChatMessages(raw, caps)
	}
	return invalidRequest("messages are required")
}

func validateTextResponseFormat(raw json.RawMessage) error {
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return invalidRequest("response_format must be an object")
	}
	if format.Type != "" && format.Type != "text" {
		return unsupported("only plain text response_format is supported")
	}
	return nil
}

func validateToolChoice(raw json.RawMessage) error {
	if raw[0] == '"' {
		var choice string
		if err := json.Unmarshal(raw, &choice); err != nil {
			return invalidRequest("tool_choice is malformed")
		}
		switch choice {
		case "auto", "none":
			return nil
		default:
			return unsupported("forced tool_choice is not supported")
		}
	}
	return unsupported("forced tool_choice is not supported")
}

func validateChatTools(raw json.RawMessage) error {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalidRequest("tools must be an array")
	}
	for _, tool := range tools {
		typ, ok := tool["type"]
		if !ok {
			return unsupported("only function tools are supported")
		}
		var kind string
		if err := json.Unmarshal(typ, &kind); err != nil || kind != "function" {
			return unsupported("only function tools are supported")
		}
	}
	return nil
}

func validateChatMessages(raw json.RawMessage, caps Capabilities) error {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return invalidRequest("messages must be an array")
	}
	for _, message := range messages {
		roleRaw, ok := message["role"]
		if !ok {
			return invalidRequest("message role is required")
		}
		var role string
		if err := json.Unmarshal(roleRaw, &role); err != nil {
			return invalidRequest("message role must be a string")
		}
		switch role {
		case "system", "developer", "user":
			content, ok := message["content"]
			if !ok {
				return invalidRequest("message content is required")
			}
			if err := validateChatUserContent(content, caps); err != nil {
				return err
			}
		case "assistant":
			if content, ok := message["content"]; ok && present(&content) {
				if err := validateChatTextContent(content, caps); err != nil {
					return err
				}
			}
			if calls, ok := message["tool_calls"]; ok && present(&calls) {
				if err := validateChatToolCalls(calls); err != nil {
					return err
				}
			}
		case "tool":
			content, ok := message["content"]
			if !ok {
				return invalidRequest("tool message content is required")
			}
			if err := validateChatTextContent(content, caps); err != nil {
				return err
			}
		default:
			return unsupported("message role is not supported")
		}
	}
	return nil
}

func validateChatUserContent(raw json.RawMessage, caps Capabilities) error {
	if raw[0] == '"' || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return invalidRequest("message content must be text or parts")
	}
	for _, part := range parts {
		typRaw, ok := part["type"]
		if !ok {
			return unsupported("content part type is not supported")
		}
		var typ string
		if err := json.Unmarshal(typRaw, &typ); err != nil {
			return unsupported("content part type is not supported")
		}
		switch typ {
		case "text":
		case "image_url":
			if err := validateChatImage(part["image_url"], caps); err != nil {
				return err
			}
		default:
			return unsupported("content part type is not supported")
		}
	}
	return nil
}

func validateChatTextContent(raw json.RawMessage, caps Capabilities) error {
	if raw[0] == '"' || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return invalidRequest("message content must be text or parts")
	}
	for _, part := range parts {
		var typ string
		if typRaw, ok := part["type"]; ok {
			_ = json.Unmarshal(typRaw, &typ)
		}
		if typ != "text" {
			return unsupported("content part type is not supported")
		}
	}
	return nil
}

func validateChatImage(raw json.RawMessage, caps Capabilities) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalidRequest("image_url is required")
	}
	var image struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &image); err != nil || image.URL == "" {
		return invalidRequest("image_url must contain a url")
	}
	return validateDataImageURL(image.URL, caps)
}

func validateDataImageURL(url string, caps Capabilities) error {
	if !strings.HasPrefix(url, "data:") {
		return unsupported("remote image URLs are not supported")
	}
	if !caps.Vision {
		return unsupported("the selected model does not support image content")
	}
	return nil
}

func validateChatToolCalls(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return invalidRequest("tool_calls must be an array")
	}
	for _, call := range calls {
		if typ, ok := call["type"]; ok {
			var kind string
			if json.Unmarshal(typ, &kind) != nil || kind != "function" {
				return unsupported("only function tool calls are supported")
			}
		}
	}
	return nil
}

func validateAnthropic(obj map[string]json.RawMessage, caps Capabilities) error {
	if err := rejectUnknownKeys(obj, keySet(
		"model", "messages", "system", "tools", "tool_choice", "max_tokens",
		"temperature", "stream", "thinking",
	)); err != nil {
		return err
	}
	if err := checkDeclaredLimit(obj, caps, "max_tokens"); err != nil {
		return err
	}
	if raw, ok := obj["thinking"]; ok && present(&raw) {
		if err := validateThinking(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["tool_choice"]; ok && present(&raw) {
		var choice struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &choice); err != nil {
			return invalidRequest("tool_choice is malformed")
		}
		if choice.Type != "" && choice.Type != "auto" && choice.Type != "none" {
			return unsupported("forced tool_choice is not supported")
		}
	}
	if raw, ok := obj["system"]; ok && present(&raw) {
		if err := validateAnthropicSystem(raw, caps); err != nil {
			return err
		}
	}
	if raw, ok := obj["tools"]; ok && present(&raw) {
		if err := validateAnthropicTools(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["messages"]; ok && present(&raw) {
		return validateAnthropicMessages(raw, caps)
	}
	return invalidRequest("messages are required")
}

// validateThinking rejects a fixed thinking budget: the wire carries only a
// reasoning effort, so a token budget cannot be mapped losslessly.
func validateThinking(raw json.RawMessage) error {
	var thinking struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &thinking); err != nil {
		return invalidRequest("thinking must be an object")
	}
	switch thinking.Type {
	case "disabled", "":
		return nil
	default:
		return unsupported("fixed thinking budgets cannot be mapped to a reasoning effort")
	}
}

func validateAnthropicSystem(raw json.RawMessage, caps Capabilities) error {
	if raw[0] == '"' {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return invalidRequest("system must be text or text blocks")
	}
	for _, block := range blocks {
		var typ string
		if typRaw, ok := block["type"]; ok {
			_ = json.Unmarshal(typRaw, &typ)
		}
		if typ != "text" {
			return unsupported("system block type is not supported")
		}
	}
	return nil
}

func validateAnthropicTools(raw json.RawMessage) error {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalidRequest("tools must be an array")
	}
	for _, tool := range tools {
		if typ, ok := tool["type"]; ok {
			var kind string
			if json.Unmarshal(typ, &kind) == nil && kind != "" && kind != "custom" {
				return unsupported("only custom function tools are supported")
			}
		}
	}
	return nil
}

func validateAnthropicMessages(raw json.RawMessage, caps Capabilities) error {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return invalidRequest("messages must be an array")
	}
	for _, message := range messages {
		roleRaw, ok := message["role"]
		if !ok {
			return invalidRequest("message role is required")
		}
		var role string
		if err := json.Unmarshal(roleRaw, &role); err != nil {
			return invalidRequest("message role must be a string")
		}
		if role != "user" && role != "assistant" {
			return unsupported("message role is not supported")
		}
		content, ok := message["content"]
		if !ok {
			return invalidRequest("message content is required")
		}
		if err := validateAnthropicBlocks(content, role, caps); err != nil {
			return err
		}
	}
	return nil
}

func validateAnthropicBlocks(raw json.RawMessage, role string, caps Capabilities) error {
	if raw[0] == '"' {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return invalidRequest("message content must be text or blocks")
	}
	for _, block := range blocks {
		var typ string
		typRaw, ok := block["type"]
		if !ok {
			return unsupported("content block type is not supported")
		}
		if err := json.Unmarshal(typRaw, &typ); err != nil {
			return unsupported("content block type is not supported")
		}
		switch typ {
		case "text", "tool_use", "tool_result":
			if typ == "tool_result" {
				if err := validateToolResultBlock(block); err != nil {
					return err
				}
			}
		case "image":
			if role != "user" {
				return unsupported("only user messages may carry images")
			}
			if err := validateAnthropicImage(block["source"], caps); err != nil {
				return err
			}
		case "thinking":
			if role != "assistant" {
				return unsupported("thinking blocks are only valid on assistant messages")
			}
		case "redacted_thinking":
			return unsupported("redacted_thinking cannot be represented")
		default:
			return unsupported("content block type is not supported")
		}
	}
	return nil
}

func validateToolResultBlock(block map[string]json.RawMessage) error {
	content, ok := block["content"]
	if !ok || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return nil
	}
	if content[0] == '"' {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return invalidRequest("tool_result content is malformed")
	}
	for _, part := range parts {
		var typ string
		if typRaw, ok := part["type"]; ok {
			_ = json.Unmarshal(typRaw, &typ)
		}
		if typ != "text" {
			return unsupported("tool_result content type is not supported")
		}
	}
	return nil
}

func validateAnthropicImage(raw json.RawMessage, caps Capabilities) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalidRequest("image source is required")
	}
	var source struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return invalidRequest("image source is malformed")
	}
	if source.Type != "base64" {
		return unsupported("remote image URLs are not supported")
	}
	if !caps.Vision {
		return unsupported("the selected model does not support image content")
	}
	return nil
}

func validateResponses(obj map[string]json.RawMessage, caps Capabilities) error {
	if err := rejectUnknownKeys(obj, keySet(
		"model", "input", "instructions", "tools", "tool_choice",
		"max_output_tokens", "temperature", "stream", "reasoning", "text",
	)); err != nil {
		return err
	}
	if err := checkDeclaredLimit(obj, caps, "max_output_tokens"); err != nil {
		return err
	}
	if raw, ok := obj["reasoning"]; ok && present(&raw) {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &reasoning); err != nil {
			return invalidRequest("reasoning must be an object")
		}
		if reasoning.Effort != "" && reasoning.Effort != "none" && !containsString(caps.ReasoningEfforts, reasoning.Effort) {
			return &Error{Code: CodeReasoningEffort, Message: "reasoning effort is not supported by the selected model", HTTPStatus: 400}
		}
	}
	if raw, ok := obj["text"]; ok && present(&raw) {
		var payload struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return invalidRequest("text must be an object")
		}
		if payload.Format != nil && payload.Format.Type != "" && payload.Format.Type != "text" {
			return unsupported("only plain text output format is supported")
		}
	}
	if raw, ok := obj["tool_choice"]; ok && present(&raw) {
		if err := validateToolChoice(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["tools"]; ok && present(&raw) {
		if err := validateResponsesTools(raw); err != nil {
			return err
		}
	}
	if raw, ok := obj["input"]; ok && present(&raw) {
		return validateResponsesInput(raw, caps)
	}
	return invalidRequest("input is required")
}

func validateResponsesTools(raw json.RawMessage) error {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalidRequest("tools must be an array")
	}
	for _, tool := range tools {
		typ, ok := tool["type"]
		if !ok {
			return unsupported("only function tools are supported")
		}
		var kind string
		if err := json.Unmarshal(typ, &kind); err != nil || kind != "function" {
			return unsupported("only function tools are supported")
		}
	}
	return nil
}

func validateResponsesInput(raw json.RawMessage, caps Capabilities) error {
	if raw[0] == '"' {
		return nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return invalidRequest("input must be a string or an array of items")
	}
	for _, item := range items {
		var typ string
		if typRaw, ok := item["type"]; ok {
			_ = json.Unmarshal(typRaw, &typ)
		}
		switch typ {
		case "message", "":
			if content, ok := item["content"]; ok && present(&content) {
				if err := validateResponsesContent(content, caps); err != nil {
					return err
				}
			}
		case "function_call", "function_call_output":
		default:
			return unsupported("input item type is not supported")
		}
	}
	return nil
}

func validateResponsesContent(raw json.RawMessage, caps Capabilities) error {
	if raw[0] == '"' {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return invalidRequest("input content is malformed")
	}
	for _, part := range parts {
		var typ string
		if typRaw, ok := part["type"]; ok {
			_ = json.Unmarshal(typRaw, &typ)
		}
		switch typ {
		case "input_text", "output_text", "text":
		case "input_image":
			var value string
			if err := json.Unmarshal(part["image_url"], &value); err != nil || value == "" {
				return invalidRequest("input_image must contain an image_url string")
			}
			if err := validateDataImageURL(value, caps); err != nil {
				return err
			}
		default:
			return unsupported("input content type is not supported")
		}
	}
	return nil
}
