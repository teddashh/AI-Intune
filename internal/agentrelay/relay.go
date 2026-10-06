// Package agentrelay defines the Hub↔agent frames and the Hub↔page frames of
// the operator terminal.
package agentrelay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/sessionid"
)

const (
	MaxSessionLength = sessionid.MaxLength
	MaxCWDLength     = 4096
	MaxDataSize      = 1 << 20
	MaxGeometry      = 10000
	MaxReasonLength  = 500
	minExitCode      = -1 << 31
	maxExitCode      = 1<<31 - 1

	ReasonTruncationMarker = " [truncated]"
)

// TerminalOS is the only GOOS whose agent dials the terminal link. bat-server
// has no build for any other operating system.
const TerminalOS = "linux"

var (
	ErrInvalidJSON       = errors.New("agent relay: invalid JSON")
	ErrNonObject         = errors.New("agent relay: JSON value is not an object")
	ErrDuplicateField    = errors.New("agent relay: duplicate field")
	ErrUnknownType       = errors.New("agent relay: unknown frame type")
	ErrWrongDirection    = errors.New("agent relay: frame type is not allowed in this direction")
	ErrInvalidSession    = errors.New("agent relay: invalid session field")
	ErrCWDTooLong        = errors.New("agent relay: cwd field exceeds maximum size")
	ErrInvalidData       = errors.New("agent relay: invalid data field")
	ErrDataTooLarge      = errors.New("agent relay: data field exceeds maximum size")
	ErrInvalidGeometry   = errors.New("agent relay: invalid terminal geometry")
	ErrInvalidReason     = errors.New("agent relay: invalid reason field")
	ErrReasonTooLong     = errors.New("agent relay: reason field exceeds maximum size")
	ErrFieldNotAllowed   = errors.New("agent relay: field is not allowed for frame type")
	ErrInvalidFieldValue = errors.New("agent relay: invalid field value")
	ErrInvalidUTF8       = errors.New("agent relay: data is not valid UTF-8")
)

type DownstreamType string

const (
	DownstreamOpen   DownstreamType = "open"
	DownstreamInput  DownstreamType = "input"
	DownstreamResize DownstreamType = "resize"
	DownstreamClose  DownstreamType = "close"
)

type UpstreamType string

const (
	UpstreamReady  UpstreamType = "ready"
	UpstreamOutput UpstreamType = "output"
	UpstreamExit   UpstreamType = "exit"
	UpstreamError  UpstreamType = "error"
)

// Downstream is a terminal frame sent from the Hub to an agent.
type Downstream struct {
	Type    DownstreamType
	Session string
	CWD     string
	Data    []byte
	Cols    int
	Rows    int
}

// Upstream is a terminal frame sent from an agent to the Hub.
type Upstream struct {
	Type    UpstreamType
	Session string
	Data    []byte
	Code    *int
	Reason  string
}

func EncodeDownstream(frame Downstream) ([]byte, error) {
	if err := validateDownstream(frame); err != nil {
		return nil, err
	}

	switch frame.Type {
	case DownstreamOpen:
		return json.Marshal(struct {
			Type    DownstreamType `json:"type"`
			Session string         `json:"session"`
			CWD     string         `json:"cwd,omitempty"`
			Cols    int            `json:"cols"`
			Rows    int            `json:"rows"`
		}{frame.Type, frame.Session, frame.CWD, frame.Cols, frame.Rows})
	case DownstreamInput:
		return json.Marshal(struct {
			Type    DownstreamType `json:"type"`
			Session string         `json:"session"`
			Data    string         `json:"data"`
		}{frame.Type, frame.Session, base64.StdEncoding.EncodeToString(frame.Data)})
	case DownstreamResize:
		return json.Marshal(struct {
			Type    DownstreamType `json:"type"`
			Session string         `json:"session"`
			Cols    int            `json:"cols"`
			Rows    int            `json:"rows"`
		}{frame.Type, frame.Session, frame.Cols, frame.Rows})
	case DownstreamClose:
		return json.Marshal(struct {
			Type    DownstreamType `json:"type"`
			Session string         `json:"session"`
		}{frame.Type, frame.Session})
	default:
		return nil, classifyDownstreamType(frame.Type)
	}
}

