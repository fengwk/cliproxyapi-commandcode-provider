// Package commandcode implements the versioned generate wire protocol.
package commandcode

import (
	"encoding/json"
	"time"
)

// Capabilities are reviewed model facts, not inferred from its name.
type Capabilities struct {
	Vision           bool
	ReasoningEfforts []string
	MaxOutputTokens  int64
}

// RequestOptions bind a canonical Anthropic-shaped input to one upstream call.
type RequestOptions struct {
	Model           string
	ThreadID        string
	Now             time.Time
	Capabilities    Capabilities
	ReasoningEffort string
}

// Usage keeps cache buckets separate while input/output totals are inclusive.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	Complete         bool
}

// Event is one validated upstream event; tool inputs retain their JSON shape.
type Event struct {
	Type            string
	Text            string
	ToolID          string
	ToolName        string
	ToolInput       json.RawMessage
	FinishReason    string
	RawFinishReason string
	Usage           *Usage
}

// Error never contains upstream payload or credential material.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *Error) Error() string { return e.Message }

func (e *Error) StatusCode() int { return e.HTTPStatus }
