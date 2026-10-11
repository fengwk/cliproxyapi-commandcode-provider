package commandcode

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"
)

// Response accumulation bounds keep one logical request (up to six continued
// turns) within a fixed memory budget and reject pathological upstream output
// with a typed error instead of growing without limit.
const (
	maxResponseContentBytes = 16 << 20 // total text + reasoning + tool arguments
	maxResponseToolCalls    = 128
	maxToolArgumentsBytes   = 1 << 20
)

type chatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type chatToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          *string        `json:"content,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type chatUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   chatUsage              `json:"usage"`
}

type plannedToolCall struct {
	id        string
	name      string
	arguments string
}

// Response accumulates one logical request into canonical OpenAI Chat output.
// It is per-request state only and never caches messages or tool schemas across
// requests. Apply returns whole SSE frames; Completion serializes the same
// content and usage for the non-streaming path.
type Response struct {
	model       string
	id          string
	created     int64
	roleEmitted bool

	text      strings.Builder
	reasoning strings.Builder
	toolCalls []plannedToolCall

	usage        Usage
	hasUsage     bool
	finished     bool
	finishCause  string
	contentBytes int
}

// NewResponse starts an empty canonical Chat response for the public model id.
// The id is random per request and carries no credential or session material.
func NewResponse(model string) *Response {
	return &Response{
		model:   model,
		id:      "chatcmpl-" + rand.Text(),
		created: time.Now().Unix(),
	}
}

// Apply folds one upstream event into the response and returns the SSE frames
// (always complete "data: ...\n\n" frames) it produces. A pause_turn finish
// only accumulates usage and emits nothing so the host can continue the same
// logical request; a normal finish emits the terminal reason, one usage frame
// and [DONE].
func (r *Response) Apply(event Event) ([][]byte, error) {
	if r.finished {
		return nil, responseStateError("response has already completed")
	}
	switch event.Type {
	case "text":
		if err := r.reserveContent(len(event.Text)); err != nil {
			return nil, err
		}
		r.text.WriteString(event.Text)
		content := event.Text
		return r.frame(chatDelta{Content: &content}, nil)
	case "reasoning-delta":
		if err := r.reserveContent(len(event.Text)); err != nil {
			return nil, err
		}
		r.reasoning.WriteString(event.Text)
		content := event.Text
		return r.frame(chatDelta{ReasoningContent: &content}, nil)
	case "reasoning-start", "reasoning-end":
		return nil, nil
	case "tool-call":
		if len(r.toolCalls) >= maxResponseToolCalls {
			return nil, responseTooLargeError()
		}
		arguments := string(event.ToolInput)
		if len(arguments) > maxToolArgumentsBytes {
			return nil, responseTooLargeError()
		}
		if err := r.reserveContent(len(arguments)); err != nil {
			return nil, err
		}
		index := len(r.toolCalls)
		r.toolCalls = append(r.toolCalls, plannedToolCall{
			id:        event.ToolID,
			name:      event.ToolName,
			arguments: arguments,
		})
		call := chatToolCall{
			Index: index,
			ID:    event.ToolID,
			Type:  "function",
			Function: chatFunction{
				Name:      event.ToolName,
				Arguments: arguments,
			},
		}
		return r.frame(chatDelta{ToolCalls: []chatToolCall{call}}, nil)
	case "finish":
		return r.applyFinish(event)
	default:
		return nil, nil
	}
}

func (r *Response) reserveContent(n int) error {
	if n < 0 || r.contentBytes > maxResponseContentBytes-n {
		return responseTooLargeError()
	}
	r.contentBytes += n
	return nil
}

func (r *Response) applyFinish(event Event) ([][]byte, error) {
	if event.Usage == nil || !event.Usage.Complete || !event.Usage.validated() {
		return nil, accountingError("finish usage is not complete")
	}
	if !r.hasUsage {
		r.usage = *event.Usage
		r.hasUsage = true
	} else {
		merged, err := r.usage.Add(*event.Usage)
		if err != nil {
			return nil, err
		}
		r.usage = merged
	}

	if event.RawFinishReason == "pause_turn" || event.FinishReason == "pause_turn" {
		return nil, nil
	}
	if event.FinishReason == "" {
		return nil, terminalUnknownError()
	}

	r.finished = true
	r.finishCause = event.FinishReason

	reason := r.finishCause
	finishFrame, err := sseFrame(chatChunk{
		ID:      r.id,
		Object:  "chat.completion.chunk",
		Created: r.created,
		Model:   r.model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatDelta{}, FinishReason: &reason}},
	})
	if err != nil {
		return nil, responseStateError("failed to serialize finish frame")
	}
	usage := r.chatUsage()
	usageFrame, err := sseFrame(chatChunk{
		ID:      r.id,
		Object:  "chat.completion.chunk",
		Created: r.created,
		Model:   r.model,
		Choices: []chatChunkChoice{},
		Usage:   &usage,
	})
	if err != nil {
		return nil, responseStateError("failed to serialize usage frame")
	}
	return [][]byte{finishFrame, usageFrame, doneFrame()}, nil
}

func (r *Response) frame(delta chatDelta, finish *string) ([][]byte, error) {
	if !r.roleEmitted && (delta.Content != nil || delta.ReasoningContent != nil || len(delta.ToolCalls) > 0) {
		delta.Role = "assistant"
		r.roleEmitted = true
	}
	frame, err := sseFrame(chatChunk{
		ID:      r.id,
		Object:  "chat.completion.chunk",
		Created: r.created,
		Model:   r.model,
		Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	})
	if err != nil {
		return nil, responseStateError("failed to serialize stream frame")
	}
	return [][]byte{frame}, nil
}

// Completion returns the canonical non-streaming chat.completion JSON. It may
// only be called after a real finish; it carries the same accumulated content
// and usage as the streamed frames.
func (r *Response) Completion() ([]byte, error) {
	if !r.finished {
		return nil, responseStateError("response is not complete")
	}
	message := chatMessage{
		Role:             "assistant",
		Content:          r.text.String(),
		ReasoningContent: r.reasoning.String(),
	}
	for _, call := range r.toolCalls {
		message.ToolCalls = append(message.ToolCalls, chatToolCall{
			ID:   call.id,
			Type: "function",
			Function: chatFunction{
				Name:      call.name,
				Arguments: call.arguments,
			},
		})
	}
	out, err := json.Marshal(chatCompletion{
		ID:      r.id,
		Object:  "chat.completion",
		Created: r.created,
		Model:   r.model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: r.finishCause,
		}},
		Usage: r.chatUsage(),
	})
	if err != nil {
		return nil, responseStateError("failed to serialize completion")
	}
	return out, nil
}

func (r *Response) chatUsage() chatUsage {
	return chatUsage{
		PromptTokens:     r.usage.InputTokens,
		CompletionTokens: r.usage.OutputTokens,
		TotalTokens:      r.usage.TotalTokens,
		PromptTokensDetails: &promptTokensDetails{
			CachedTokens:     r.usage.CacheReadTokens,
			CacheWriteTokens: r.usage.CacheWriteTokens,
		},
		CompletionTokensDetails: &completionTokensDetails{
			ReasoningTokens: r.usage.ReasoningTokens,
		},
	}
}

func sseFrame(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(raw)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, raw...)
	frame = append(frame, '\n', '\n')
	return frame, nil
}

func doneFrame() []byte {
	return []byte("data: [DONE]\n\n")
}
