package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestRegistrationResponseSchemaNegotiation(t *testing.T) {
	if _, err := registrationResponse(5); err == nil {
		t.Fatalf("an older non-zero schema must be rejected")
	}
	if _, err := registrationResponse(1); err == nil {
		t.Fatalf("a legacy schema must be rejected")
	}
	reg, err := registrationResponse(pluginabi.SchemaVersion)
	if err != nil || reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("the current schema must be accepted: %v %d", err, reg.SchemaVersion)
	}
	// A zero schema is a metadata-only probe and is answered with the native schema.
	probe, err := registrationResponse(0)
	if err != nil || probe.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("a zero-schema metadata probe must be accepted: %v %d", err, probe.SchemaVersion)
	}
	// A newer host schema is capped at the SDK schema, never exceeded.
	newer, err := registrationResponse(pluginabi.SchemaVersion + 1)
	if err != nil || newer.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("a newer host schema must be capped: %v %d", err, newer.SchemaVersion)
	}
}

func TestHandleLifecycleRejectsLegacySchema(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, []byte(`{"schema_version":4}`))
	if err != nil {
		t.Fatalf("dispatch must not return a transport error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("a legacy schema must be rejected: %+v", env)
	}
	raw, err = m.HandleCall(pluginabi.MethodPluginRegister, []byte(`{"schema_version":6}`))
	if err != nil {
		t.Fatalf("dispatch must not return a transport error: %v", err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("the current schema must register: %+v", env.Error)
	}
}

func TestMetadataBaseURLDescribesBaseURL(t *testing.T) {
	for _, field := range Metadata().ConfigFields {
		if field.Name != "base-url" {
			continue
		}
		if !strings.Contains(field.Description, "API base URL") || strings.Contains(field.Description, "authority") {
			t.Fatalf("base-url must be described as an API base URL: %q", field.Description)
		}
		return
	}
	t.Fatalf("base-url field missing")
}
