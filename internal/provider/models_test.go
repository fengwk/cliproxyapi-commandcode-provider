package provider

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func testManager(mode Mode, manual ...ManualModel) *Manager {
	m := NewManager(nil)
	m.setConfig(Config{Mode: mode, BaseURL: DefaultBaseURL, ManualModels: manual})
	return m
}

func TestPublishedModelsCLIModeIntersection(t *testing.T) {
	m := testManager(ModeCLI)
	models := m.publishedModels(m.config())
	if len(models) != 87 {
		t.Fatalf("cli mode must publish the reviewed 87-model intersection, got %d", len(models))
	}
	byID := map[string]bool{}
	for _, model := range models {
		byID[model.ID] = true
	}
	if !byID["commandcode/claude-sonnet-5"] {
		t.Fatalf("expected reviewed vision model to be published")
	}
	// A CLI-only model that is absent from the public catalog must not leak.
	if byID["commandcode/minimax/minimax-m3-free"] {
		t.Fatalf("cli-only models must not be published by default")
	}
}

func TestPublishedModelsCapabilities(t *testing.T) {
	m := testManager(ModeCLI)
	models := m.publishedModels(m.config())
	for _, model := range models {
		if model.ID != "commandcode/claude-sonnet-5" {
			continue
		}
		if model.Thinking == nil || len(model.Thinking.Levels) == 0 {
			t.Fatalf("reviewed reasoning levels must be published")
		}
		hasImage := false
		for _, modality := range model.SupportedInputModalities {
			if modality == "image" {
				hasImage = true
			}
		}
		if !hasImage {
			t.Fatalf("reviewed vision modality must be published")
		}
		return
	}
	t.Fatalf("claude-sonnet-5 not found")
}

func TestPublishedModelsProviderEmbeddedStale(t *testing.T) {
	m := testManager(ModeProvider)
	models := m.publishedModels(m.config())
	if len(models) != 87 {
		t.Fatalf("provider mode must publish the embedded last-good snapshot, got %d", len(models))
	}
	info := m.catalogInfoForMode(m.config())
	if info.Source != "embedded" || !info.Stale {
		t.Fatalf("unrefreshed provider catalog must be shown stale: %+v", info)
	}
	if info.FetchedAt == "" {
		t.Fatalf("stale snapshot must carry its source date")
	}
}

func TestPublishedModelsWhitelistAndManual(t *testing.T) {
	m := NewManager(nil)
	m.setConfig(Config{Mode: ModeCLI, BaseURL: DefaultBaseURL, Models: []string{"claude-sonnet-5"}})
	models := m.publishedModels(m.config())
	if len(models) != 1 || models[0].ID != "commandcode/claude-sonnet-5" {
		t.Fatalf("whitelist must narrow the published catalog: %+v", models)
	}

	manual := testManager(ModeCLI, ManualModel{ID: "minimax/minimax-m3-free"})
	models = manual.publishedModels(manual.config())
	found := false
	for _, model := range models {
		if model.ID == "commandcode/minimax/minimax-m3-free" {
			found = true
			if !model.UserDefined {
				t.Fatalf("manual models must be marked user-defined")
			}
		}
	}
	if !found {
		t.Fatalf("manual model must be published even when it is cli-only")
	}
}

