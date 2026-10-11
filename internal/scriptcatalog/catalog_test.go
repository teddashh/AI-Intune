package scriptcatalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistryHashIntegrity(t *testing.T) {
	if len(List()) != 1 {
		t.Fatal("phase one must register exactly one script")
	}
	for _, meta := range List() {
		e, ok := Lookup(meta.ID)
		if !ok || Digest(e.Bytes) != e.SHA256 {
			t.Fatal("embedded hash mismatch")
		}
		if e.Mode != "read" || e.RunsAs != "agent-user" || e.ArgsSchema.AdditionalProperties {
			t.Fatal("unsafe catalog metadata")
		}
	}
	e, _ := Lookup("fleet-probe-v1")
	e.Bytes[0] = 'x'
	unchanged, _ := Lookup(e.ID)
	if Digest(unchanged.Bytes) != unchanged.SHA256 {
		t.Fatal("registry is mutable")
	}
}
func TestArgsValidation(t *testing.T) {
	min, max := 1, 5
	e := Entry{ArgsSchema: Schema{Type: "object", Properties: map[string]Property{"mode": {Type: "string", Enum: []string{"facts"}, MaxLength: 8}, "label": {Type: "string", Pattern: "^[a-z]+$", MaxLength: 8}, "count": {Type: "integer", Minimum: &min, Maximum: &max}}}}
	for _, raw := range []string{`{"unknown":1}`, `{"mode":"bad"}`, `{"label":"toolonglabel"}`, `{"label":";$()"}`, `{"count":0}`, `{"count":6}`, `{"count":1.5}`, `{"mode":"facts","mode":"facts"}`, `null`, `[]`, `{} {}`, `{"label":null}`, strings.Repeat(" ", 4097)} {
		if _, err := e.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid args %s", raw)
		}
	}
	if _, err := e.Validate(json.RawMessage(`{"mode":"facts","label":"probe","count":3}`)); err != nil {
		t.Fatal(err)
	}
	probe, _ := Lookup("fleet-probe-v1")
	if _, err := probe.Validate(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}
