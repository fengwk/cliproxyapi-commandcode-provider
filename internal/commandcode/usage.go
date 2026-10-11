package commandcode

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// jsonInt64 parses a JSON number only when it is an exact integer. Fractional
// or exponent forms are rejected so silent rounding cannot corrupt accounting.
func jsonInt64(raw json.RawMessage) (int64, bool) {
	s := string(bytes.TrimSpace(raw))
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', 'e', 'E':
			return 0, false
		}
	}
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func present(raw *json.RawMessage) bool {
	return raw != nil && !bytes.Equal(bytes.TrimSpace(*raw), []byte("null"))
}

func parseToken(raw json.RawMessage) (int64, error) {
	value, ok := jsonInt64(raw)
	if !ok {
		return 0, usageInvalid("usage token value must be a non-negative integer")
	}
	if value < 0 {
		return 0, usageInvalid("usage token value must not be negative")
	}
	return value, nil
}

type usageDetailsWire struct {
	NoCacheTokens    *json.RawMessage `json:"noCacheTokens"`
	CacheReadTokens  *json.RawMessage `json:"cacheReadTokens"`
	CacheWriteTokens *json.RawMessage `json:"cacheWriteTokens"`
}

type outputDetailsWire struct {
	ReasoningTokens *json.RawMessage `json:"reasoningTokens"`
}

type totalUsageWire struct {
	InputTokens        *json.RawMessage   `json:"inputTokens"`
	OutputTokens       *json.RawMessage   `json:"outputTokens"`
	TotalTokens        *json.RawMessage   `json:"totalTokens"`
	InputTokenDetails  *usageDetailsWire  `json:"inputTokenDetails"`
	OutputTokenDetails *outputDetailsWire `json:"outputTokenDetails"`
}

// normalizeFinishUsage turns one finish event's totalUsage into a canonical
// Usage. Missing cache-write buckets may be filled once from a supplement
// event; anything else missing, conflicting, negative, fractional or
// overflowing fails closed with an identifiable accounting error. An absent
// bucket is never treated as an explicit zero.
func normalizeFinishUsage(raw json.RawMessage, supplement *int64) (Usage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Usage{}, accountingError("finish event had no usage")
	}
	var wire totalUsageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Usage{}, usageInvalid("usage is not a valid object")
	}

	if !present(wire.InputTokens) {
		return Usage{}, accountingError("usage input tokens are missing")
	}
	inputTokens, err := parseToken(*wire.InputTokens)
	if err != nil {
		return Usage{}, err
	}
	if !present(wire.OutputTokens) {
		return Usage{}, accountingError("usage output tokens are missing")
	}
	outputTokens, err := parseToken(*wire.OutputTokens)
	if err != nil {
		return Usage{}, err
	}

	if wire.InputTokenDetails == nil {
		return Usage{}, accountingError("usage input token details are missing")
	}
	if !present(wire.InputTokenDetails.NoCacheTokens) {
		return Usage{}, accountingError("usage uncached input tokens are missing")
	}
	noCache, err := parseToken(*wire.InputTokenDetails.NoCacheTokens)
	if err != nil {
		return Usage{}, err
	}
	if !present(wire.InputTokenDetails.CacheReadTokens) {
		return Usage{}, accountingError("usage cache read tokens are missing")
	}
	cacheRead, err := parseToken(*wire.InputTokenDetails.CacheReadTokens)
	if err != nil {
		return Usage{}, err
	}

	var cacheWrite int64
	if present(wire.InputTokenDetails.CacheWriteTokens) {
		cacheWrite, err = parseToken(*wire.InputTokenDetails.CacheWriteTokens)
		if err != nil {
			return Usage{}, err
		}
		if supplement != nil && *supplement != cacheWrite {
			return Usage{}, usageConflict("cache write tokens conflict with the stream supplements")
		}
	} else if supplement != nil {
		cacheWrite = *supplement
	} else {
		return Usage{}, accountingError("usage cache write tokens are missing")
	}

	if wire.OutputTokenDetails == nil || !present(wire.OutputTokenDetails.ReasoningTokens) {
		return Usage{}, accountingError("usage reasoning tokens are missing")
	}
	reasoning, err := parseToken(*wire.OutputTokenDetails.ReasoningTokens)
	if err != nil {
		return Usage{}, err
	}

	sum, ok := addNonNegative(noCache, cacheRead, cacheWrite)
	if !ok {
		return Usage{}, usageInvalid("usage input partitions overflow int64")
	}
	if sum != inputTokens {
		return Usage{}, usageConflict("usage input partitions do not add up to the input total")
	}
	if reasoning > outputTokens {
		return Usage{}, usageConflict("usage reasoning tokens exceed the output total")
	}

	if inputTokens > math.MaxInt64-outputTokens {
		return Usage{}, usageInvalid("usage total overflows int64")
	}
	totalTokens := inputTokens + outputTokens
	if present(wire.TotalTokens) {
		explicit, err := parseToken(*wire.TotalTokens)
		if err != nil {
			return Usage{}, err
		}
		if explicit != totalTokens {
			return Usage{}, usageConflict("usage total does not match input plus output")
		}
		totalTokens = explicit
	}

	return Usage{
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoning,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		TotalTokens:      totalTokens,
		Complete:         true,
	}, nil
}

func addNonNegative(values ...int64) (int64, bool) {
	var sum int64
	for _, value := range values {
		if value < 0 {
			return 0, false
		}
		if sum > math.MaxInt64-value {
			return 0, false
		}
		sum += value
	}
	return sum, true
}

func addInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

// validated reports whether a Usage is internally consistent. Complete is only
// meaningful when the partitions agree with the inclusive totals.
func (u Usage) validated() bool {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.TotalTokens < 0 ||
		u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
		return false
	}
	if u.CacheReadTokens > u.InputTokens || u.CacheWriteTokens > u.InputTokens {
		return false
	}
	if u.ReasoningTokens > u.OutputTokens {
		return false
	}
	return u.TotalTokens == u.InputTokens+u.OutputTokens
}

// Add sums two per-turn usages, guarding int64 overflow. Each turn is added
// exactly once; Complete holds only while both sides were complete and the
// result stays self-consistent.
func (u Usage) Add(other Usage) (Usage, error) {
	sum := Usage{Complete: u.Complete && other.Complete}
	var ok bool
	if sum.InputTokens, ok = addInt64(u.InputTokens, other.InputTokens); !ok {
		return Usage{}, usageInvalid("usage input total overflows int64")
	}
	if sum.OutputTokens, ok = addInt64(u.OutputTokens, other.OutputTokens); !ok {
		return Usage{}, usageInvalid("usage output total overflows int64")
	}
	if sum.ReasoningTokens, ok = addInt64(u.ReasoningTokens, other.ReasoningTokens); !ok {
		return Usage{}, usageInvalid("usage reasoning total overflows int64")
	}
	if sum.CacheReadTokens, ok = addInt64(u.CacheReadTokens, other.CacheReadTokens); !ok {
		return Usage{}, usageInvalid("usage cache read total overflows int64")
	}
	if sum.CacheWriteTokens, ok = addInt64(u.CacheWriteTokens, other.CacheWriteTokens); !ok {
		return Usage{}, usageInvalid("usage cache write total overflows int64")
	}
	if sum.TotalTokens, ok = addInt64(u.TotalTokens, other.TotalTokens); !ok {
		return Usage{}, usageInvalid("usage total overflows int64")
	}
	if !sum.validated() {
		return Usage{}, usageConflict("accumulated usage is not self-consistent")
	}
	return sum, nil
}
