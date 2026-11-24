package output

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// SchemaDraft202012 is the canonical JSON Schema meta-schema URI emitted by SLB.
const SchemaDraft202012 = "https://json-schema.org/draft/2020-12/schema"

// SchemaIDPrefix is the base URI for SLB command output schemas.
const SchemaIDPrefix = "https://github.com/Dicklesworthstone/slb/schemas/"

// JSONSchema represents a JSON Schema (Draft 2020-12) document or sub-schema.
type JSONSchema struct {
	Schema               string                 `json:"$schema,omitempty"`
	ID                   string                 `json:"$id,omitempty"`
	Title                string                 `json:"title,omitempty"`
	Description          string                 `json:"description,omitempty"`
	Type                 any                    `json:"type,omitempty"`
	Format               string                 `json:"format,omitempty"`
	Enum                 []any                  `json:"enum,omitempty"`
	Const                any                    `json:"const,omitempty"`
	Properties           map[string]*JSONSchema `json:"properties,omitempty"`
	Required             []string               `json:"required,omitempty"`
	Items                *JSONSchema            `json:"items,omitempty"`
	AdditionalProperties any                    `json:"additionalProperties,omitempty"`
	AnyOf                []*JSONSchema          `json:"anyOf,omitempty"`
	OneOf                []*JSONSchema          `json:"oneOf,omitempty"`
	Minimum              *float64               `json:"minimum,omitempty"`
	Maximum              *float64               `json:"maximum,omitempty"`
	Command              string                 `json:"x-slb-command,omitempty"`
	Streaming            bool                   `json:"x-slb-streaming,omitempty"`
	Formats              []string               `json:"x-slb-formats,omitempty"`
}

// StringSchema creates a string property schema with a description.
func StringSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: "string", Description: desc}
}

// DateTimeSchema creates an RFC3339 date-time string schema.
func DateTimeSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: "string", Format: "date-time", Description: desc}
}

// NullableDateTimeSchema creates a nullable RFC3339 date-time schema.
func NullableDateTimeSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: []string{"string", "null"}, Format: "date-time", Description: desc}
}

// NullableStringSchema creates a nullable string schema.
func NullableStringSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: []string{"string", "null"}, Description: desc}
}

// EnumSchema creates a string enum schema with allowed values.
func EnumSchema(desc string, values ...string) *JSONSchema {
	raw := make([]any, len(values))
	for i, v := range values {
		raw[i] = v
	}
	return &JSONSchema{Type: "string", Description: desc, Enum: raw}
}

// NullableEnumSchema creates a nullable string enum schema.
func NullableEnumSchema(desc string, values ...string) *JSONSchema {
	raw := make([]any, 0, len(values)+1)
	for _, v := range values {
		raw = append(raw, v)
	}
	raw = append(raw, nil)
	return &JSONSchema{Type: []string{"string", "null"}, Description: desc, Enum: raw}
}

// IntSchema creates an integer property schema.
func IntSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: "integer", Description: desc}
}

// NullableIntSchema creates a nullable integer property schema.
func NullableIntSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: []string{"integer", "null"}, Description: desc}
}

// NumberSchema creates a floating-point number property schema.
func NumberSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: "number", Description: desc}
}

// BoolSchema creates a boolean property schema.
func BoolSchema(desc string) *JSONSchema {
	return &JSONSchema{Type: "boolean", Description: desc}
}

// AnySchema creates a schema accepting any JSON value with a description.
func AnySchema(desc string) *JSONSchema {
	return &JSONSchema{Description: desc}
}

// ArraySchema creates a non-null array schema.
func ArraySchema(desc string, items *JSONSchema) *JSONSchema {
	return &JSONSchema{Type: "array", Description: desc, Items: items}
}

// NullableArraySchema creates an array schema that also accepts JSON null (e.g., nil Go slices).
func NullableArraySchema(desc string, items *JSONSchema) *JSONSchema {
	return &JSONSchema{Type: []string{"array", "null"}, Description: desc, Items: items}
}

