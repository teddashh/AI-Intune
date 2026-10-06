package agentrelay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/sessionid"
)

func TestSessionLengthMatchesSharedGrammarAndRejects129Bytes(t *testing.T) {
	if MaxSessionLength != sessionid.MaxLength {
		t.Fatalf("MaxSessionLength = %d, sessionid.MaxLength = %d", MaxSessionLength, sessionid.MaxLength)
	}

	session := strings.Repeat("s", sessionid.MaxLength+1)
	tests := []struct {
		name string
		fn   func() error
	}{
		{name: "encode downstream", fn: func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamClose, Session: session})
			return err
		}},
		{name: "decode downstream", fn: func() error {
			_, err := DecodeDownstream([]byte(`{"type":"close","session":` + quoted(session) + `}`))
			return err
		}},
		{name: "encode upstream", fn: func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamReady, Session: session})
			return err
		}},
		{name: "decode upstream", fn: func() error {
			_, err := DecodeUpstream([]byte(`{"type":"ready","session":` + quoted(session) + `}`))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); !errors.Is(err, ErrInvalidSession) {
				t.Fatalf("error = %v, want ErrInvalidSession", err)
			}
		})
	}
}

func TestRoundTripAllDownstreamTypes(t *testing.T) {
	frames := []Downstream{
		{Type: DownstreamOpen, Session: "open-session", CWD: "/srv/work", Cols: 120, Rows: 40},
		{Type: DownstreamInput, Session: "input-session", Data: []byte("hello\x00terminal")},
		{Type: DownstreamResize, Session: "resize-session", Cols: 200, Rows: 60},
		{Type: DownstreamClose, Session: "close-session"},
	}
	for _, frame := range frames {
		frame := frame
		t.Run(string(frame.Type), func(t *testing.T) {
			encoded, err := EncodeDownstream(frame)
			if err != nil {
				t.Fatalf("EncodeDownstream() error = %v", err)
			}
			decoded, err := DecodeDownstream(encoded)
			if err != nil {
				t.Fatalf("DecodeDownstream() error = %v", err)
			}
			if !reflect.DeepEqual(decoded, frame) {
				t.Fatalf("round trip mismatch: got %#v, want %#v", decoded, frame)
			}
		})
	}
}

func TestRoundTripAllUpstreamTypes(t *testing.T) {
	code := 17
	frames := []Upstream{
		{Type: UpstreamReady, Session: "ready-session"},
		{Type: UpstreamOutput, Session: "output-session", Data: []byte("hello\x00terminal")},
		{Type: UpstreamExit, Session: "exit-session", Code: &code},
		{Type: UpstreamError, Session: "error-session", Reason: "process unavailable"},
	}
	for _, frame := range frames {
		frame := frame
		t.Run(string(frame.Type), func(t *testing.T) {
			encoded, err := EncodeUpstream(frame)
			if err != nil {
				t.Fatalf("EncodeUpstream() error = %v", err)
			}
			decoded, err := DecodeUpstream(encoded)
			if err != nil {
				t.Fatalf("DecodeUpstream() error = %v", err)
			}
			if !reflect.DeepEqual(decoded, frame) {
				t.Fatalf("round trip mismatch: got %#v, want %#v", decoded, frame)
			}
		})
	}
}

func TestDecodeRejectsWrongDirection(t *testing.T) {
	tests := []struct {
		name string
		fn   func() error
	}{
		{"output downstream", func() error {
			_, err := DecodeDownstream([]byte(`{"type":"output","session":"s","data":"YQ=="}`))
			return err
		}},
		{"input upstream", func() error {
			_, err := DecodeUpstream([]byte(`{"type":"input","session":"s","data":"YQ=="}`))
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.fn(); !errors.Is(err, ErrWrongDirection) {
				t.Fatalf("error = %v, want ErrWrongDirection", err)
			}
		})
	}
}

func TestDecodeRejectsUnknownType(t *testing.T) {
	for _, frameType := range []string{"mystery", ""} {
		payload := []byte(`{"type":"` + frameType + `","session":"s"}`)
		if _, err := DecodeDownstream(payload); !errors.Is(err, ErrUnknownType) {
			t.Errorf("DecodeDownstream(type %q) error = %v, want ErrUnknownType", frameType, err)
		}
		if _, err := DecodeUpstream(payload); !errors.Is(err, ErrUnknownType) {
			t.Errorf("DecodeUpstream(type %q) error = %v, want ErrUnknownType", frameType, err)
		}
	}
}

