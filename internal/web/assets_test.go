package web

import (
	"strings"
	"testing"
)

// TestAssetServesEmbeddedUIResources guards the plugin-host contract: the three
// browser assets stay embedded and keep their exact content types.
func TestAssetServesEmbeddedUIResources(t *testing.T) {
	cases := map[string]string{
		"ui.html": "text/html; charset=utf-8",
		"ui.js":   "text/javascript; charset=utf-8",
		"ui.css":  "text/css; charset=utf-8",
	}
	for name, wantType := range cases {
		body, contentType, ok := Asset(name)
		if !ok {
			t.Fatalf("Asset(%q) not found", name)
		}
		if contentType != wantType {
			t.Errorf("Asset(%q) contentType = %q, want %q", name, contentType, wantType)
		}
		if len(body) == 0 {
			t.Errorf("Asset(%q) returned empty content", name)
		}
	}
}

// TestAssetRejectsUnknownNames ensures only the three public resources resolve.
func TestAssetRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"", "nope", "../ui.html", "internal/web/ui.html", "ui.html.bak"} {
		if _, _, ok := Asset(name); ok {
			t.Errorf("Asset(%q) unexpectedly resolved", name)
		}
	}
}

// TestUIDocumentStaysCSPCompatible keeps the page free of inline script/style so it
// works under a script-src/style-src 'self' policy.
func TestUIDocumentStaysCSPCompatible(t *testing.T) {
	body, _, ok := Asset("ui.html")
	if !ok {
		t.Fatal("ui.html not found")
	}
	html := string(body)
	for _, forbidden := range []string{"<style", " onclick=", " onload=", " onerror=", "javascript:"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("ui.html must not contain %q", forbidden)
		}
	}
	for _, required := range []string{`href="./ui.css"`, `src="./ui.js" defer`} {
		if !strings.Contains(html, required) {
			t.Errorf("ui.html must contain %q", required)
		}
	}

	// Verify CommandCode branding and plugin ID
	if !strings.Contains(html, "CommandCode Provider") {
		t.Error("ui.html missing CommandCode Provider display name")
	}
	if !strings.Contains(html, "cliproxyapi-commandcode-provider") {
		t.Error("ui.html missing cliproxyapi-commandcode-provider plugin ID")
	}

	// Verify no OpenCode leftovers
	for _, forbidden := range []string{"opencode", "OpenCode", "cliproxyapi-opencode-provider"} {
		if strings.Contains(strings.ToLower(html), strings.ToLower(forbidden)) {
			t.Errorf("ui.html contains forbidden OpenCode reference %q", forbidden)
		}
	}
}

// TestAssetsFreeOfOpenCodeReferences ensures all embedded files are free of OpenCode leftovers.
func TestAssetsFreeOfOpenCodeReferences(t *testing.T) {
	files := []string{"ui.html", "ui.js", "ui.css"}
	forbiddenTerms := []string{"cliproxyapi-opencode-provider", "opencode-go"}
	for _, file := range files {
		body, _, ok := Asset(file)
		if !ok {
			t.Fatalf("Asset(%q) not found", file)
		}
		content := string(body)
		for _, term := range forbiddenTerms {
			if strings.Contains(content, term) {
				t.Errorf("%s contains forbidden term %q", file, term)
			}
		}
	}
}