// MapSchema creates an object schema with typed additionalProperties.
func MapSchema(desc string, valueSchema *JSONSchema) *JSONSchema {
	return &JSONSchema{Type: "object", Description: desc, AdditionalProperties: valueSchema}
}

// ObjectSchema creates an object schema with explicit properties and required keys.
func ObjectSchema(desc string, props map[string]*JSONSchema, required ...string) *JSONSchema {
	s := &JSONSchema{
		Type:                 "object",
		Description:          desc,
		Properties:           props,
		AdditionalProperties: false,
	}
	if len(required) > 0 {
		req := append([]string(nil), required...)
		sort.Strings(req)
		s.Required = req
	}
	return s
}

// MergeProperties copies properties from src into dst and merges required keys.
func MergeProperties(dst, src *JSONSchema) *JSONSchema {
	if dst.Properties == nil {
		dst.Properties = make(map[string]*JSONSchema, len(src.Properties))
	}
	for k, v := range src.Properties {
		if _, exists := dst.Properties[k]; !exists {
			dst.Properties[k] = v
		}
	}
	seen := make(map[string]bool, len(dst.Required)+len(src.Required))
	merged := make([]string, 0, len(dst.Required)+len(src.Required))
	for _, r := range dst.Required {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}
	for _, r := range src.Required {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}
	sort.Strings(merged)
	dst.Required = merged
	return dst
}

// FinalizeRootSchema stamps canonical JSON Schema Draft 2020-12 root metadata onto s.
func FinalizeRootSchema(slug, title, command, desc string, streaming bool, formats []string, s *JSONSchema) *JSONSchema {
	if s == nil {
		s = &JSONSchema{Type: "object"}
	}
	s.Schema = SchemaDraft202012
	s.ID = SchemaIDPrefix + slug + ".schema.json"
	s.Title = title
	if s.Description == "" {
		s.Description = desc
	}
	s.Command = command
	s.Streaming = streaming
	if len(formats) > 0 {
		s.Formats = append([]string(nil), formats...)
	}
	return s
}

// SchemaFromType reflects a Go type into a JSON Schema matching encoding/json output.
// Optional fieldDescriptions maps JSON property names (or "Parent.field" paths) to human descriptions.
func SchemaFromType(t reflect.Type, fieldDescriptions map[string]string) *JSONSchema {
	return schemaFromType(t, "", fieldDescriptions, make(map[reflect.Type]bool))
}

var timeType = reflect.TypeOf(time.Time{})

func schemaFromType(t reflect.Type, path string, descs map[string]string, visiting map[reflect.Type]bool) *JSONSchema {
	for t.Kind() == reflect.Pointer {
		inner := schemaFromType(t.Elem(), path, descs, visiting)
		return makeNullable(inner)
	}

	if t == timeType {
		return DateTimeSchema(lookupDescription(path, t.Name(), descs))
	}

	switch t.Kind() {
	case reflect.String:
		return StringSchema(lookupDescription(path, t.Name(), descs))
	case reflect.Bool:
		return BoolSchema(lookupDescription(path, t.Name(), descs))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return IntSchema(lookupDescription(path, t.Name(), descs))
	case reflect.Float32, reflect.Float64:
		return NumberSchema(lookupDescription(path, t.Name(), descs))
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return StringSchema(lookupDescription(path, "base64", descs))
		}
		elemSchema := schemaFromType(t.Elem(), path+"[]", descs, visiting)
		if t.Kind() == reflect.Slice {
			return NullableArraySchema(lookupDescription(path, t.Name(), descs), elemSchema)
		}
		return ArraySchema(lookupDescription(path, t.Name(), descs), elemSchema)
	case reflect.Map:
		valSchema := schemaFromType(t.Elem(), path+".*", descs, visiting)
		return &JSONSchema{
			Type:                 []string{"object", "null"},
			Description:          lookupDescription(path, t.Name(), descs),
			AdditionalProperties: valSchema,
		}
	case reflect.Interface:
		return AnySchema(lookupDescription(path, "value", descs))
	case reflect.Struct:
		if visiting[t] {
			return &JSONSchema{Type: "object", Description: lookupDescription(path, t.Name(), descs)}
		}
		visiting[t] = true
		defer delete(visiting, t)

		props := make(map[string]*JSONSchema)
		var required []string
		collectStructFields(t, path, descs, visiting, props, &required)
		sort.Strings(required)
		return &JSONSchema{
			Type:                 "object",
			Description:          lookupDescription(path, t.Name(), descs),
			Properties:           props,
			Required:             required,
			AdditionalProperties: false,
		}
	default:
		return AnySchema(lookupDescription(path, t.Name(), descs))
	}
}