func TestDecodeRejectsBadSession(t *testing.T) {
	tests := []struct {
		name    string
		session string
	}{
		{"empty", ""},
		{"newline", "bad\nsession"},
		{"control", "bad\u0001session"},
		{"too long", strings.Repeat("s", MaxSessionLength+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"close","session":` + quoted(test.session) + `}`)
			if _, err := DecodeDownstream(payload); !errors.Is(err, ErrInvalidSession) {
				t.Fatalf("error = %v, want ErrInvalidSession", err)
			}
		})
	}
}

func TestDecodeRejectsBadData(t *testing.T) {
	if _, err := DecodeDownstream([]byte(`{"type":"input","session":"s","data":"not-base64"}`)); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("invalid base64 error = %v, want ErrInvalidData", err)
	}

	tooLarge := make([]byte, MaxDataSize+1)
	payload := []byte(`{"type":"output","session":"s","data":"` + base64.StdEncoding.EncodeToString(tooLarge) + `"}`)
	if _, err := DecodeUpstream(payload); !errors.Is(err, ErrDataTooLarge) {
		t.Fatalf("oversize data error = %v, want ErrDataTooLarge", err)
	}
}

func TestDecodeRejectsBadGeometry(t *testing.T) {
	values := []int{0, -1, MaxGeometry + 1}
	for _, field := range []string{"cols", "rows"} {
		for _, value := range values {
			cols, rows := 80, 24
			if field == "cols" {
				cols = value
			} else {
				rows = value
			}
			payload := []byte(`{"type":"resize","session":"s","cols":` + integer(cols) + `,"rows":` + integer(rows) + `}`)
			if _, err := DecodeDownstream(payload); !errors.Is(err, ErrInvalidGeometry) {
				t.Errorf("%s=%d error = %v, want ErrInvalidGeometry", field, value, err)
			}
		}
	}
}

func TestDecodeRejectsFieldsNotAllowedForType(t *testing.T) {
	tests := []struct {
		name     string
		upstream bool
		payload  string
	}{
		{"resize with data", false, `{"type":"resize","session":"s","cols":80,"rows":24,"data":"YQ=="}`},
		{"close with data", false, `{"type":"close","session":"s","data":"YQ=="}`},
		{"ready with data", true, `{"type":"ready","session":"s","data":"YQ=="}`},
		{"input with cols", false, `{"type":"input","session":"s","data":"YQ==","cols":80}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.upstream {
				_, err = DecodeUpstream([]byte(test.payload))
			} else {
				_, err = DecodeDownstream([]byte(test.payload))
			}
			if !errors.Is(err, ErrFieldNotAllowed) {
				t.Fatalf("error = %v, want ErrFieldNotAllowed", err)
			}
		})
	}
}

func TestReasonTruncationLeavesTrace(t *testing.T) {
	reason := strings.Repeat("界", MaxReasonLength)
	payload := []byte(`{"type":"error","session":"s","reason":` + quoted(reason) + `}`)
	frame, err := DecodeUpstream(payload)
	if err != nil {
		t.Fatalf("DecodeUpstream() error = %v", err)
	}
	if len(frame.Reason) > MaxReasonLength {
		t.Fatalf("truncated reason length = %d, maximum = %d", len(frame.Reason), MaxReasonLength)
	}
	if !strings.HasSuffix(frame.Reason, ReasonTruncationMarker) {
		t.Fatalf("truncated reason lacks marker: %q", frame.Reason)
	}
}

