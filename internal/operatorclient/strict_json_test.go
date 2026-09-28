package operatorclient

import (
	"strings"
	"testing"
)

func TestDecodeStrictJSONDocumentRejectsAmbiguousNestedResponses(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	type response struct {
		Items []item `json:"items"`
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate nested field", `{"items":[{"name":"a","name":"b"}]}`, "duplicate object field"},
		{"unknown nested field", `{"items":[{"name":"a","secret":"b"}]}`, "unknown field"},
		{"case alias", `{"Items":[]}`, "unknown field"},
		{"missing field", `{}`, "missing field"},
		{"null required scalar", `{"items":[{"name":null}]}`, "null is not allowed"},
		{"trailing document", `{"items":[]} {"items":[]}`, "trailing JSON"},
		{"non object", `[]`, "must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got response
			err := decodeStrictJSONDocument([]byte(tt.raw), "test", &got)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}

	var got response
	if err := decodeStrictJSONDocument([]byte(`{"items":[{"name":"a"}]}`), "test", &got); err != nil {
		t.Fatalf("valid response: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Name != "a" {
		t.Fatalf("decoded response=%+v", got)
	}
}
