package commandcode

import (
	"encoding/json"
	"math"
	"testing"
)

func mustNormalize(t *testing.T, raw string, supplement *int64) Usage {
	t.Helper()
	usage, err := normalizeFinishUsage(json.RawMessage(raw), supplement)
	if err != nil {
		t.Fatalf("normalizeFinishUsage(%s): %v", raw, err)
	}
	return usage
}

func TestNormalizeUsageComplete(t *testing.T) {
	usage := mustNormalize(t, `{
		"inputTokens":10,"outputTokens":4,"totalTokens":14,
		"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},
		"outputTokenDetails":{"reasoningTokens":2}
	}`, nil)
	if !usage.Complete {
		t.Fatalf("usage should be complete: %+v", usage)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 4 || usage.TotalTokens != 14 ||
		usage.CacheReadTokens != 5 || usage.CacheWriteTokens != 2 || usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

// An omitted total is derived only once every partition is self-consistent.
func TestNormalizeUsageDerivesTotal(t *testing.T) {
	usage := mustNormalize(t, `{
		"inputTokens":10,"outputTokens":4,
		"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},
		"outputTokenDetails":{"reasoningTokens":2}
	}`, nil)
	if usage.TotalTokens != 14 || !usage.Complete {
		t.Fatalf("usage = %+v", usage)
	}
}

// A single missing bucket is recovered from the inclusive total; the reviewer's
// worked examples must all succeed without fabricating an absent zero.
func TestNormalizeUsageDerivesSingleMissingBucket(t *testing.T) {
	cases := map[string]struct {
		raw           string
		wantRead      int64
		wantWrite     int64
		wantReasoning int64
	}{
		"I=10 uncached=3 read=5 write absent => write 2": {
			raw:           `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5},"outputTokenDetails":{"reasoningTokens":2}}`,
			wantRead:      5,
			wantWrite:     2,
			wantReasoning: 2,
		},
		"I=10 read=5 write=2 uncached absent => uncached 3": {
			raw:           `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"cacheReadTokens":5,"cacheWriteTokens":2},"outputTokenDetails":{"reasoningTokens":2}}`,
			wantRead:      5,
			wantWrite:     2,
			wantReasoning: 2,
		},
		"reasoning absent text explicit => reasoning 2": {
			raw:           `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},"outputTokenDetails":{"textTokens":2}}`,
			wantRead:      5,
			wantWrite:     2,
			wantReasoning: 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			usage := mustNormalize(t, tc.raw, nil)
			if usage.CacheReadTokens != tc.wantRead || usage.CacheWriteTokens != tc.wantWrite ||
				usage.ReasoningTokens != tc.wantReasoning || !usage.Complete {
				t.Fatalf("usage = %+v", usage)
			}
			if usage.CacheReadTokens+usage.CacheWriteTokens > usage.InputTokens {
				t.Fatalf("cache subsets exceed inclusive input: %+v", usage)
			}
		})
	}
}

func TestNormalizeUsageFailClosed(t *testing.T) {
	cases := map[string]struct {
		raw        string
		supplement *int64
		code       string
	}{
		"ambiguous cache partition": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"cacheReadTokens":5},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeAccounting,
		},
		"missing reasoning": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":8,"cacheReadTokens":2,"cacheWriteTokens":0},"outputTokenDetails":{}}`,
			code: CodeAccounting,
		},
		"no usage": {
			raw:  `null`,
			code: CodeAccounting,
		},
		"partitions conflict": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":2,"cacheWriteTokens":3},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeUsageConflict,
		},
		"total conflict": {
			raw:  `{"inputTokens":10,"outputTokens":4,"totalTokens":99,"inputTokenDetails":{"noCacheTokens":8,"cacheReadTokens":2,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeUsageConflict,
		},
		"reasoning over output": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":8,"cacheReadTokens":2,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":9}}`,
			code: CodeUsageConflict,
		},
		"output partitions conflict": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":8,"cacheReadTokens":2,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":2,"textTokens":3}}`,
			code: CodeUsageConflict,
		},
		"cache subsets exceed input": {
			raw:  `{"inputTokens":10,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":8,"cacheReadTokens":5,"cacheWriteTokens":5},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeUsageConflict,
		},
		"negative": {
			raw:  `{"inputTokens":-1,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeUsageInvalid,
		},
		"fractional": {
			raw:  `{"inputTokens":1.5,"outputTokens":4,"inputTokenDetails":{"noCacheTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}`,
			code: CodeUsageInvalid,
		},
		"conflicting supplement": {
			raw:        `{"inputTokens":3,"outputTokens":1,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":1,"cacheWriteTokens":1},"outputTokenDetails":{"reasoningTokens":0}}`,
			supplement: ptrInt64(7),
			code:       CodeUsageConflict,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeFinishUsage(json.RawMessage(tc.raw), tc.supplement)
			assertCode(t, err, tc.code)
		})
	}
}

// An absent cache-write bucket is filled once from a supplement, not treated as
// an explicit zero.
func TestNormalizeUsageSupplement(t *testing.T) {
	raw := `{"inputTokens":3,"outputTokens":1,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":1},"outputTokenDetails":{"reasoningTokens":0}}`
	usage := mustNormalize(t, raw, ptrInt64(1))
	if usage.CacheWriteTokens != 1 || !usage.Complete {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 2, TotalTokens: 14, Complete: true}
	b := Usage{InputTokens: 1, OutputTokens: 1, ReasoningTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, TotalTokens: 2, Complete: true}
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.InputTokens != 11 || sum.OutputTokens != 5 || sum.TotalTokens != 16 || !sum.Complete {
		t.Fatalf("sum = %+v", sum)
	}
}

func TestUsageAddOverflow(t *testing.T) {
	a := Usage{InputTokens: math.MaxInt64 - 1, OutputTokens: 0, TotalTokens: math.MaxInt64 - 1, Complete: true}
	b := Usage{InputTokens: 10, OutputTokens: 0, TotalTokens: 10, Complete: true}
	if _, err := a.Add(b); err == nil {
		t.Fatalf("expected overflow error")
	}
}

func TestUsageAddRejectsNegativeOperand(t *testing.T) {
	// Opposing negatives must not be able to cancel into a plausible sum.
	positive := Usage{InputTokens: 5, TotalTokens: 5, Complete: true}
	negative := Usage{InputTokens: -5, TotalTokens: -5, Complete: true}
	if _, err := positive.Add(negative); err == nil {
		t.Fatalf("expected rejection of a negative operand")
	}
	if _, err := negative.Add(positive); err == nil {
		t.Fatalf("expected rejection of a negative operand")
	}
}

func TestUsageValidatedRejectsOversizedCache(t *testing.T) {
	// cache read + cache write must stay within the inclusive input total; the
	// old per-bucket check would have accepted this.
	if (Usage{InputTokens: 10, CacheReadTokens: 6, CacheWriteTokens: 6, TotalTokens: 10}).validated() {
		t.Fatalf("validated must reject cache read+write exceeding input")
	}
	if (Usage{InputTokens: 10, CacheReadTokens: 5, CacheWriteTokens: 5, TotalTokens: 10}).validated() == false {
		t.Fatalf("cache read+write equal to input is valid")
	}
}

func TestUsageAddIncompletePropagates(t *testing.T) {
	a := Usage{InputTokens: 1, TotalTokens: 1, Complete: true}
	b := Usage{InputTokens: 1, TotalTokens: 1, Complete: false}
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.Complete {
		t.Fatalf("sum should not be complete when an operand was incomplete")
	}
}

func ptrInt64(v int64) *int64 { return &v }
