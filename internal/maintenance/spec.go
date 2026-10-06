package maintenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Spec is the desired-state document for one disk-clean revision.
// It carries no shell, no operator command, and no path outside the closed
// profile. Conf is the exact rendered file. The agent re-renders Profile and
// refuses to run unless the bytes and ConfigDigest both match.
type Spec struct {
	Kind          string  `json:"kind"`
	SchemaVersion int     `json:"schema_version"`
	ResourceID    string  `json:"resource_id"`
	Scope         string  `json:"scope"`
	ConfigDigest  string  `json:"config_digest"`
	Conf          string  `json:"conf"`
	Profile       Profile `json:"profile"`
}

// BuildSpec renders the profile and returns the job spec plus the conf bytes.
func BuildSpec(p Profile) (Spec, []byte, error) {
	conf, digest, err := Render(p)
	if err != nil {
		return Spec{}, nil, err
	}
	return Spec{
		Kind:          JobKind,
		SchemaVersion: SchemaVersion,
		ResourceID:    ResourceID,
		Scope:         p.Scope,
		ConfigDigest:  digest,
		Conf:          string(conf),
		Profile:       p.normalized(),
	}, conf, nil
}

// ParseSpec re-validates a job spec. A command, shell, or any other unknown
// field is refused. Conf must be exactly what Render produces for Profile.
func ParseSpec(raw []byte) (Spec, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Spec{}, errors.New("maintenance: spec is empty")
	}
	if len(raw) > MaxProfileBytes+MaxConfBytes {
		return Spec{}, errors.New("maintenance: spec is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec Spec
	if err := dec.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("maintenance: spec JSON: %s", err.Error())
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("maintenance: spec has trailing data")
	}
	if spec.Kind != JobKind || spec.SchemaVersion != SchemaVersion || spec.ResourceID != ResourceID {
		return Spec{}, errors.New("maintenance: spec kind, schema_version, or resource_id is not the disk-clean v1 document")
	}
	if err := spec.Profile.Validate(); err != nil {
		return Spec{}, err
	}
	if spec.Scope != spec.Profile.Scope {
		return Spec{}, errors.New("maintenance: spec scope does not match the profile")
	}
	conf, digest, err := Render(spec.Profile)
	if err != nil {
		return Spec{}, err
	}
	if spec.Conf != string(conf) || spec.ConfigDigest != digest {
		return Spec{}, errors.New("maintenance: spec conf or config_digest does not match the rendered profile")
	}
	return spec, nil
}

// MarshalSpec encodes a spec that ParseSpec will accept.
func MarshalSpec(spec Spec) ([]byte, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if _, err := ParseSpec(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
