package output

import (
	"encoding/json"
	"fmt"
	"net/netip"
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

type schemaTestMutualA struct {
	*schemaTestMutualB
	A string `json:"a"`
}

type schemaTestMutualB struct {
	*schemaTestMutualA
	B string `json:"b"`
}

func TestSchemaFromType_PointerEmbeddedCycles(t *testing.T) {
	type selfNode struct {
		*selfNode
		Name string `json:"name"`
	}
	type outerWrap struct {
		*selfNode
		Tag string `json:"tag"`
	}

	// 1. Direct pointer-embedded self cycle must terminate without stack overflow.
	sSelf := SchemaFromType(reflect.TypeOf(selfNode{}), nil)
	docSelf := mustSchemaDoc(t, sSelf)
	if err := ValidateDocumentAgainstSchema(docSelf, mustJSONRoundTrip(t, selfNode{Name: "root"})); err != nil {
		t.Fatalf("valid selfNode failed validation: %v", err)
	}
	// Planted negative: missing required "name" must fail.
	if err := ValidateDocumentAgainstSchema(docSelf, map[string]any{}); err == nil {
		t.Fatalf("expected missing required 'name' on selfNode to fail")
	}

	// 2. Mutual pointer-embedded cycle must terminate cleanly.
	sMutual := SchemaFromType(reflect.TypeOf(schemaTestMutualB{}), nil)
	docMutual := mustSchemaDoc(t, sMutual)
	if err := ValidateDocumentAgainstSchema(docMutual, mustJSONRoundTrip(t, schemaTestMutualB{B: "ok"})); err != nil {
		t.Fatalf("valid mutualB failed validation: %v", err)
	}

	// 3. Non-cyclic outer embedding cyclic inner must terminate cleanly and keep pointer-embedded fields optional.
	sWrap := SchemaFromType(reflect.TypeOf(outerWrap{}), nil)
	docWrap := mustSchemaDoc(t, sWrap)
	if err := ValidateDocumentAgainstSchema(docWrap, mustJSONRoundTrip(t, outerWrap{Tag: "t1"})); err != nil {
		t.Fatalf("outerWrap with nil embedded pointer failed validation: %v", err)
	}
	if err := ValidateDocumentAgainstSchema(docWrap, mustJSONRoundTrip(t, outerWrap{selfNode: &selfNode{Name: "n"}, Tag: "t2"})); err != nil {
		t.Fatalf("outerWrap with populated embedded pointer failed validation: %v", err)
	}
	// Planted negative: missing required "tag" must fail.
	if err := ValidateDocumentAgainstSchema(docWrap, map[string]any{"name": "n"}); err == nil {
		t.Fatalf("expected missing required 'tag' on outerWrap to fail")
	}
}

func TestSchemaFromType_FieldShadowingAndRequiredDedup(t *testing.T) {
	type baseEmbedded struct {
		ID     string `json:"id"`
		Shared string `json:"shared"`
		Kept   string `json:"kept"`
	}
	type outerAfter struct {
		baseEmbedded
		ID     int    `json:"id,omitempty"`
		Shared string `json:"shared"`
	}
	type outerBefore struct {
		ID     int    `json:"id,omitempty"`
		Shared string `json:"shared"`
		baseEmbedded
	}

	for name, typ := range map[string]reflect.Type{
		"outer_after_embedded":  reflect.TypeOf(outerAfter{}),
		"outer_before_embedded": reflect.TypeOf(outerBefore{}),
	} {
		t.Run(name, func(t *testing.T) {
			s := SchemaFromType(typ, nil)
			// Required must have unique items: "kept" and "shared" ONLY ("id" shadowed with omitempty).
			wantReq := []string{"kept", "shared"}
			if !reflect.DeepEqual(s.Required, wantReq) {
				t.Fatalf("required=%v, want %v", s.Required, wantReq)
			}
			if s.Properties["id"].Type != "integer" {
				t.Fatalf("outer int field must shadow embedded string field: got %v", s.Properties["id"].Type)
			}
			doc := mustSchemaDoc(t, s)

			// Valid payload omitting shadowed omitempty "id".
			if err := ValidateDocumentAgainstSchema(doc, map[string]any{"shared": "ok", "kept": "yes"}); err != nil {
				t.Fatalf("payload omitting shadowed omitempty id failed: %v", err)
			}
			// Valid payload including outer integer "id".
			if err := ValidateDocumentAgainstSchema(doc, map[string]any{"id": float64(7), "shared": "ok", "kept": "yes"}); err != nil {
				t.Fatalf("payload with outer integer id failed: %v", err)
			}
			// Planted negative: embedded string type for shadowed "id" must be rejected.
			if err := ValidateDocumentAgainstSchema(doc, map[string]any{"id": "wrong-type", "shared": "ok", "kept": "yes"}); err == nil {
				t.Fatalf("expected shadowed string id to fail integer schema")
			}
		})
	}

	// Verify real encoding/json marshaling of outerAfter and outerBefore conforms.
	if err := ValidateDocumentAgainstSchema(
		mustSchemaDoc(t, SchemaFromType(reflect.TypeOf(outerAfter{}), nil)),
		mustJSONRoundTrip(t, outerAfter{baseEmbedded: baseEmbedded{ID: "ignored", Shared: "x", Kept: "k"}, Shared: "s"}),
	); err != nil {
		t.Fatalf("real marshaled outerAfter failed validation: %v", err)
	}
	if err := ValidateDocumentAgainstSchema(
		mustSchemaDoc(t, SchemaFromType(reflect.TypeOf(outerBefore{}), nil)),
		mustJSONRoundTrip(t, outerBefore{ID: 9, Shared: "s", baseEmbedded: baseEmbedded{ID: "ignored", Kept: "k"}}),
	); err != nil {
		t.Fatalf("real marshaled outerBefore failed validation: %v", err)
	}

	// Ambiguous sibling fields at equal depth are omitted by encoding/json and SchemaFromType.
	type left struct {
		Dup string
	}
	type right struct {
		Dup string
	}
	type diamond struct {
		left
		right
		Own string `json:"own"`
	}
	sDiamond := SchemaFromType(reflect.TypeOf(diamond{}), nil)
	if _, exists := sDiamond.Properties["Dup"]; exists {
		t.Fatalf("ambiguous peer field 'Dup' should be omitted to match encoding/json")
	}
	if err := ValidateDocumentAgainstSchema(mustSchemaDoc(t, sDiamond), mustJSONRoundTrip(t, diamond{Own: "ok"})); err != nil {
		t.Fatalf("real marshaled diamond failed validation: %v", err)
	}
}

func TestSchemaFromType_NilByteSlice(t *testing.T) {
	type blobPayload struct {
		Data  []byte  `json:"data"`
		Fixed [4]byte `json:"fixed"`
	}
	s := SchemaFromType(reflect.TypeOf(blobPayload{}), nil)
	doc := mustSchemaDoc(t, s)

	// Nil []byte marshals to JSON null and must validate.
	nilBytesPayload := mustJSONRoundTrip(t, blobPayload{Data: nil, Fixed: [4]byte{1, 2, 3, 4}})
	if err := ValidateDocumentAgainstSchema(doc, nilBytesPayload); err != nil {
		t.Fatalf("nil []byte payload failed validation: %v", err)
	}

	// Non-nil []byte marshals to base64 string and must validate.
	nonNilPayload := mustJSONRoundTrip(t, blobPayload{Data: []byte("hello"), Fixed: [4]byte{1, 2, 3, 4}})
	if err := ValidateDocumentAgainstSchema(doc, nonNilPayload); err != nil {
		t.Fatalf("non-nil []byte payload failed validation: %v", err)
	}

	// Planted negative: integer for []byte must fail validation.
	if err := ValidateDocumentAgainstSchema(doc, map[string]any{"data": float64(42), "fixed": []any{float64(1), float64(2), float64(3), float64(4)}}); err == nil {
		t.Fatalf("expected integer 'data' to fail []byte nullable-string schema")
	}
}

func TestSchemaFromType_OmitZeroAndExactTagOptions(t *testing.T) {
	type tagPayload struct {
		ZeroTime  time.Time `json:"zero_time,omitzero"`
		ZeroInt   int       `json:"zero_int,omitzero"`
		NotOmit   string    `json:"not_omit,notomitempty"`
		MultiOpts string    `json:"multi_opts,string,omitempty"`
	}

	s := SchemaFromType(reflect.TypeOf(tagPayload{}), nil)
	wantReq := []string{"not_omit"}
	if !reflect.DeepEqual(s.Required, wantReq) {
		t.Fatalf("required=%v, want %v (omitzero must be optional; substring 'notomitempty' must remain required)", s.Required, wantReq)
	}
	doc := mustSchemaDoc(t, s)

	// Real marshaled zero struct omits zero_time, zero_int, multi_opts and keeps not_omit.
	if err := ValidateDocumentAgainstSchema(doc, mustJSONRoundTrip(t, tagPayload{NotOmit: "present"})); err != nil {
		t.Fatalf("marshaled tagPayload failed validation: %v", err)
	}
	// Planted negative: omitting "not_omit" must fail validation.
	if err := ValidateDocumentAgainstSchema(doc, map[string]any{}); err == nil {
		t.Fatalf("expected missing required 'not_omit' to fail validation")
	}
}

func TestMakeNullable_EnumAndConst(t *testing.T) {
	origEnum := EnumSchema("mode", "fast", "safe")
	nullEnum := makeNullable(origEnum)
	if len(origEnum.Enum) != 2 {
		t.Fatalf("makeNullable mutated original Enum slice: %v", origEnum.Enum)
	}
	docEnum := mustSchemaDoc(t, nullEnum)
	for _, valid := range []any{"fast", "safe", nil} {
		if err := ValidateDocumentAgainstSchema(docEnum, valid); err != nil {
			t.Fatalf("nullable enum rejected valid value %#v: %v", valid, err)
		}
	}
	// Planted negative: unknown value must still fail.
	if err := ValidateDocumentAgainstSchema(docEnum, "unknown"); err == nil {
		t.Fatalf("nullable enum accepted invalid value 'unknown'")
	}
	// Calling makeNullable twice must not duplicate nil in Enum.
	nullEnumTwice := makeNullable(nullEnum)
	if len(nullEnumTwice.Enum) != 3 {
		t.Fatalf("double makeNullable duplicated nil in Enum: %v", nullEnumTwice.Enum)
	}

	origConst := &JSONSchema{Type: "string", Const: "ok", Description: "Fixed status."}
	nullConst := makeNullable(origConst)
	if nullConst.Const != nil {
		t.Fatalf("nullable const must clear Const, got %#v", nullConst.Const)
	}
	docConst := mustSchemaDoc(t, nullConst)
	for _, valid := range []any{"ok", nil} {
		if err := ValidateDocumentAgainstSchema(docConst, valid); err != nil {
			t.Fatalf("nullable const rejected valid value %#v: %v", valid, err)
		}
	}
	// Planted negative: other string must fail.
	if err := ValidateDocumentAgainstSchema(docConst, "bad"); err == nil {
		t.Fatalf("nullable const accepted invalid value 'bad'")
	}
}

type CustomJSONStamp struct {
	InternalSecret string `json:"internal_secret"`
}

func (c CustomJSONStamp) MarshalJSON() ([]byte, error) {
	return json.Marshal("stamp:" + c.InternalSecret)
}

type CustomTextTokenA struct {
	HiddenA string `json:"hidden_a"`
}

func (c CustomTextTokenA) MarshalText() ([]byte, error) {
	return []byte("tokA:" + c.HiddenA), nil
}

type CustomTextTokenB struct {
	HiddenB string `json:"hidden_b"`
}

func (c CustomTextTokenB) MarshalText() ([]byte, error) {
	return []byte("tokB:" + c.HiddenB), nil
}

func TestSchemaFromType_JSONAndTextMarshalers(t *testing.T) {
	// 1. json.RawMessage must allow arbitrary JSON values (not base64 string).
	type rawEnvelope struct {
		Payload json.RawMessage `json:"payload"`
		Addr    netip.Addr      `json:"addr"`
		Stamp   CustomJSONStamp `json:"stamp"`
	}
	sEnv := SchemaFromType(reflect.TypeOf(rawEnvelope{}), nil)
	docEnv := mustSchemaDoc(t, sEnv)

	addr := netip.MustParseAddr("192.0.2.10")
	for _, rawVal := range []json.RawMessage{
		json.RawMessage(`{"nested":true}`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`42`),
		json.RawMessage(`"raw-str"`),
	} {
		v := mustJSONRoundTrip(t, rawEnvelope{
			Payload: rawVal,
			Addr:    addr,
			Stamp:   CustomJSONStamp{InternalSecret: "v1"},
		})
		if err := ValidateDocumentAgainstSchema(docEnv, v); err != nil {
			t.Fatalf("rawEnvelope with payload=%s failed validation: %v", string(rawVal), err)
		}
	}
	// Planted negative: non-string for encoding.TextMarshaler field (addr) must fail.
	if err := ValidateDocumentAgainstSchema(docEnv, map[string]any{
		"payload": map[string]any{"ok": true},
		"addr":    float64(12345),
		"stamp":   "stamp:v1",
	}); err == nil {
		t.Fatalf("expected numeric value for netip.Addr TextMarshaler to fail validation")
	}

	// 2. Single embedded TextMarshaler promotes MarshalText onto the outer struct,
	//    so encoding/json marshals the outer struct as a JSON string (never flattening fields).
	type singleEmbedText struct {
		CustomTextTokenA
		Extra string `json:"extra"`
	}
	sSingleText := SchemaFromType(reflect.TypeOf(singleEmbedText{}), nil)
	docSingleText := mustSchemaDoc(t, sSingleText)
	if err := ValidateDocumentAgainstSchema(docSingleText, mustJSONRoundTrip(t, singleEmbedText{
		CustomTextTokenA: CustomTextTokenA{HiddenA: "x"},
		Extra:            "ignored-by-promoted-MarshalText",
	})); err != nil {
		t.Fatalf("singleEmbedText failed validation: %v", err)
	}
	// Planted negative: an object with flattened internal fields must fail.
	if err := ValidateDocumentAgainstSchema(docSingleText, map[string]any{"hidden_a": "x", "extra": "y"}); err == nil {
		t.Fatalf("expected object payload to fail single-embedded TextMarshaler string schema")
	}

	// 3. Tagged embedded TextMarshaler is NOT flattened; it becomes a string property.
	type taggedEmbedText struct {
		CustomTextTokenA `json:"token_a"`
		CustomTextTokenB `json:"token_b"`
		Extra            string `json:"extra"`
	}
	sTagged := SchemaFromType(reflect.TypeOf(taggedEmbedText{}), nil)
	if _, leaked := sTagged.Properties["hidden_a"]; leaked {
		t.Fatalf("tagged embedded TextMarshaler leaked hidden_a into properties: %v", sTagged.Properties)
	}
	docTagged := mustSchemaDoc(t, sTagged)
	if err := ValidateDocumentAgainstSchema(docTagged, mustJSONRoundTrip(t, taggedEmbedText{
		CustomTextTokenA: CustomTextTokenA{HiddenA: "a"},
		CustomTextTokenB: CustomTextTokenB{HiddenB: "b"},
		Extra:            "ok",
	})); err != nil {
		t.Fatalf("taggedEmbedText failed validation: %v", err)
	}
	// Planted negative: object for tagged TextMarshaler must fail string schema.
	if err := ValidateDocumentAgainstSchema(docTagged, map[string]any{"token_a": map[string]any{"hidden_a": "a"}, "token_b": "tokB:b", "extra": "ok"}); err == nil {
		t.Fatalf("expected object payload for tagged TextMarshaler to fail string schema")
	}

	// 4. Dual untagged embedded TextMarshalers cancel MarshalText promotion in Go,
	//    so encoding/json traverses both embedded structs; SchemaFromType must match json.Marshal.
	type dualEmbedText struct {
		CustomTextTokenA
		CustomTextTokenB
		Extra string `json:"extra"`
	}
	docDual := mustSchemaDoc(t, SchemaFromType(reflect.TypeOf(dualEmbedText{}), nil))
	if err := ValidateDocumentAgainstSchema(docDual, mustJSONRoundTrip(t, dualEmbedText{
		CustomTextTokenA: CustomTextTokenA{HiddenA: "a"},
		CustomTextTokenB: CustomTextTokenB{HiddenB: "b"},
		Extra:            "ok",
	})); err != nil {
		t.Fatalf("dualEmbedText failed validation against real json.Marshal output: %v", err)
	}

	// 4. Single embedded json.Marshaler promotes MarshalJSON onto outer struct.
	type singleEmbedJSON struct {
		CustomJSONStamp
		Extra string `json:"extra"`
	}
	sSingleJSON := SchemaFromType(reflect.TypeOf(singleEmbedJSON{}), nil)
	if len(sSingleJSON.Properties) != 0 {
		t.Fatalf("embedded json.Marshaler must not flatten struct fields into Properties, got %v", sSingleJSON.Properties)
	}
	if err := ValidateDocumentAgainstSchema(mustSchemaDoc(t, sSingleJSON), mustJSONRoundTrip(t, singleEmbedJSON{
		CustomJSONStamp: CustomJSONStamp{InternalSecret: "s"},
		Extra:           "ignored",
	})); err != nil {
		t.Fatalf("singleEmbedJSON failed validation: %v", err)
	}
}

func TestMakeNullable_AnyOfAndOneOf(t *testing.T) {
	// 1. AnyOf becomes nullable without mutating original slice.
	origAny := &JSONSchema{
		Description: "string or int",
		AnyOf:       []*JSONSchema{StringSchema("s"), IntSchema("n")},
	}
	nullAny := makeNullable(origAny)
	if len(origAny.AnyOf) != 2 {
		t.Fatalf("makeNullable mutated original AnyOf slice: len=%d", len(origAny.AnyOf))
	}
	if len(nullAny.AnyOf) != 3 {
		t.Fatalf("expected 3 AnyOf branches after makeNullable, got %d", len(nullAny.AnyOf))
	}
	docAny := mustSchemaDoc(t, nullAny)
	for _, valid := range []any{"hello", float64(7), nil} {
		if err := ValidateDocumentAgainstSchema(docAny, valid); err != nil {
			t.Fatalf("nullable AnyOf rejected valid value %#v: %v", valid, err)
		}
	}
	// Planted negative: boolean must still fail.
	if err := ValidateDocumentAgainstSchema(docAny, true); err == nil {
		t.Fatalf("nullable AnyOf accepted invalid boolean")
	}
	// Idempotency: second makeNullable must not duplicate null branch.
	if twice := makeNullable(nullAny); len(twice.AnyOf) != 3 {
		t.Fatalf("double makeNullable duplicated null branch in AnyOf: len=%d", len(twice.AnyOf))
	}

	// 2. OneOf becomes nullable without adding duplicate null branches.
	origOne := &JSONSchema{
		Description: "string or int",
		OneOf:       []*JSONSchema{StringSchema("s"), IntSchema("n")},
	}
	nullOne := makeNullable(origOne)
	if len(origOne.OneOf) != 2 {
		t.Fatalf("makeNullable mutated original OneOf slice: len=%d", len(origOne.OneOf))
	}
	if len(nullOne.OneOf) != 3 {
		t.Fatalf("expected 3 OneOf branches after makeNullable, got %d", len(nullOne.OneOf))
	}
	docOne := mustSchemaDoc(t, nullOne)
	for _, valid := range []any{"hello", float64(7), nil} {
		if err := ValidateDocumentAgainstSchema(docOne, valid); err != nil {
			t.Fatalf("nullable OneOf rejected valid value %#v: %v", valid, err)
		}
	}
	// Planted negative: boolean must fail.
	if err := ValidateDocumentAgainstSchema(docOne, true); err == nil {
		t.Fatalf("nullable OneOf accepted invalid boolean")
	}
	// Idempotency: second makeNullable must NOT add a second null branch (which would break oneOf!).
	twiceOne := makeNullable(nullOne)
	if len(twiceOne.OneOf) != 3 {
		t.Fatalf("double makeNullable duplicated null branch in OneOf: len=%d", len(twiceOne.OneOf))
	}
	if err := ValidateDocumentAgainstSchema(mustSchemaDoc(t, twiceOne), nil); err != nil {
		t.Fatalf("double makeNullable broke oneOf null matching: %v", err)
	}

	// 3. OneOf where a branch already accepts null must not add another null branch.
	alreadyNullOne := &JSONSchema{
		Description: "nullable string or int",
		OneOf:       []*JSONSchema{NullableStringSchema("s"), IntSchema("n")},
	}
	afterNullOne := makeNullable(alreadyNullOne)
	if len(afterNullOne.OneOf) != 2 {
		t.Fatalf("makeNullable added redundant null branch to already-nullable OneOf: len=%d", len(afterNullOne.OneOf))
	}
	if err := ValidateDocumentAgainstSchema(mustSchemaDoc(t, afterNullOne), nil); err != nil {
		t.Fatalf("already-nullable OneOf failed null after makeNullable: %v", err)
	}

	// 4. Nil input and []any Type slice.
	if got := makeNullable(nil); got.Type != "null" {
		t.Fatalf("makeNullable(nil).Type=%v, want null", got.Type)
	}
	anyTypeSchema := makeNullable(&JSONSchema{Type: []any{"string"}})
	if err := ValidateDocumentAgainstSchema(mustSchemaDoc(t, anyTypeSchema), nil); err != nil {
		t.Fatalf("[]any Type schema failed null after makeNullable: %v", err)
	}
}

func mustSchemaDoc(t *testing.T, s *JSONSchema) map[string]any {
	t.Helper()
	raw, err := MarshalJSONSchema(s)
	if err != nil {
		t.Fatalf("MarshalJSONSchema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return doc
}

func mustJSONRoundTrip(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return out
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
