package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStoredJSONPreservesNestedReceiptRepresentation(t *testing.T) {
	body := []byte(`{"result":{"z":1,"a":2},"arguments":"{ \"x\": 1.0 }"}`)
	var value struct {
		Result    json.RawMessage `json:"result"`
		Arguments string          `json:"arguments"`
	}
	if err := UnmarshalJSON(body, &value); err != nil {
		t.Fatal(err)
	}
	if string(value.Result) != `{"z":1,"a":2}` || value.Arguments != `{ "x": 1.0 }` {
		t.Fatalf("stored native material was rewritten: %+v", value)
	}
}

func TestEmbeddedSchemaChangesRemainIndependent(t *testing.T) {
	schema, err := Contract("read")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"path":"source.go"}`)
	if _, err := decodeValidatedJSON(schema, body); err != nil {
		t.Fatal(err)
	}
	var modified map[string]any
	if err := json.Unmarshal(schema, &modified); err != nil {
		t.Fatal(err)
	}
	modified["properties"].(map[string]any)["path"] = map[string]any{"type": "boolean"}
	changed, err := json.Marshal(modified)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeValidatedJSON(changed, body); err == nil {
		t.Fatal("changed schema reused the original rules")
	}
	if _, err := decodeValidatedJSON(changed, []byte(`{"path":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeValidatedJSON(schema, body); err != nil {
		t.Fatal("changed schema affected the embedded rules", err)
	}
}

func TestJSONRejectsAmbiguousOrLossyInput(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"a":1,"\u0061":2}`), []byte(`{"a":[{"x":1,"x":2}]}`),
		[]byte(`{"raw":{"x":1,"x":2}}`), []byte(`{"raw":"\ud800"}`),
		[]byte(`"\ud800"`), []byte(`"\udfff"`), []byte(`"\ud800\u1234"`),
		{'"', 0xff, '"'}, []byte(`{} {}`), []byte(`NaN`),
	} {
		if value, err := DecodeJSON(body); err == nil || value != nil {
			t.Fatalf("accepted or exposed partially decoded ambiguous/lossy input %q: %v", body, err)
		}
		var stored any
		var opaque struct {
			Raw json.RawMessage `json:"raw"`
		}
		for _, target := range []any{&stored, &opaque} {
			if err := UnmarshalJSON(body, target); err == nil {
				t.Fatalf("accepted ambiguous/lossy stored input %q", body)
			}
		}
	}
	for _, body := range []string{`"\ud83d\ude80"`, `"\\ud800"`, `"�"`, `{"汉字":"é\r\n"}`} {
		if _, err := DecodeJSON([]byte(body)); err != nil {
			t.Fatalf("rejected valid Unicode %s: %v", body, err)
		}
	}
}

func TestJSONSyntaxFeedbackRejectsInvalidUTF8WithoutReplacement(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`)
	body := append([]byte(`{"text":"private-value`), 0xff)
	body = append(body, []byte(`"}`)...)
	_, err := decodeValidatedJSON(schema, body)
	var violation JSONSchemaViolation
	if !errors.As(err, &violation) || violation.Path != "$.text" || !strings.Contains(violation.Reason, "invalid UTF-8") || !strings.Contains(violation.Reason, "byte offset 22") {
		t.Fatalf("missing v2 location or Unicode diagnosis: %v", err)
	}
	if strings.Contains(violation.Error(), "private-value") {
		t.Fatal("syntax feedback exposed argument content")
	}
}

func TestJSONSchemaExactIntegerBounds(t *testing.T) {
	schema := []byte(`{"type":"integer","minimum":0,"maximum":9007199254740993}`)
	for _, body := range []string{"9007199254740993.0", "0e9999999999999999999999", "1e2"} {
		if _, err := decodeValidatedJSON(schema, []byte(body)); err != nil {
			t.Fatalf("valid integer rejected: %s: %v", body, err)
		}
	}
	for _, body := range []string{"9007199254740994", "1e9999999999999999999999", "-1", "-1e-9999999999999999999999", "0.1"} {
		if _, err := decodeValidatedJSON(schema, []byte(body)); err == nil {
			t.Fatalf("invalid integer accepted: %s", body)
		}
	}
}

type storedDecoder struct{ body string }

func (d *storedDecoder) UnmarshalJSON(body []byte) error {
	d.body = string(body)
	var nested any
	return UnmarshalJSON(body, &nested)
}

func TestJSONSchemaNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"integer", "1", true}, {"decimal-integral", "1.00", true}, {"exponent-integral", "1.5e1", true},
		{"exponent-fraction", "1e-1", false}, {"negative-zero", "-0.00", true}, {"large-integer", "9007199254740993", true},
		{"large-fraction", "9007199254740992.5", false}, {"tiny-fraction", "1e-999999999999999999999", false},
		{"huge-integral", "1e999999999999999999999", true}, {"zero-exponent", "0e-999999999999999999999", true},
		{"trailing-document", "1 2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeValidatedJSON([]byte(`{"type":"integer"}`), []byte(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("%s: %v", tc.body, err)
			}
		})
	}
}

func TestStoredJSONDecodesDestination(t *testing.T) {
	body := []byte(`{"n":1}`)
	var value struct {
		N int `json:"n"`
	}
	if err := UnmarshalJSON(body, &value); err != nil || value.N != 1 {
		t.Fatalf("value=%+v error=%v", value, err)
	}
	if err := UnmarshalJSON([]byte(`{"N":2,"extension":true}`), &value); err != nil || value.N != 1 {
		t.Fatalf("field matching or unknown-field handling changed: %+v, %v", value, err)
	}
	var incompatible struct {
		N string `json:"n"`
	}
	if err := UnmarshalJSON(body, &incompatible); err == nil {
		t.Fatal("accepted incompatible destination")
	}
	var custom storedDecoder
	if err := UnmarshalJSON(body, &custom); err != nil || custom.body != string(body) {
		t.Fatalf("custom decoder body=%q error=%v", custom.body, err)
	}
}