func collectStructFields(
	t reflect.Type,
	prefix string,
	descs map[string]string,
	visiting map[reflect.Type]bool,
	props map[string]*JSONSchema,
	required *[]string,
) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		name, opts := parseJSONTag(jsonTag)
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType {
				collectStructFields(ft, prefix, descs, visiting, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fieldPath := name
		if prefix != "" {
			fieldPath = prefix + "." + name
		}
		propSchema := schemaFromType(f.Type, fieldPath, descs, visiting)
		if d := lookupExactDescription(fieldPath, name, descs); d != "" {
			propSchema.Description = d
		} else if propSchema.Description == "" {
			propSchema.Description = humanizeFieldName(name)
		}
		props[name] = propSchema
		if !strings.Contains(opts, "omitempty") {
			*required = append(*required, name)
		}
	}
}

func parseJSONTag(tag string) (string, string) {
	if tag == "" {
		return "", ""
	}
	name, opts, _ := strings.Cut(tag, ",")
	return name, opts
}

func makeNullable(s *JSONSchema) *JSONSchema {
	if s == nil {
		return &JSONSchema{Type: "null"}
	}
	cp := *s
	switch tv := cp.Type.(type) {
	case string:
		if tv != "null" && tv != "" {
			cp.Type = []string{tv, "null"}
		}
	case []string:
		hasNull := false
		for _, item := range tv {
			if item == "null" {
				hasNull = true
				break
			}
		}
		if !hasNull {
			cp.Type = append(append([]string(nil), tv...), "null")
		}
	}
	return &cp
}

func lookupExactDescription(path, name string, descs map[string]string) string {
	if descs == nil {
		return ""
	}
	if d, ok := descs[path]; ok && d != "" {
		return d
	}
	if d, ok := descs[name]; ok && d != "" {
		return d
	}
	return ""
}

func lookupDescription(path, fallback string, descs map[string]string) string {
	if descs != nil {
		if d, ok := descs[path]; ok && d != "" {
			return d
		}
		if idx := strings.LastIndex(path, "."); idx >= 0 {
			if d, ok := descs[path[idx+1:]]; ok && d != "" {
				return d
			}
		}
	}
	if fallback != "" {
		return humanizeFieldName(fallback)
	}
	if path != "" {
		return humanizeFieldName(path)
	}
	return "Structured value."
}

func humanizeFieldName(name string) string {
	name = strings.TrimSuffix(name, "[]")
	name = strings.TrimSuffix(name, ".*")
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return "Value."
	}
	// Convert snake_case or CamelCase to readable phrase.
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if r == '_' || r == '-' {
			b.WriteByte(' ')
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z' && nextLower) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	words := strings.Fields(b.String())
	if len(words) == 0 {
		return "Value."
	}
	phrase := strings.ToLower(strings.Join(words, " "))
	return fmt.Sprintf("%s%s.", strings.ToUpper(phrase[:1]), phrase[1:])
}

// MarshalJSONSchema returns pretty-printed deterministic JSON Schema bytes.
func MarshalJSONSchema(s *JSONSchema) ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}
