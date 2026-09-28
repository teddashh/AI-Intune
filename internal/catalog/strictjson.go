package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// ParseManifest decodes one exact manifest schema. Unknown, missing,
// case-aliased, duplicate and trailing fields are rejected before validation.
func ParseManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeStrictDocument(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("catalog: invalid manifest: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, fmt.Errorf("catalog: invalid manifest: %w", err)
	}
	return canonicalManifest(manifest), nil
}

// ParseProfile decodes one exact machine-profile schema.
func ParseProfile(raw []byte) (MachineProfile, error) {
	var profile MachineProfile
	if err := decodeStrictDocument(raw, &profile); err != nil {
		return MachineProfile{}, fmt.Errorf("catalog: invalid profile: %w", err)
	}
	if err := ValidateProfile(profile); err != nil {
		return MachineProfile{}, fmt.Errorf("catalog: invalid profile: %w", err)
	}
	profile.Packages = append([]PackageRef(nil), profile.Packages...)
	sortPackageRefs(profile.Packages)
	return profile, nil
}

func decodeStrictDocument(raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("document must be a JSON object")
	}
	if err := rejectDuplicateFields(raw); err != nil {
		return err
	}
	if err := validateExactShape(raw, reflect.TypeOf(dst)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("document has trailing JSON")
	}
	return nil
}

func validateExactShape(raw []byte, typ reflect.Type) error {
	nullable := false
	for typ != nil && typ.Kind() == reflect.Pointer {
		nullable = true
		typ = typ.Elem()
	}
	if typ == nil {
		return errors.New("decode target has no concrete type")
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		if nullable || typ.Kind() == reflect.Interface {
			return nil
		}
		return errors.New("null is not allowed")
	}

	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		allowed := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" || field.Anonymous {
				continue
			}
			name := field.Name
			if tag, exists := field.Tag.Lookup("json"); exists {
				name, _, _ = strings.Cut(tag, ",")
				if name == "-" {
					continue
				}
				if name == "" {
					name = field.Name
				}
			}
			allowed[name] = field.Type
		}
		for name, value := range object {
			fieldType, exists := allowed[name]
			if !exists {
				return fmt.Errorf("unknown field %q", name)
			}
			if err := validateExactShape(value, fieldType); err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
		}
		for name := range allowed {
			if _, exists := object[name]; !exists {
				return fmt.Errorf("missing field %q", name)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for i, value := range values {
			if err := validateExactShape(value, typ.Elem()); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	}
	return nil
}

func rejectDuplicateFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("document has trailing JSON")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object field name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("object did not close")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("array did not close")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}
