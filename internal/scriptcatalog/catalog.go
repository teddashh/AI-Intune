// Package scriptcatalog is the build-time allowlist shared by Hub and agent.
package scriptcatalog

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const Kind = "script_v1"

//go:embed fleet-probe-v1.sh
var probe []byte

type Property struct {
	Type      string   `json:"type"`
	Enum      []string `json:"enum,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	MinLength int      `json:"minLength,omitempty"`
	MaxLength int      `json:"maxLength,omitempty"`
	Minimum   *int     `json:"minimum,omitempty"`
	Maximum   *int     `json:"maximum,omitempty"`
}
type Schema struct {
	Type                 string              `json:"type"`
	AdditionalProperties bool                `json:"additionalProperties"`
	Properties           map[string]Property `json:"properties"`
	Required             []string            `json:"required,omitempty"`
}

// CatalogResponse is the object envelope returned by the operator catalog API.
type CatalogResponse struct {
	Items []Entry `json:"items"`
}

type Entry struct {
	ID             string   `json:"id"`
	Version        string   `json:"version"`
	SHA256         string   `json:"sha256"`
	ArgsSchema     Schema   `json:"args_schema"`
	DefaultTimeout int      `json:"default_timeout_seconds"`
	MaxTimeout     int      `json:"max_timeout_seconds"`
	OutputCap      int      `json:"output_cap_bytes"`
	Mode           string   `json:"mode"`
	RunsAs         string   `json:"runs_as"`
	AllowedOS      []string `json:"allowed_os"`
	Bytes          []byte   `json:"-"`
}

// Each additional embedded script is one registry entry. Hashes are constants,
// never computed from potentially changed bytes when accepting a job.
var registry = []Entry{
	{"fleet-probe-v1", "1", "0c242784f162df0489571fa0ac901e0e583f54afa3d121d0b033e8dff2bad924", Schema{Type: "object", Properties: map[string]Property{}}, 15, 60, 64 << 10, "read", "agent-user", []string{"linux"}, probe},
}

func List() []Entry {
	// Deep copy protects the immutable allowlist from callers.
	raw, _ := json.Marshal(registry)
	var out []Entry
	_ = json.Unmarshal(raw, &out)
	return out
}
func Lookup(id string) (Entry, bool) {
	for i, e := range registry {
		if e.ID == id {
			out := List()[i]
			out.Bytes = bytes.Clone(e.Bytes)
			return out, true
		}
	}
	return Entry{}, false
}
func Digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (e Entry) AllowsOS(os string) bool {
	for _, v := range e.AllowedOS {
		if v == os {
			return true
		}
	}
	return false
}

// Validate returns canonical args, rejecting duplicate and unknown fields.
// This deliberately small closed schema supports only bounded scalars.
func (e Entry) Validate(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > 4096 || !utf8.Valid(raw) {
		return nil, errors.New("args must be a JSON object of at most 4096 bytes")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("args must be an object")
	}
	values := map[string]any{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid args")
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("invalid args")
		}
		if _, ok := values[key]; ok {
			return nil, errors.New("duplicate args field")
		}
		p, ok := e.ArgsSchema.Properties[key]
		if !ok {
			return nil, errors.New("unknown args field")
		}
		var v any
		if d.Decode(&v) != nil {
			return nil, errors.New("invalid args")
		}
		switch p.Type {
		case "string":
			s, ok := v.(string)
			if !ok || utf8.RuneCountInString(s) < p.MinLength || p.MaxLength <= 0 || utf8.RuneCountInString(s) > p.MaxLength {
				return nil, errors.New("args string outside bounds")
			}
			if len(p.Enum) > 0 {
				found := false
				for _, x := range p.Enum {
					found = found || s == x
				}
				if !found {
					return nil, errors.New("args enum mismatch")
				}
			}
			if p.Pattern != "" {
				match, err := regexp.MatchString(p.Pattern, s)
				if err != nil || !match {
					return nil, errors.New("args pattern mismatch")
				}
			}
		case "integer":
			n, ok := v.(json.Number)
			if !ok {
				return nil, errors.New("args integer required")
			}
			i, err := n.Int64()
			if err != nil || p.Minimum == nil || p.Maximum == nil || i < int64(*p.Minimum) || i > int64(*p.Maximum) {
				return nil, errors.New("args integer outside bounds")
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return nil, errors.New("args boolean required")
			}
		default:
			return nil, errors.New("unsupported args schema")
		}
		values[key] = v
	}
	if _, err = d.Token(); err != nil {
		return nil, errors.New("invalid args")
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing args JSON")
	}
	for _, key := range e.ArgsSchema.Required {
		if _, ok := values[key]; !ok {
			return nil, errors.New("required args missing")
		}
	}
	return json.Marshal(values)
}

type Spec struct {
	Kind         string          `json:"kind"`
	ScriptID     string          `json:"script_id"`
	ScriptSHA256 string          `json:"script_sha256"`
	Args         json.RawMessage `json:"args"`
}

func Decode(raw []byte) (Spec, error) {
	var s Spec
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, errors.New("invalid script spec")
	}
	if d.Decode(new(any)) != io.EOF || s.Kind != Kind {
		return s, errors.New("invalid script spec")
	}
	return s, nil
}

var secrets = regexp.MustCompile(`(?i)(bearer\s+[^\s"']+|(?:"?(?:token|password|secret|api[_-]?key)"?)\s*[:=]\s*"?[^\s"']+|(?:sk-|gh[pousr]_|github_pat_)[a-z0-9_-]+|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|[a-z0-9_+/=-]{32,})`)

func Redact(s string) string {
	return secrets.ReplaceAllString(strings.ToValidUTF8(s, "�"), "[REDACTED]")
}