func TestReasonRejectsControlCharacters(t *testing.T) {
	reasons := []string{
		"\x1b[2J\x1b[H wiped",
		"nul\x00byte",
		"bell\a",
		"back\bspace",
	}
	for _, reason := range reasons {
		t.Run(quoted(reason), func(t *testing.T) {
			payload := []byte(`{"type":"error","session":"s","reason":` + quoted(reason) + `}`)
			if _, err := DecodeUpstream(payload); !errors.Is(err, ErrInvalidReason) {
				t.Errorf("DecodeUpstream() error = %v, want ErrInvalidReason", err)
			}
			if _, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s", Reason: reason}); !errors.Is(err, ErrInvalidReason) {
				t.Errorf("EncodeUpstream() error = %v, want ErrInvalidReason", err)
			}
		})
	}
}

func TestSessionAndReasonShareControlRule(t *testing.T) {
	const controlled = "shared\x1bvalue"
	if _, err := DecodeUpstream([]byte(`{"type":"ready","session":` + quoted(controlled) + `}`)); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("controlled session error = %v, want ErrInvalidSession", err)
	}
	if _, err := DecodeUpstream([]byte(`{"type":"error","session":"s","reason":` + quoted(controlled) + `}`)); !errors.Is(err, ErrInvalidReason) {
		t.Errorf("controlled reason error = %v, want ErrInvalidReason", err)
	}
	if _, err := EncodeUpstream(Upstream{Type: UpstreamReady, Session: controlled}); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("encoded controlled session error = %v, want ErrInvalidSession", err)
	}
	if _, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s", Reason: controlled}); !errors.Is(err, ErrInvalidReason) {
		t.Errorf("encoded controlled reason error = %v, want ErrInvalidReason", err)
	}
}

func TestReasonRejectsTabIntentionally(t *testing.T) {
	// Error reasons are single-line operator messages, so tabs are control characters rather than formatting.
	const reason = "tab\tseparated"
	payload := []byte(`{"type":"error","session":"s","reason":` + quoted(reason) + `}`)
	if _, err := DecodeUpstream(payload); !errors.Is(err, ErrInvalidReason) {
		t.Errorf("DecodeUpstream() error = %v, want ErrInvalidReason", err)
	}
	if _, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s", Reason: reason}); !errors.Is(err, ErrInvalidReason) {
		t.Errorf("EncodeUpstream() error = %v, want ErrInvalidReason", err)
	}
}