func DecodeDownstream(payload []byte) (Downstream, error) {
	fields, err := decodeObject(payload)
	if err != nil {
		return Downstream{}, err
	}

	typeName, err := requiredString(fields, "type", ErrUnknownType)
	if err != nil {
		return Downstream{}, err
	}
	frame := Downstream{Type: DownstreamType(typeName)}
	if err := classifyDownstreamType(frame.Type); err != nil {
		return Downstream{}, err
	}
	if err := rejectUnknownFields(fields, "type", "session", "cwd", "data", "cols", "rows"); err != nil {
		return Downstream{}, err
	}

	frame.Session, err = requiredString(fields, "session", ErrInvalidSession)
	if err != nil || !validSession(frame.Session) {
		return Downstream{}, ErrInvalidSession
	}

	switch frame.Type {
	case DownstreamOpen:
		if err := rejectPresent(fields, "data"); err != nil {
			return Downstream{}, err
		}
		if raw, ok := fields["cwd"]; ok {
			if err := json.Unmarshal(raw, &frame.CWD); err != nil {
				return Downstream{}, ErrInvalidFieldValue
			}
		}
		if err := validateCWD(frame.CWD); err != nil {
			return Downstream{}, err
		}
		frame.Cols, frame.Rows, err = decodeGeometry(fields)
	case DownstreamInput:
		if err := rejectPresent(fields, "cwd", "cols", "rows"); err != nil {
			return Downstream{}, err
		}
		frame.Data, err = decodeData(fields)
	case DownstreamResize:
		if err := rejectPresent(fields, "cwd", "data"); err != nil {
			return Downstream{}, err
		}
		frame.Cols, frame.Rows, err = decodeGeometry(fields)
	case DownstreamClose:
		err = rejectPresent(fields, "cwd", "data", "cols", "rows")
	}
	if err != nil {
		return Downstream{}, err
	}
	return frame, nil
}

func EncodeUpstream(frame Upstream) ([]byte, error) {
	if err := validateUpstream(frame); err != nil {
		return nil, err
	}

	switch frame.Type {
	case UpstreamReady:
		return json.Marshal(struct {
			Type    UpstreamType `json:"type"`
			Session string       `json:"session"`
		}{frame.Type, frame.Session})
	case UpstreamOutput:
		return json.Marshal(struct {
			Type    UpstreamType `json:"type"`
			Session string       `json:"session"`
			Data    string       `json:"data"`
		}{frame.Type, frame.Session, base64.StdEncoding.EncodeToString(frame.Data)})
	case UpstreamExit:
		return json.Marshal(struct {
			Type    UpstreamType `json:"type"`
			Session string       `json:"session"`
			Code    *int         `json:"code,omitempty"`
		}{frame.Type, frame.Session, frame.Code})
	case UpstreamError:
		return json.Marshal(struct {
			Type    UpstreamType `json:"type"`
			Session string       `json:"session"`
			Reason  string       `json:"reason"`
		}{frame.Type, frame.Session, frame.Reason})
	default:
		return nil, classifyUpstreamType(frame.Type)
	}
}

