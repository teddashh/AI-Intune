package agentrelay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"
)

func TestDecodePageAcceptsOnlyThePageCommands(t *testing.T) {
	hello := base64.StdEncoding.EncodeToString([]byte("你好"))
	tests := []struct {
		name    string
		payload string
		want    PageCommand
		wantErr error
	}{
		{
			name:    "input",
			payload: `{"type":"input","data":"` + hello + `"}`,
			want:    PageCommand{Type: PageInput, Data: []byte("你好")},
		},
		{
			name:    "resize",
			payload: `{"type":"resize","cols":100,"rows":30}`,
			want:    PageCommand{Type: PageResize, Cols: 100, Rows: 30},
		},
		{
			name:    "other type",
			payload: `{"type":"open"}`,
			wantErr: ErrWrongDirection,
		},
		{
			name:    "missing data",
			payload: `{"type":"input"}`,
			wantErr: ErrInvalidData,
		},
		{
			name:    "missing cols",
			payload: `{"type":"resize","rows":24}`,
			wantErr: ErrInvalidGeometry,
		},
		{
			name:    "extra session key",
			payload: `{"type":"input","data":"` + hello + `","session":"sess"}`,
			wantErr: ErrFieldNotAllowed,
		},
		{
			name:    "duplicate data",
			payload: `{"type":"input","data":"` + hello + `","data":"` + hello + `"}`,
			wantErr: ErrDuplicateField,
		},
		{
			name:    "non-integer cols",
			payload: `{"type":"resize","cols":1.5,"rows":24}`,
			wantErr: ErrInvalidGeometry,
		},
		{
			name:    "out of range cols",
			payload: `{"type":"resize","cols":10001,"rows":24}`,
			wantErr: ErrInvalidGeometry,
		},
		{
			name:    "trailing data",
			payload: `{"type":"resize","cols":80,"rows":24}{}`,
			wantErr: ErrInvalidJSON,
		},
		{
			name:    "invalid utf-8",
			payload: `{"type":"input","data":"` + base64.StdEncoding.EncodeToString([]byte{0xff}) + `"}`,
			wantErr: ErrInvalidUTF8,
		},
		{
			name:    "non-canonical base64",
			payload: `{"type":"input","data":"YQ"}`,
			wantErr: ErrInvalidData,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodePage([]byte(test.payload))
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("DecodePage() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodePage() error = %v", err)
			}
			if got.Type != test.want.Type || got.Cols != test.want.Cols || got.Rows != test.want.Rows ||
				!bytes.Equal(got.Data, test.want.Data) {
				t.Fatalf("DecodePage() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestPageEncodersMatchThePageChecks(t *testing.T) {
	ready, err := EncodePageReady()
	if err != nil {
		t.Fatal(err)
	}
	assertPageReady(t, ready)

	output, err := EncodePageOutput([]byte("你好"))
	if err != nil {
		t.Fatal(err)
	}
	assertPageOutput(t, output, []byte("你好"))

	for _, code := range []int{0, 17, -1} {
		encoded, err := EncodePageExit(code)
		if err != nil {
			t.Fatal(err)
		}
		assertPageExit(t, encoded, code)
	}

	const reason = "這個終端已結束，未再接受輸入。請回到機器頁重新開啟。"
	encoded, err := EncodePageError(reason)
	if err != nil {
		t.Fatal(err)
	}
	assertPageError(t, encoded, reason)
}

func assertPageReady(t *testing.T, payload []byte) {
	t.Helper()
	msg := decodePageObject(t, payload)
	if len(msg) != 1 || msg["type"] != "ready" {
		t.Fatalf("ready frame = %#v", msg)
	}
}

func assertPageOutput(t *testing.T, payload, want []byte) {
	t.Helper()
	msg := decodePageObject(t, payload)
	if len(msg) != 2 || msg["type"] != "output" {
		t.Fatalf("output frame = %#v", msg)
	}
	data, ok := msg["data"].(string)
	if !ok {
		t.Fatalf("output data = %#v", msg["data"])
	}
	got, err := base64.StdEncoding.DecodeString(data)
	if err != nil || !bytes.Equal(got, want) || !utf8.Valid(got) {
		t.Fatalf("output bytes = %q, %v", got, err)
	}
}

func assertPageExit(t *testing.T, payload []byte, code int) {
	t.Helper()
	raw := decodePageRaw(t, payload)
	if len(raw) != 2 {
		t.Fatalf("exit keys = %#v", raw)
	}
	var typeName string
	if err := json.Unmarshal(raw["type"], &typeName); err != nil || typeName != "exit" {
		t.Fatalf("exit type = %s", raw["type"])
	}
	if !jsonIntegerToken(raw["code"]) {
		t.Fatalf("exit code token = %s", raw["code"])
	}
	var got int
	if err := json.Unmarshal(raw["code"], &got); err != nil || got != code {
		t.Fatalf("exit code = %s, want %d", raw["code"], code)
	}
}

func assertPageError(t *testing.T, payload []byte, reason string) {
	t.Helper()
	msg := decodePageObject(t, payload)
	if len(msg) != 2 || msg["type"] != "error" || msg["reason"] != reason {
		t.Fatalf("error frame = %#v", msg)
	}
}

func decodePageObject(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(payload))
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.More() {
		t.Fatal("trailing data")
	}
	return msg
}

func decodePageRaw(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	fields, err := decodeObject(payload)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func jsonIntegerToken(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	start := 0
	if raw[0] == '-' {
		start = 1
	}
	if start >= len(raw) {
		return false
	}
	for _, c := range raw[start:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
