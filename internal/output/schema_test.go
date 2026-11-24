package output

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchemaFromType_StructAndPointers(t *testing.T) {
	type embeddedMeta struct {
		Policy string `json:"policy"`
	}
	type samplePayload struct {
		embeddedMeta
		ID        string         `json:"id"`
		Count     int            `json:"count"`
		Optional  string         `json:"optional,omitempty"`
		Timestamp time.Time      `json:"timestamp"`
		Nullable  *int           `json:"nullable"`
		Tags      []string       `json:"tags"`
		Meta      map[string]any `json:"meta,omitempty"`
		Ignored   string         `json:"-"`
		NoTag     bool
	}

	s := SchemaFromType(reflect.TypeOf(samplePayload{}), map[string]string{
		"id":     "Unique identifier.",
		"policy": "Effective policy mode.",
	})
	s = FinalizeRootSchema("sample", "Sample Payload", "slb sample", "Sample schema.", false, []string{"json"}, s)

	raw, err := MarshalJSONSchema(s)
	if err != nil {
		t.Fatalf("MarshalJSONSchema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc["$schema"] != SchemaDraft202012 {
		t.Fatalf("unexpected $schema: %v", doc["$schema"])
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing properties map")
	}
	if _, exists := props["Ignored"]; exists {
		t.Fatalf("json:\"-\" field should be excluded")
	}
	for _, want := range []string{"policy", "id", "count", "optional", "timestamp", "nullable", "tags", "meta", "NoTag"} {
		prop, exists := props[want].(map[string]any)
		if !exists {
			t.Fatalf("expected property %q in schema", want)
		}
		desc, _ := prop["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Fatalf("property %q missing description", want)
		}
	}
	if props["id"].(map[string]any)["description"] != "Unique identifier." {
		t.Fatalf("expected custom description for id, got %v", props["id"].(map[string]any)["description"])
	}
	if props["timestamp"].(map[string]any)["format"] != "date-time" {
		t.Fatalf("expected date-time format for time.Time, got %v", props["timestamp"])
	}

	// Validate real marshaled payload against the generated schema.
	n := 7
	instance := samplePayload{
		embeddedMeta: embeddedMeta{Policy: "same_host"},
		ID:           "req-1",
		Count:        3,
		Timestamp:    time.Now().UTC(),
		Nullable:     &n,
		Tags:         []string{"a"},
		NoTag:        true,
	}
	payloadBytes, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	var payload any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDocumentAgainstSchema(doc, payload); err != nil {
		t.Fatalf("valid payload failed validation: %v", err)
	}

	// Planted negative: missing required field must fail validation.
	badObj := payload.(map[string]any)
	delete(badObj, "id")
	if err := ValidateDocumentAgainstSchema(doc, badObj); err == nil {
		t.Fatalf("expected missing required field 'id' to fail schema validation")
	}

	// Planted negative: wrong type must fail validation.
	badObj["id"] = 12345
	if err := ValidateDocumentAgainstSchema(doc, badObj); err == nil {
		t.Fatalf("expected non-string 'id' to fail schema validation")
	}

	// Planted negative: unknown field when additionalProperties=false must fail validation.
	badObj["id"] = "req-1"
	badObj["unexpected_extra_field"] = true
	if err := ValidateDocumentAgainstSchema(doc, badObj); err == nil {
		t.Fatalf("expected unknown field to fail additionalProperties=false")
	}
}

// ValidateDocumentAgainstSchema is a strict test-only Draft 2020-12 subset validator
// used to verify real marshaled payloads conform to emitted schemas.
func ValidateDocumentAgainstSchema(schema map[string]any, value any) error {
	return validateValue("$", schema, value)
}

func validateValue(path string, schema map[string]any, value any) error {
	if anyOf, ok := schema["anyOf"].([]any); ok {
		var errs []string
		for _, rawSub := range anyOf {
			sub, ok := rawSub.(map[string]any)
			if !ok {
				continue
			}
			if err := validateValue(path, sub, value); err == nil {
				return nil
			} else {
				errs = append(errs, err.Error())
			}
		}
		return fmt.Errorf("%s matched none of anyOf (%s)", path, strings.Join(errs, "; "))
	}
	if oneOf, ok := schema["oneOf"].([]any); ok {
		matches := 0
		var errs []string
		for _, rawSub := range oneOf {
			sub, ok := rawSub.(map[string]any)
			if !ok {
				continue
			}
			if err := validateValue(path, sub, value); err == nil {
				matches++
			} else {
				errs = append(errs, err.Error())
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s matched %d of oneOf (want 1): %s", path, matches, strings.Join(errs, "; "))
		}
		return nil
	}
	if c, hasConst := schema["const"]; hasConst {
		if !reflect.DeepEqual(c, value) {
			return fmt.Errorf("%s: got %#v, want const %#v", path, value, c)
		}
	}
	if enumVals, ok := schema["enum"].([]any); ok {
		matched := false
		for _, ev := range enumVals {
			if reflect.DeepEqual(ev, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %#v not in enum %v", path, value, enumVals)
		}
	}
	if t, hasType := schema["type"]; hasType {
		if !matchesType(t, value) {
			return fmt.Errorf("%s: value %#v does not match type %v", path, value, t)
		}
	}
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		if format, _ := schema["format"].(string); format == "date-time" && v != "" {
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				return fmt.Errorf("%s: invalid date-time %q: %w", path, v, err)
			}
		}
	case map[string]any:
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, rk := range reqRaw {
				key, _ := rk.(string)
				if _, exists := v[key]; !exists {
					return fmt.Errorf("%s: missing required property %q", path, key)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, val := range v {
			if props != nil {
				if sub, exists := props[k].(map[string]any); exists {
					if err := validateValue(path+"."+k, sub, val); err != nil {
						return err
					}
					continue
				}
			}
			if ap, hasAP := schema["additionalProperties"]; hasAP {
				switch apVal := ap.(type) {
				case bool:
					if !apVal {
						return fmt.Errorf("%s: unexpected additional property %q", path, k)
					}
				case map[string]any:
					if err := validateValue(path+"."+k, apVal, val); err != nil {
						return err
					}
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, elem := range v {
				if err := validateValue(fmt.Sprintf("%s[%d]", path, i), items, elem); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func matchesType(schemaType any, value any) bool {
	switch st := schemaType.(type) {
	case string:
		return matchesSingleType(st, value)
	case []any:
		for _, item := range st {
			if s, ok := item.(string); ok && matchesSingleType(s, value) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func matchesSingleType(t string, value any) bool {
	switch t {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}