func TestResolveTargetFormat(t *testing.T) {
	claudeOnly := []string{"/messages"}
	chatOnly := []string{"/chat/completions"}
	chatAndResponses := []string{"/chat/completions", "/responses"}

	cases := []struct {
		name      string
		client    translator.Format
		endpoints []string
		stream    bool
		want      translator.Format
		wantOK    bool
	}{
		{"claude native", translator.FormatClaude, claudeOnly, true, translator.FormatClaude, true},
		{"openai to claude-only", translator.FormatOpenAI, claudeOnly, true, translator.FormatClaude, true},
		{"responses to claude-only", translator.FormatOpenAIResponse, claudeOnly, true, translator.FormatClaude, true},
		{"claude to chat-only", translator.FormatClaude, chatOnly, true, translator.FormatOpenAI, true},
		{"openai native chat", translator.FormatOpenAI, chatOnly, false, translator.FormatOpenAI, true},
		{"responses native responses", translator.FormatOpenAIResponse, chatAndResponses, true, translator.FormatOpenAIResponse, true},
		{"no declared endpoint", translator.FormatOpenAI, nil, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveTargetFormat(tc.client, tc.client, tc.endpoints, tc.stream)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("resolveTargetFormat(%s, %v) = (%q, %v), want (%q, %v)", tc.client, tc.endpoints, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLookupFactsProviderManual(t *testing.T) {
	m := testManager(ModeProvider, ManualModel{ID: "custom-chat", Protocol: "openai", ContextWindow: 4096})
	facts, ok := m.lookupFacts(m.config(), "custom-chat")
	if !ok {
		t.Fatalf("manual provider model must resolve")
	}
	if !endpointDeclared(facts.endpoints, translator.FormatOpenAI) || facts.contextWindow != 4096 {
		t.Fatalf("manual protocol/context must be applied: %+v", facts)
	}
	// A cross-provider model must not resolve.
	if _, ok := m.lookupFacts(m.config(), "gpt-4o"); ok {
		t.Fatalf("unknown models must fail closed")
	}
}

func TestFactsThinkingHonest(t *testing.T) {
	unknown := modelFacts{native: "x"}
	if unknown.thinkingSupport() != nil {
		t.Fatalf("unknown reasoning must not be invented")
	}
	reasoning := modelFacts{native: "x", reasoningSeen: true, reasoning: true}
	if reasoning.thinkingSupport() == nil {
		t.Fatalf("an observed reasoning capability should be published")
	}
	levelled := modelFacts{native: "x", efforts: []string{"low", "high"}}
	support := levelled.thinkingSupport()
	if support == nil || len(support.Levels) != 2 {
		t.Fatalf("observed effort levels must be published: %+v", support)
	}
}

func TestCatalogCountReflectsEffectiveModels(t *testing.T) {
	narrowed := NewManager(nil)
	narrowed.setConfig(Config{Mode: ModeCLI, BaseURL: DefaultBaseURL, Models: []string{"claude-sonnet-5"}})
	if info := narrowed.catalogInfoForMode(narrowed.config()); info.Count != 1 {
		t.Fatalf("the catalog count must reflect the whitelist, got %d", info.Count)
	}
	withManual := testManager(ModeCLI, ManualModel{ID: "minimax/minimax-m3-free"})
	if info := withManual.catalogInfoForMode(withManual.config()); info.Count != 88 {
		t.Fatalf("the catalog count must include manual models, got %d", info.Count)
	}
}

func TestRefreshCatalogFailureMarksStaleKeepsLastGood(t *testing.T) {
	host := &fakeHost{}
	healthy := true
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			if !healthy {
				return nil, errors.New("upstream down")
			}
			return httpOKResult(`{"data":[{"id":"commandcode/alpha","supported_endpoints":["/chat/completions"]}]}`, nil), nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})
	m.refreshCatalog(nil, "")
	if models, info := m.catalogSnapshot(m.config()); info.Source != "live" || info.Stale || len(models) != 1 {
		t.Fatalf("a successful refresh must yield a fresh live snapshot: %+v", info)
	}
	// A later failure must keep the last-good snapshot but flag it stale.
	healthy = false
	m.catalog.mu.Lock()
	m.catalog.lastAttempt = time.Time{}
	m.catalog.mu.Unlock()
	m.refreshCatalog(nil, "")
	models, info := m.catalogSnapshot(m.config())
	if info.Source != "live" || !info.Stale {
		t.Fatalf("a failed refresh must report the last-good snapshot stale: %+v", info)
	}
	if len(models) != 1 {
		t.Fatalf("a failed refresh must not clear the last-good snapshot: %+v", models)
	}
}

func TestCatalogSnapshotBoundToAuthority(t *testing.T) {
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			return httpOKResult(`{"data":[{"id":"commandcode/alpha","supported_endpoints":["/chat/completions"]}]}`, nil), nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})
	m.refreshCatalog(nil, "")
	if _, info := m.catalogSnapshot(m.config()); info.Source != "live" {
		t.Fatalf("expected a live snapshot for the first authority: %+v", info)
	}
	// Switching authority must not reuse the old snapshot's endpoints.
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:8"})
	if _, info := m.catalogSnapshot(m.config()); info.Source != "embedded" || !info.Stale {
		t.Fatalf("a snapshot bound to the old authority must not be reused: %+v", info)
	}
}