// DecodeUpstream truncates an overlong error reason to MaxReasonLength and
// appends ReasonTruncationMarker so callers receive a bounded, visible reason.
func DecodeUpstream(payload []byte) (Upstream, error) {
	fields, err := decodeObject(payload)
	if err != nil {
		return Upstream{}, err
	}

	typeName, err := requiredString(fields, "type", ErrUnknownType)
	if err != nil {
		return Upstream{}, err
	}
	frame := Upstream{Type: UpstreamType(typeName)}
	if err := classifyUpstreamType(frame.Type); err != nil {
		return Upstream{}, err
	}
	if err := rejectUnknownFields(fields, "type", "session", "data", "code", "reason"); err != nil {
		return Upstream{}, err
	}

	frame.Session, err = requiredString(fields, "session", ErrInvalidSession)
	if err != nil || !validSession(frame.Session) {
		return Upstream{}, ErrInvalidSession
	}

	switch frame.Type {
	case UpstreamReady:
		err = rejectPresent(fields, "data", "code", "reason")
	case UpstreamOutput:
		if err := rejectPresent(fields, "code", "reason"); err != nil {
			return Upstream{}, err
		}
		frame.Data, err = decodeData(fields)
	case UpstreamExit:
		if err := rejectPresent(fields, "data", "reason"); err != nil {
			return Upstream{}, err
		}
		if raw, ok := fields["code"]; ok {
			var code int64
			if err := json.Unmarshal(raw, &code); err != nil || code < minExitCode || code > maxExitCode {
				return Upstream{}, ErrInvalidFieldValue
			}
			codeValue := int(code)
			frame.Code = &codeValue
		}
	case UpstreamError:
		if err := rejectPresent(fields, "data", "code"); err != nil {
			return Upstream{}, err
		}
		frame.Reason, err = requiredString(fields, "reason", ErrInvalidReason)
		if err != nil || frame.Reason == "" || !utf8.ValidString(frame.Reason) || containsControl(frame.Reason) {
			return Upstream{}, ErrInvalidReason
		}
		if len(frame.Reason) > MaxReasonLength {
			frame.Reason = truncateReason(frame.Reason)
		}
	}
	if err != nil {
		return Upstream{}, err
	}
	return frame, nil
}

func validateDownstream(frame Downstream) error {
	if err := classifyDownstreamType(frame.Type); err != nil {
		return err
	}
	if !validSession(frame.Session) {
		return ErrInvalidSession
	}

	switch frame.Type {
	case DownstreamOpen:
		if frame.Data != nil {
			return ErrFieldNotAllowed
		}
		if err := validateCWD(frame.CWD); err != nil {
			return err
		}
		return validateGeometry(frame.Cols, frame.Rows)
	case DownstreamInput:
		if frame.CWD != "" || frame.Cols != 0 || frame.Rows != 0 {
			return ErrFieldNotAllowed
		}
		return validateData(frame.Data)
	case DownstreamResize:
		if frame.CWD != "" || frame.Data != nil {
			return ErrFieldNotAllowed
		}
		return validateGeometry(frame.Cols, frame.Rows)
	case DownstreamClose:
		if frame.CWD != "" || frame.Data != nil || frame.Cols != 0 || frame.Rows != 0 {
			return ErrFieldNotAllowed
		}
	}
	return nil
}

func validateUpstream(frame Upstream) error {
	if err := classifyUpstreamType(frame.Type); err != nil {
		return err
	}
	if !validSession(frame.Session) {
		return ErrInvalidSession
	}

	switch frame.Type {
	case UpstreamReady:
		if frame.Data != nil || frame.Code != nil || frame.Reason != "" {
			return ErrFieldNotAllowed
		}
	case UpstreamOutput:
		if frame.Code != nil || frame.Reason != "" {
			return ErrFieldNotAllowed
		}
		return validateData(frame.Data)
	case UpstreamExit:
		if frame.Data != nil || frame.Reason != "" {
			return ErrFieldNotAllowed
		}
		if frame.Code != nil && (int64(*frame.Code) < minExitCode || int64(*frame.Code) > maxExitCode) {
			return ErrInvalidFieldValue
		}
	case UpstreamError:
		if frame.Data != nil || frame.Code != nil {
			return ErrFieldNotAllowed
		}
		if frame.Reason == "" || !utf8.ValidString(frame.Reason) || containsControl(frame.Reason) {
			return ErrInvalidReason
		}
		if len(frame.Reason) > MaxReasonLength {
			return ErrReasonTooLong
		}
	}
	return nil
}

