package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// ValidateValue validates an instance against an exact, installed definition.
func ValidateValue(def TagType, raw json.RawMessage, lookup func(string, int) (TagType, error)) error {
	var value any
	if err := DecodeStrict(raw, &value); err != nil {
		return err
	}
	return validateValue(def, value, lookup, 0)
}
func validateValue(def TagType, value any, lookup func(string, int) (TagType, error), depth int) error {
	if depth > 64 {
		return fmt.Errorf("tag type nesting exceeds 64 levels")
	}
	switch def.DataType {
	case "json":
		schema, err := CompileTagTypeSchema(def.Format.Schema)
		if err != nil {
			return err
		}
		return schema.Validate(value)
	case "tag_set":
		object, ok := value.(map[string]any)
		if !ok || len(object) != len(def.Format.TagTypes) {
			return fmt.Errorf("%s requires an object with every declared child tag type", def.Key)
		}
		for key, version := range def.Format.TagTypes {
			child, err := lookup(key, version)
			if err != nil {
				return err
			}
			v, ok := object[key]
			if !ok {
				return fmt.Errorf("missing child %s", key)
			}
			if err = validateValue(child, v, lookup, depth+1); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s requires a string", def.Key)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s requires a boolean", def.Key)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s requires a number", def.Key)
		}
	default:
		return fmt.Errorf("unknown tag type data_type %q", def.DataType)
	}
	if def.Format == nil {
		return nil
	}
	if r := def.Format.Range; r != nil {
		n := string(value.(json.Number))
		if compareNumber(n, strconv.FormatFloat(r[0], 'g', -1, 64)) < 0 || compareNumber(n, strconv.FormatFloat(r[1], 'g', -1, 64)) > 0 {
			return fmt.Errorf("value outside inclusive range [%v,%v]", r[0], r[1])
		}
	}
	if len(def.Format.Choices) > 0 {
		var choices any
		d := json.NewDecoder(bytes.NewReader(def.Format.Choices))
		d.UseNumber()
		if err := d.Decode(&choices); err != nil {
			return err
		}
		switch values := choices.(type) {
		case []any:
			for _, v := range values {
				if equalValue(v, value) {
					return nil
				}
			}
		case map[string]any:
			for _, v := range values {
				if equalValue(v, value) {
					return nil
				}
			}
		}
		return fmt.Errorf("value is not one of the declared choices")
	}
	return nil
}
func equalValue(a, b any) bool {
	x, ok := a.(json.Number)
	if !ok {
		return reflect.DeepEqual(a, b)
	}
	y, ok := b.(json.Number)
	if !ok {
		return false
	}
	return compareNumber(string(x), string(y)) == 0
}

// Compare validated JSON numbers without allocating powers of ten for large
// exponents. Range bounds and manifest choices are finite numbers.
func compareNumber(a, b string) int {
	type decimal struct {
		negative bool
		digits   string
		exponent int64
	}
	parse := func(s string) decimal {
		d := decimal{}
		if strings.HasPrefix(s, "-") {
			d.negative = true
			s = s[1:]
		}
		if n := strings.IndexAny(s, "eE"); n >= 0 {
			e, err := strconv.ParseInt(s[n+1:], 10, 64)
			if err != nil || e > 1<<60 || e < -(1<<60) {
				e = 1 << 60
				if strings.HasPrefix(s[n+1:], "-") {
					e = -(1 << 60)
				}
			}
			d.exponent = e
			s = s[:n]
		}
		if n := strings.IndexByte(s, '.'); n >= 0 {
			d.exponent -= int64(len(s) - n - 1)
			s = s[:n] + s[n+1:]
		}
		d.digits = strings.TrimLeft(s, "0")
		if d.digits == "" {
			d.negative = false
			d.exponent = 0
		}
		return d
	}
	x, y := parse(a), parse(b)
	if x.digits == "" && y.digits == "" {
		return 0
	}
	if x.negative != y.negative {
		if x.negative {
			return -1
		}
		return 1
	}
	sign := 1
	if x.negative {
		sign = -1
	}
	if x.digits == "" {
		return -sign
	}
	if y.digits == "" {
		return sign
	}
	xe, ye := int64(len(x.digits))+x.exponent, int64(len(y.digits))+y.exponent
	if xe < ye {
		return -sign
	}
	if xe > ye {
		return sign
	}
	width := max(len(x.digits), len(y.digits))
	return sign * strings.Compare(x.digits+strings.Repeat("0", width-len(x.digits)), y.digits+strings.Repeat("0", width-len(y.digits)))
}

// ClientView translates transport-only schema terminology for client output.
// Call only on hub definitions/plans, never on segments or instance values.
func ClientView(value any) any {
	raw, _ := json.Marshal(value)
	var decoded any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&decoded)
	var convert func(any) any
	convert = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			if _, definition := x["data_type"]; definition {
				for k, v := range x {
					out[k] = v
				}
				if x["data_type"] == "tag_set" {
					if format, ok := x["format"].(map[string]any); ok {
						f := map[string]any{}
						for k, v := range format {
							if k == "tags" {
								k = "tag_types"
							}
							f[k] = v
						}
						out["format"] = f
					}
				}
				return out
			}
			_, flow := x["gh_repo"]
			_, resolution := x["root"]
			_, ref := x["identifier"]
			for k, v := range x {
				if k == "tags" && (flow || resolution) {
					k = "tag_types"
				}
				if ref && k == "kind" && v == "tags" {
					v = "tagtypes"
				}
				if k == "schema" {
					out[k] = v
				} else {
					out[k] = convert(v)
				}
			}
			return out
		case []any:
			for i := range x {
				x[i] = convert(x[i])
			}
			return x
		}
		return v
	}
	return convert(decoded)
}
func WireKind(kind string) string {
	if kind == "tagtypes" || kind == "tag_types" || kind == "tag-types" {
		return TagTypes
	}
	return kind
}
func LocalKind(kind string) string {
	if kind == TagTypes {
		return "tagtypes"
	}
	return kind
}
