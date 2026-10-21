package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema resolution is disabled: %s", url)
}

// CompileTagTypeSchema validates schemas without filesystem or network access.
// Only fragment references within this document are supported. Schema annotations
// such as examples/default are data, not nested schema documents.
func CompileTagTypeSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	_, object := value.(map[string]any)
	_, boolean := value.(bool)
	if !object && !boolean {
		return nil, errors.New("format.schema must be a JSON Schema object or boolean")
	}
	if err := checkSchemaReferences(value); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(offlineSchemaLoader{})
	const location = "urn:mf-hub:tag-schema"
	if err := c.AddResource(location, value); err != nil {
		return nil, fmt.Errorf("invalid format.schema: %w", err)
	}
	s, err := c.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("invalid format.schema: %w", err)
	}
	return s, nil
}

func checkSchemaReferences(value any) error {
	m, ok := value.(map[string]any)
	if !ok {
		return nil // Boolean subschemas are permitted by the dialect.
	}
	if dialect, exists := m["$schema"]; exists && dialect != SchemaDialect {
		return errors.New("format.schema must use JSON Schema Draft 2020-12")
	}
	if _, exists := m["$id"]; exists {
		return errors.New("schema $id is not supported; use local fragment references")
	}
	for _, key := range []string{"$ref", "$dynamicRef"} {
		if ref, exists := m[key]; exists {
			text, ok := ref.(string)
			if !ok || !strings.HasPrefix(text, "#") {
				return fmt.Errorf("schema %s must be a local fragment reference", key)
			}
		}
	}
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		if children, ok := m[key].(map[string]any); ok {
			for _, child := range children {
				if err := checkSchemaReferences(child); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if children, ok := m[key].([]any); ok {
			for _, child := range children {
				if err := checkSchemaReferences(child); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"items", "contains", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		if err := checkSchemaReferences(m[key]); err != nil {
			return err
		}
	}
	return nil
}