func TestCWDLengthLimit(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want error
	}{
		{"at limit", strings.Repeat("c", MaxCWDLength), nil},
		{"over limit", strings.Repeat("c", MaxCWDLength+1), ErrCWDTooLong},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"open","session":"s","cwd":` + quoted(test.cwd) + `,"cols":80,"rows":24}`)
			if _, err := DecodeDownstream(payload); !errors.Is(err, test.want) {
				t.Errorf("DecodeDownstream() error = %v, want %v", err, test.want)
			}
			frame := Downstream{Type: DownstreamOpen, Session: "s", CWD: test.cwd, Cols: 80, Rows: 24}
			if _, err := EncodeDownstream(frame); !errors.Is(err, test.want) {
				t.Errorf("EncodeDownstream() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCWDRejectsControlCharacters(t *testing.T) {
	for _, cwd := range []string{"bad\npath", "bad\x1bpath"} {
		t.Run(quoted(cwd), func(t *testing.T) {
			payload := []byte(`{"type":"open","session":"s","cwd":` + quoted(cwd) + `,"cols":80,"rows":24}`)
			if _, err := DecodeDownstream(payload); !errors.Is(err, ErrInvalidFieldValue) {
				t.Errorf("DecodeDownstream() error = %v, want ErrInvalidFieldValue", err)
			}
			frame := Downstream{Type: DownstreamOpen, Session: "s", CWD: cwd, Cols: 80, Rows: 24}
			if _, err := EncodeDownstream(frame); !errors.Is(err, ErrInvalidFieldValue) {
				t.Errorf("EncodeDownstream() error = %v, want ErrInvalidFieldValue", err)
			}
		})
	}
}

func TestCWDEmptyStillValid(t *testing.T) {
	frame, err := DecodeDownstream([]byte(`{"type":"open","session":"s","cwd":"","cols":80,"rows":24}`))
	if err != nil {
		t.Fatalf("DecodeDownstream() error = %v", err)
	}
	if frame.CWD != "" {
		t.Fatalf("decoded cwd = %q, want empty", frame.CWD)
	}

	encoded, err := EncodeDownstream(Downstream{Type: DownstreamOpen, Session: "s", CWD: "", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("EncodeDownstream() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, present := fields["cwd"]; present {
		t.Fatalf("encoded frame contains omitted cwd: %s", encoded)
	}
}

func TestExitCodeInt32Bound(t *testing.T) {
	tests := []struct {
		name string
		text string
		code int
		want error
	}{
		{"maximum", "2147483647", 2147483647, nil},
		{"minimum", "-2147483648", -2147483648, nil},
		{"above maximum", "2147483648", 2147483648, ErrInvalidFieldValue},
		{"below minimum", "-2147483649", -2147483649, ErrInvalidFieldValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"exit","session":"s","code":` + test.text + `}`)
			if _, err := DecodeUpstream(payload); !errors.Is(err, test.want) {
				t.Errorf("DecodeUpstream() error = %v, want %v", err, test.want)
			}
			if _, err := EncodeUpstream(Upstream{Type: UpstreamExit, Session: "s", Code: &test.code}); !errors.Is(err, test.want) {
				t.Errorf("EncodeUpstream() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOverlongReasonDecodesWithoutError(t *testing.T) {
	reason := "original-prefix-" + strings.Repeat("r", MaxReasonLength)
	payload := []byte(`{"type":"error","session":"s","reason":` + quoted(reason) + `}`)
	frame, err := DecodeUpstream(payload)
	if err != nil {
		t.Fatalf("DecodeUpstream() error = %v", err)
	}
	if len(frame.Reason) > MaxReasonLength {
		t.Fatalf("truncated reason length = %d, maximum = %d", len(frame.Reason), MaxReasonLength)
	}
	if !strings.HasSuffix(frame.Reason, ReasonTruncationMarker) {
		t.Fatalf("truncated reason lacks marker: %q", frame.Reason)
	}
	if !strings.HasPrefix(frame.Reason, "original-prefix-") {
		t.Fatalf("truncated reason lost original prefix: %q", frame.Reason)
	}
}

func TestTruncationLandsOnRuneBoundary(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{"three-byte runes", strings.Repeat("界", MaxReasonLength)},
		{"four-byte runes", strings.Repeat("😀", MaxReasonLength)},
		{"boundary inside rune", strings.Repeat("a", 487) + "界" + strings.Repeat("z", MaxReasonLength)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"type":"error","session":"s","reason":` + quoted(test.reason) + `}`)
			frame, err := DecodeUpstream(payload)
			if err != nil {
				t.Fatalf("DecodeUpstream() error = %v", err)
			}
			if !utf8.ValidString(frame.Reason) {
				t.Fatalf("truncated reason is not valid UTF-8: %q", frame.Reason)
			}
			if !strings.HasSuffix(frame.Reason, ReasonTruncationMarker) {
				t.Fatalf("truncated reason lacks marker: %q", frame.Reason)
			}
		})
	}
}

func TestEmptyDataDecodesToNonNilSlice(t *testing.T) {
	frame, err := DecodeUpstream([]byte(`{"type":"output","session":"s","data":""}`))
	if err != nil {
		t.Fatalf("DecodeUpstream() error = %v", err)
	}
	if frame.Data == nil || len(frame.Data) != 0 {
		t.Fatalf("decoded data = %#v, want non-nil empty slice", frame.Data)
	}
}

func TestEncodeRejectsInvalidFrame(t *testing.T) {
	tooLarge := make([]byte, MaxDataSize+1)
	tests := []struct {
		name string
		fn   func() error
		want error
	}{
		{"downstream wrong direction", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamType(UpstreamOutput), Session: "s"})
			return err
		}, ErrWrongDirection},
		{"upstream wrong direction", func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamType(DownstreamInput), Session: "s"})
			return err
		}, ErrWrongDirection},
		{"unknown type", func() error { _, err := EncodeDownstream(Downstream{Type: "mystery", Session: "s"}); return err }, ErrUnknownType},
		{"empty session", func() error { _, err := EncodeDownstream(Downstream{Type: DownstreamClose}); return err }, ErrInvalidSession},
		{"bad session", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamClose, Session: "bad\nsession"})
			return err
		}, ErrInvalidSession},
		{"control session", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamClose, Session: "bad\x01session"})
			return err
		}, ErrInvalidSession},
		{"long session", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamClose, Session: strings.Repeat("s", MaxSessionLength+1)})
			return err
		}, ErrInvalidSession},
		{"nil input data", func() error { _, err := EncodeDownstream(Downstream{Type: DownstreamInput, Session: "s"}); return err }, ErrInvalidData},
		{"large output data", func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamOutput, Session: "s", Data: tooLarge})
			return err
		}, ErrDataTooLarge},
		{"zero cols", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamResize, Session: "s", Cols: 0, Rows: 24})
			return err
		}, ErrInvalidGeometry},
		{"negative rows", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamResize, Session: "s", Cols: 80, Rows: -1})
			return err
		}, ErrInvalidGeometry},
		{"large cols", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamResize, Session: "s", Cols: MaxGeometry + 1, Rows: 24})
			return err
		}, ErrInvalidGeometry},
		{"large rows", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamResize, Session: "s", Cols: 80, Rows: MaxGeometry + 1})
			return err
		}, ErrInvalidGeometry},
		{"empty reason", func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s"})
			return err
		}, ErrInvalidReason},
		{"reason newline", func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s", Reason: "bad\nreason"})
			return err
		}, ErrInvalidReason},
		{"reason too long", func() error {
			_, err := EncodeUpstream(Upstream{Type: UpstreamError, Session: "s", Reason: strings.Repeat("r", MaxReasonLength+1)})
			return err
		}, ErrReasonTooLong},
		{"downstream extra field", func() error {
			_, err := EncodeDownstream(Downstream{Type: DownstreamClose, Session: "s", Data: []byte{}})
			return err
		}, ErrFieldNotAllowed},
		{"upstream extra field", func() error {
			code := 0
			_, err := EncodeUpstream(Upstream{Type: UpstreamReady, Session: "s", Code: &code})
			return err
		}, ErrFieldNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.fn(); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestErrorsNeverContainFrameContent(t *testing.T) {
	const sentinel = "FRAME-CONTENT-SENTINEL-9f84"
	tests := []struct {
		name     string
		upstream bool
		payload  []byte
	}{
		{"invalid JSON", false, []byte(`{"type":"close","session":"` + sentinel)},
		{"non-object", false, []byte(`"` + sentinel + `"`)},
		{"duplicate", false, []byte(`{"type":"close","type":"` + sentinel + `","session":"s"}`)},
		{"unknown type", false, []byte(`{"type":"` + sentinel + `","session":"s"}`)},
		{"wrong direction", false, []byte(`{"type":"output","session":"` + sentinel + `"}`)},
		{"bad session", false, []byte(`{"type":"close","session":"` + sentinel + `\n"}`)},
		{"bad data", false, []byte(`{"type":"input","session":"s","data":"` + sentinel + `"}`)},
		{"large data", true, oversizedSentinelOutput(sentinel)},
		{"bad geometry", false, []byte(`{"type":"resize","session":"` + sentinel + `","cols":0,"rows":24}`)},
		{"bad reason", true, []byte(`{"type":"error","session":"s","reason":"` + sentinel + `\n"}`)},
		{"controlled reason", true, []byte(`{"type":"error","session":"s","reason":` + quoted(sentinel+"\x1b") + `}`)},
		{"long cwd", false, []byte(`{"type":"open","session":"s","cwd":` + quoted(strings.Repeat(sentinel, MaxCWDLength/len(sentinel)+1)) + `,"cols":80,"rows":24}`)},
		{"out-of-range code", true, []byte(`{"type":"exit","session":"` + sentinel + `","code":2147483648}`)},
		{"field not allowed", false, []byte(`{"type":"close","session":"s","` + sentinel + `":true}`)},
		{"invalid field value", false, []byte(`{"type":"open","session":"s","cwd":` + quoted(sentinel) + `,"cols":"bad","rows":24}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.upstream {
				_, err = DecodeUpstream(test.payload)
			} else {
				_, err = DecodeDownstream(test.payload)
			}
			if err == nil {
				t.Fatal("decode unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error exposes frame content: %v", err)
			}
		})
	}

	encodeErrors := []error{
		encodeDownstreamError(Downstream{Type: DownstreamClose, Session: sentinel + "\n"}),
		encodeDownstreamError(Downstream{Type: DownstreamClose, Session: "s", CWD: sentinel}),
		encodeDownstreamError(Downstream{Type: DownstreamOpen, Session: "s", CWD: strings.Repeat(sentinel, MaxCWDLength/len(sentinel)+1), Cols: 80, Rows: 24}),
		encodeUpstreamError(Upstream{Type: UpstreamError, Session: "s", Reason: sentinel + "\n"}),
		encodeUpstreamError(Upstream{Type: UpstreamError, Session: "s", Reason: sentinel + "\x1b"}),
		encodeUpstreamError(Upstream{Type: UpstreamExit, Session: sentinel, Code: intPointer(2147483648)}),
		encodeUpstreamError(Upstream{Type: UpstreamReady, Session: "s", Reason: sentinel}),
	}
	for i, err := range encodeErrors {
		if err == nil {
			t.Fatalf("encode rejection %d unexpectedly succeeded", i)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("encode error exposes frame content: %v", err)
		}
	}
}

func TestDataIsBinarySafe(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	frame := Upstream{Type: UpstreamOutput, Session: "binary", Data: data}
	encoded, err := EncodeUpstream(frame)
	if err != nil {
		t.Fatalf("EncodeUpstream() error = %v", err)
	}
	decoded, err := DecodeUpstream(encoded)
	if err != nil {
		t.Fatalf("DecodeUpstream() error = %v", err)
	}
	if !bytes.Equal(decoded.Data, data) {
		t.Fatal("binary data changed during round trip")
	}
}

func TestDecodeRejectsNonObject(t *testing.T) {
	for _, payload := range []string{`[]`, `"value"`, `42`, `null`} {
		if _, err := DecodeDownstream([]byte(payload)); !errors.Is(err, ErrNonObject) {
			t.Errorf("DecodeDownstream(%s) error = %v, want ErrNonObject", payload, err)
		}
		if _, err := DecodeUpstream([]byte(payload)); !errors.Is(err, ErrNonObject) {
			t.Errorf("DecodeUpstream(%s) error = %v, want ErrNonObject", payload, err)
		}
	}
}

func TestDecodeRejectsMalformedAndUnexpectedFields(t *testing.T) {
	if _, err := DecodeDownstream([]byte(`{"type":`)); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("malformed JSON error = %v, want ErrInvalidJSON", err)
	}
	if _, err := DecodeDownstream([]byte(`{"type":"close","session":"s","channel":"fs:readFile"}`)); !errors.Is(err, ErrFieldNotAllowed) {
		t.Fatalf("unknown field error = %v, want ErrFieldNotAllowed", err)
	}
	if _, err := DecodeDownstream([]byte(`{"type":"close","type":"close","session":"s"}`)); !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("duplicate field error = %v, want ErrDuplicateField", err)
	}
}

func TestExitCodePresenceRoundTrips(t *testing.T) {
	for _, code := range []*int{nil, intPointer(0), intPointer(-1)} {
		frame := Upstream{Type: UpstreamExit, Session: "exit", Code: code}
		encoded, err := EncodeUpstream(frame)
		if err != nil {
			t.Fatalf("EncodeUpstream() error = %v", err)
		}
		decoded, err := DecodeUpstream(encoded)
		if err != nil {
			t.Fatalf("DecodeUpstream() error = %v", err)
		}
		if !reflect.DeepEqual(decoded, frame) {
			t.Fatalf("round trip mismatch: got %#v, want %#v", decoded, frame)
		}
	}
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func integer(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [32]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

func oversizedSentinelOutput(sentinel string) []byte {
	data := bytes.Repeat([]byte(sentinel), MaxDataSize/len(sentinel)+1)
	return []byte(`{"type":"output","session":"s","data":"` + base64.StdEncoding.EncodeToString(data) + `"}`)
}

func intPointer(value int) *int {
	return &value
}

func encodeDownstreamError(frame Downstream) error {
	_, err := EncodeDownstream(frame)
	return err
}

func encodeUpstreamError(frame Upstream) error {
	_, err := EncodeUpstream(frame)
	return err
}
