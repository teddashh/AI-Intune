package agentrelay

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"
)

// PageType is a frame sent by the operator page. The socket path is the
// session binding, so a page frame has no session field.
type PageType string

const (
	PageInput  PageType = "input"
	PageResize PageType = "resize"
)

// PageCommand is one decoded page→Hub frame.
type PageCommand struct {
	Type PageType
	Data []byte
	Cols int
	Rows int
}

// DecodePage parses one page→Hub frame with the same strictness as
// DecodeDownstream. input data must be canonical standard base64 of valid
// UTF-8. resize cols and rows are integers in 1..MaxGeometry. Any other
// type, key, duplicate, number, or trailing value is rejected.
func DecodePage(payload []byte) (PageCommand, error) {
	fields, err := decodeObject(payload)
	if err != nil {
		return PageCommand{}, err
	}
	typeName, err := requiredString(fields, "type", ErrUnknownType)
	if err != nil {
		return PageCommand{}, err
	}
	frame := PageCommand{Type: PageType(typeName)}
	switch frame.Type {
	case PageInput:
		if err := rejectUnknownFields(fields, "type", "data"); err != nil {
			return PageCommand{}, err
		}
		frame.Data, err = decodeData(fields)
		if err != nil {
			return PageCommand{}, err
		}
		if !utf8.Valid(frame.Data) {
			return PageCommand{}, ErrInvalidUTF8
		}
	case PageResize:
		if err := rejectUnknownFields(fields, "type", "cols", "rows"); err != nil {
			return PageCommand{}, err
		}
		frame.Cols, frame.Rows, err = decodeGeometry(fields)
		if err != nil {
			return PageCommand{}, err
		}
	case PageType(DownstreamOpen), PageType(DownstreamClose),
		PageType(UpstreamReady), PageType(UpstreamOutput), PageType(UpstreamExit), PageType(UpstreamError):
		return PageCommand{}, ErrWrongDirection
	default:
		return PageCommand{}, ErrUnknownType
	}
	return frame, nil
}

// EncodePageReady is the Hub→page frame that lets the page accept input.
func EncodePageReady() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: "ready"})
}

// EncodePageOutput is one chunk of terminal output. data follows the same
// size rule as an upstream output frame.
func EncodePageOutput(data []byte) ([]byte, error) {
	if err := validateData(data); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{Type: "output", Data: base64.StdEncoding.EncodeToString(data)})
}

// EncodePageExit reports the terminal program's exit code. The page rejects
// an exit frame that has no integer code, so code is required.
func EncodePageExit(code int) ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Code int    `json:"code"`
	}{Type: "exit", Code: code})
}

// EncodePageError carries one Hub sentence. The page displays reason as text.
func EncodePageError(reason string) ([]byte, error) {
	if reason == "" || !utf8.ValidString(reason) {
		return nil, ErrInvalidReason
	}
	return json.Marshal(struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}{Type: "error", Reason: reason})
}