func classifyDownstreamType(frameType DownstreamType) error {
	switch frameType {
	case DownstreamOpen, DownstreamInput, DownstreamResize, DownstreamClose:
		return nil
	case DownstreamType(UpstreamReady), DownstreamType(UpstreamOutput), DownstreamType(UpstreamExit), DownstreamType(UpstreamError):
		return ErrWrongDirection
	default:
		return ErrUnknownType
	}
}

func classifyUpstreamType(frameType UpstreamType) error {
	switch frameType {
	case UpstreamReady, UpstreamOutput, UpstreamExit, UpstreamError:
		return nil
	case UpstreamType(DownstreamOpen), UpstreamType(DownstreamInput), UpstreamType(DownstreamResize), UpstreamType(DownstreamClose):
		return ErrWrongDirection
	default:
		return ErrUnknownType
	}
}

func validSession(session string) bool {
	return sessionid.Valid(session)
}

func validateCWD(cwd string) error {
	if !utf8.ValidString(cwd) || containsControl(cwd) {
		return ErrInvalidFieldValue
	}
	if len(cwd) > MaxCWDLength {
		return ErrCWDTooLong
	}
	return nil
}

func validateData(data []byte) error {
	if data == nil {
		return ErrInvalidData
	}
	if len(data) > MaxDataSize {
		return ErrDataTooLarge
	}
	return nil
}

func validateGeometry(cols, rows int) error {
	if cols <= 0 || cols > MaxGeometry || rows <= 0 || rows > MaxGeometry {
		return ErrInvalidGeometry
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func truncateReason(reason string) string {
	keep := MaxReasonLength - len(ReasonTruncationMarker)
	for keep > 0 && !utf8.RuneStart(reason[keep]) {
		keep--
	}
	return reason[:keep] + ReasonTruncationMarker
}

func decodeGeometry(fields map[string]json.RawMessage) (int, int, error) {
	var cols, rows int
	colsRaw, colsOK := fields["cols"]
	rowsRaw, rowsOK := fields["rows"]
	if !colsOK || !rowsOK || json.Unmarshal(colsRaw, &cols) != nil || json.Unmarshal(rowsRaw, &rows) != nil {
		return 0, 0, ErrInvalidGeometry
	}
	if err := validateGeometry(cols, rows); err != nil {
		return 0, 0, err
	}
	return cols, rows, nil
}

func decodeData(fields map[string]json.RawMessage) ([]byte, error) {
	encoded, err := requiredString(fields, "data", ErrInvalidData)
	if err != nil {
		return nil, err
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxDataSize+2) {
		return nil, ErrDataTooLarge
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(data) != encoded {
		return nil, ErrInvalidData
	}
	if len(data) > MaxDataSize {
		return nil, ErrDataTooLarge
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func decodeObject(payload []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return nil, ErrInvalidJSON
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrNonObject
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := decoder.Token(); err != nil {
		return nil, ErrInvalidJSON
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidJSON
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrInvalidJSON
		}
		if _, exists := fields[key]; exists {
			return nil, ErrDuplicateField
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, ErrInvalidJSON
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, ErrInvalidJSON
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidJSON
	}
	return fields, nil
}

func requiredString(fields map[string]json.RawMessage, name string, fieldErr error) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fieldErr
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fieldErr
	}
	return value, nil
}

func rejectUnknownFields(fields map[string]json.RawMessage, allowed ...string) error {
	for name := range fields {
		found := false
		for _, candidate := range allowed {
			if name == candidate {
				found = true
				break
			}
		}
		if !found {
			return ErrFieldNotAllowed
		}
	}
	return nil
}

func rejectPresent(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		if _, ok := fields[name]; ok {
			return ErrFieldNotAllowed
		}
	}
	return nil
}