func TestInFlightRefreshDiscardedOnAuthorityChange(t *testing.T) {
	host := &fakeHost{}
	var m *Manager
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			// The authority changes while the refresh is in flight.
			m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:8"})
			return httpOKResult(`{"data":[{"id":"commandcode/alpha","supported_endpoints":["/chat/completions"]}]}`, nil), nil
		default:
			return nil, nil
		}
	}
	m = host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})
	m.refreshCatalog(nil, "")

	m.catalog.mu.Lock()
	live := m.catalog.live
	m.catalog.mu.Unlock()
	if live != nil {
		t.Fatalf("a refresh that spanned an authority change must be discarded")
	}
	if _, info := m.catalogSnapshot(m.config()); info.Source != "embedded" {
		t.Fatalf("the new authority must fall back to embedded: %+v", info)
	}
}

func TestNonChatCatalogEntriesNotPublishedOrRoutable(t *testing.T) {
	body := `{"data":[` +
		`{"id":"commandcode/system-only","supported_endpoints":["/system/decide"]},` +
		`{"id":"commandcode/chat-only","supported_endpoints":["/chat/completions"]}]}`
	m := newManagerWithHTTP(body, nil)
	m.refreshCatalog(nil, "")
	models := m.publishedModels(m.config())
	for _, model := range models {
		if strings.HasSuffix(model.ID, "system-only") {
			t.Fatalf("a non-chat endpoint must not be published: %+v", model)
		}
	}
	foundChat := false
	for _, model := range models {
		if strings.HasSuffix(model.ID, "chat-only") {
			foundChat = true
		}
	}
	if !foundChat {
		t.Fatalf("a chat-capable entry must be published: %+v", models)
	}
	if _, ok := m.lookupFacts(m.config(), "system-only"); ok {
		t.Fatalf("a non-chat entry must not be routable")
	}
}

func TestDecodePublicCatalogBounds(t *testing.T) {
	if _, err := decodePublicCatalog([]byte(`{"data":[{"id":"x","context_length":-1,"supported_endpoints":["/chat/completions"]}]}`)); err == nil {
		t.Fatalf("a negative context length must be rejected")
	}
	if _, err := decodePublicCatalog([]byte(`{"data":[{"id":"","supported_endpoints":["/chat/completions"]}]}`)); err == nil {
		t.Fatalf("an empty id must be rejected")
	}
	if _, err := decodePublicCatalog([]byte(`{"data":[{"id":"a b","supported_endpoints":["/chat/completions"]}]}`)); err == nil {
		t.Fatalf("an id with whitespace must be rejected")
	}
	entries := make([]string, 0, maxCatalogEntries+1)
	for i := 0; i <= maxCatalogEntries; i++ {
		entries = append(entries, `{"id":"m`+strconv.Itoa(i)+`","supported_endpoints":["/chat/completions"]}`)
	}
	if _, err := decodePublicCatalog([]byte(`{"data":[` + strings.Join(entries, ",") + `]}`)); err == nil {
		t.Fatalf("a catalog larger than %d entries must be rejected", maxCatalogEntries)
	}
	// Unknown capability fields are ignored, never guessed into reasoning facts.
	models, err := decodePublicCatalog([]byte(`{"data":[{"id":"commandcode/x","context_length":7,"reasoning":true,"unknown":"y","supported_endpoints":["/chat/completions"]}]}`))
	if err != nil || len(models) != 1 || models[0].ContextLength != 7 {
		t.Fatalf("unknown fields must be ignored: %v %+v", err, models)
	}
}
