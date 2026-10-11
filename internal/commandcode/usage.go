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
	TextTokens      *json.RawMessage `json:"textTokens"`
}

type totalUsageWire struct {
	InputTokens        *json.RawMessage   `json:"inputTokens"`
	OutputTokens       *json.RawMessage   `json:"outputTokens"`
	TotalTokens        *json.RawMessage   `json:"totalTokens"`
	InputTokenDetails  *usageDetailsWire  `json:"inputTokenDetails"`
	OutputTokenDetails *outputDetailsWire `json:"outputTokenDetails"`
}

// normalizeFinishUsage turns one finish event's totalUsage into a canonical
// Usage. The inclusive input total and the explicitly present buckets are used
// to recover exactly one missing bucket; two or more missing buckets are
// ambiguous and fail closed. A cache-write supplement fills only an absent
// bucket and a conflicting supplement is rejected. Absent buckets are never
// silently treated as zero and reasoning is only derived when the remaining
// output bucket pins it uniquely.
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

	var (
		noCache, cacheRead, cacheWrite        int64
		haveNoCache, haveCacheRead, haveCache bool
	)
	if wire.InputTokenDetails != nil {
		details := wire.InputTokenDetails
		if present(details.NoCacheTokens) {
			if noCache, err = parseToken(*details.NoCacheTokens); err != nil {
				return Usage{}, err
			}
			haveNoCache = true
		}
		if present(details.CacheReadTokens) {
			if cacheRead, err = parseToken(*details.CacheReadTokens); err != nil {
				return Usage{}, err
			}
			haveCacheRead = true
		}
		if present(details.CacheWriteTokens) {
			if cacheWrite, err = parseToken(*details.CacheWriteTokens); err != nil {
				return Usage{}, err
			}
			haveCache = true
		}
	}
	if haveCache {
		if supplement != nil && *supplement != cacheWrite {
			return Usage{}, usageConflict("cache write tokens conflict with the stream supplements")
		}
	} else if supplement != nil {
		cacheWrite = *supplement
		haveCache = true
	}

	presentBuckets := make([]int64, 0, 3)
	missing := 0
	if haveNoCache {
		presentBuckets = append(presentBuckets, noCache)
	} else {
		missing++
	}
	if haveCacheRead {
		presentBuckets = append(presentBuckets, cacheRead)
	} else {
		missing++
	}
	if haveCache {
		presentBuckets = append(presentBuckets, cacheWrite)
	} else {
		missing++
	}

	sum, ok := addNonNegative(presentBuckets...)
	if !ok {
		return Usage{}, usageInvalid("usage input partitions overflow int64")
	}
	switch missing {
	case 0:
		if sum != inputTokens {
			return Usage{}, usageConflict("usage input partitions do not add up to the input total")
		}
	case 1:
		if sum > inputTokens {
			return Usage{}, usageConflict("usage input partitions exceed the input total")
		}
		derived := inputTokens - sum
		switch {
		case !haveNoCache:
			noCache = derived
		case !haveCacheRead:
			cacheRead = derived
		default:
			cacheWrite = derived
		}
	default:
		return Usage{}, accountingError("usage cache partition is ambiguous")
	}

	var (
		reasoning      int64
		haveReasoning  bool
		textTokens     int64
		haveOutputText bool
	)
	if wire.OutputTokenDetails != nil {
		details := wire.OutputTokenDetails
		if present(details.ReasoningTokens) {
			if reasoning, err = parseToken(*details.ReasoningTokens); err != nil {
				return Usage{}, err
			}
			haveReasoning = true
		}
		if present(details.TextTokens) {
			if textTokens, err = parseToken(*details.TextTokens); err != nil {
				return Usage{}, err
			}
			haveOutputText = true
		}
	}
	switch {
	case haveReasoning && haveOutputText:
		reasoningAndText, ok := addNonNegative(reasoning, textTokens)
		if !ok {
			return Usage{}, usageInvalid("usage output partitions overflow int64")
		}
		if reasoningAndText != outputTokens {
			return Usage{}, usageConflict("usage output partitions do not add up to the output total")
		}
	case haveReasoning:
		if reasoning > outputTokens {
			return Usage{}, usageConflict("usage reasoning tokens exceed the output total")
		}
	case haveOutputText:
		if textTokens > outputTokens {
			return Usage{}, usageConflict("usage output tokens exceed the output total")
		}
		reasoning = outputTokens - textTokens
	default:
		return Usage{}, accountingError("usage reasoning tokens are missing")
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

// validated reports whether a Usage is internally consistent: non-negative
// buckets, cache read and write within the inclusive input total, reasoning
// within output, and the total equal to input plus output without overflow.
func (u Usage) validated() bool {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.TotalTokens < 0 ||
		u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
		return false
	}
	if u.CacheReadTokens > u.InputTokens {
		return false
	}
	if u.CacheWriteTokens > u.InputTokens-u.CacheReadTokens {
		return false
	}
	if u.ReasoningTokens > u.OutputTokens {
		return false
	}
	total, ok := addInt64(u.InputTokens, u.OutputTokens)
	return ok && u.TotalTokens == total
}

// Add sums two per-turn usages, guarding int64 overflow. Both operands are
// validated first so opposing negatives cannot cancel into a plausible sum;
// each turn is added exactly once and Complete holds only while both sides were
// complete and the result stays self-consistent.
func (u Usage) Add(other Usage) (Usage, error) {
	if !u.validated() || !other.validated() {
		return Usage{}, usageInvalid("cannot add invalid usage")
	}
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
