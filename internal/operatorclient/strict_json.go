package operatorclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// decodeStrictJSONDocument is used by read APIs whose response is a nested,
// versioned DTO. encoding/json deliberately accepts case-insensitive field
// aliases and duplicate object names; either behaviour could make the CLI see
// a different fleet fact than the bytes an operator captured for evidence.
func decodeStrictJSONDocument(raw []byte, label string, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("operator client: %s response must be a JSON object", label)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return fmt.Errorf("operator client: invalid %s response: %w", label, err)
	}
	if err := validateExactJSONShape(raw, reflect.TypeOf(dst)); err != nil {
		return fmt.Errorf("operator client: invalid %s response: %w", label, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("operator client: decode %s response: %w", label, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("operator client: %s response has trailing JSON", label)
	}
	return nil
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// validateExactJSONShape closes encoding/json's case-insensitive field-name
// compatibility. It follows the declared response type through nested structs
// and arrays after the duplicate-name scanner has made map decoding unambiguous.
func validateExactJSONShape(raw []byte, typ reflect.Type) error {
	nullable := false
	for typ != nil && typ.Kind() == reflect.Pointer {
		nullable = true
		typ = typ.Elem()
	}
	if typ == nil {
		return errors.New("response target has no concrete type")
	}
	if typ.Implements(jsonUnmarshalerType) ||
		(typ.Kind() != reflect.Pointer && reflect.PointerTo(typ).Implements(jsonUnmarshalerType)) {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		if nullable || typ.Kind() == reflect.Interface {
			return nil
		}
		return errors.New("null is not allowed for this field")
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		allowed := make(map[string]reflect.Type)
		required := make(map[string]bool)
		if err := collectExactJSONStructFields(typ, allowed, required); err != nil {
			return err
		}
		for name, value := range object {
			fieldType, ok := allowed[name]
			if !ok {
				return fmt.Errorf("unknown field %q", name)
			}
			if err := validateExactJSONShape(value, fieldType); err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
		}
		for name, isRequired := range required {
			if _, present := object[name]; isRequired && !present {
				return fmt.Errorf("missing field %q", name)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for i, value := range values {
			if err := validateExactJSONShape(value, typ.Elem()); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for name, value := range values {
			if err := validateExactJSONShape(value, typ.Elem()); err != nil {
				return fmt.Errorf("map value %q: %w", name, err)
			}
		}
	}
	return nil
}

func collectExactJSONStructFields(typ reflect.Type, allowed map[string]reflect.Type,
	required map[string]bool,
) error {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag, tagged := field.Tag.Lookup("json")
		name, options := "", ""
		if tagged {
			name, options, _ = strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				if err := collectExactJSONStructFields(embedded, allowed, required); err != nil {
					return err
				}
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		if _, exists := allowed[name]; exists {
			return fmt.Errorf("response type has ambiguous JSON field %q", name)
		}
		allowed[name] = field.Type
		required[name] = !tagged || !jsonTagHasOption(options, "omitempty")
	}
	return nil
}

func jsonTagHasOption(options, wanted string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == wanted {
			return true
		}
	}
	return false
}

func rejectDuplicateJSONFields(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := scanStrictJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response has trailing JSON")
		}
		return fmt.Errorf("read trailing JSON: %w", err)
	}
	return nil
}

func scanStrictJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
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
			if err := scanStrictJSONValue(dec); err != nil {
				return err
			}
		}
		closing, err := dec.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("object did not close")
		}
		return nil
	case '[':
		for dec.More() {
			if err := scanStrictJSONValue(dec); err != nil {
				return err
			}
		}
		closing, err := dec.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("array did not close")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
