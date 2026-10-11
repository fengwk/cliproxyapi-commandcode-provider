package provider

import (
	"testing"

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
