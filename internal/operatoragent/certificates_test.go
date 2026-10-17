package operatoragent

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeCertificateHub struct {
	fakeHub
	reads int
	id    string
}

func (h *fakeCertificateHub) CertificateRead(_ context.Context, id string) (any, error) {
	h.reads++
	h.id = id
	return []any{}, nil
}
func TestCertificateTools(t *testing.T) {
	h := &fakeCertificateHub{}
	s := &Service{Hub: h}
	for _, tool := range certificateTools() {
		if tool.InputSchema["additionalProperties"] != false || !tool.Annotations.ReadOnlyHint {
			t.Fatal(tool)
		}
	}
	for _, tc := range []struct{ name, body string }{{"certificate_list", `{}`}, {"certificate_get", `{"id":"monitor-1"}`}} {
		if _, err := s.Call(context.Background(), tc.name, json.RawMessage(tc.body)); err != nil {
			t.Fatal(err)
		}
	}
	if h.reads != 2 || h.id != "monitor-1" {
		t.Fatal(h.reads, h.id)
	}
	for _, tc := range []struct{ name, body string }{{"certificate_list", `{"id":"monitor-1"}`}, {"certificate_get", `{"id":"../monitor"}`}, {"certificate_get", `{}`}} {
		if _, err := s.Call(context.Background(), tc.name, json.RawMessage(tc.body)); err == nil {
			t.Fatal("invalid call accepted")
		}
	}
	if h.reads != 2 {
		t.Fatal("invalid call reached Hub")
	}
}
