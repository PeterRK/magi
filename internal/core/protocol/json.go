package protocol

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Retain exact number spellings instead of v2's default float64 conversion.
// All other values use v2's strict decoder, including duplicate names, UTF-8,
// surrogate pairs and trailing input. Serialization remains unchanged.
var exactJSONNumbers = jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(func(d *jsontext.Decoder, value *any) error {
	if d.PeekKind() != '0' {
		return errors.ErrUnsupported
	}
	token, err := d.ReadToken()
	if err == nil {
		*value = json.Number(token.String())
	}
	return err
}))

func DecodeJSON(body []byte) (any, error) {
	var value any
	if err := jsonv2.Unmarshal(body, &value, exactJSONNumbers); err != nil {
		return nil, err
	}
	return value, nil
}

// UnmarshalJSON strictly decodes typed data without rewriting RawMessage fields.
// Field names match JSON tags exactly; unknown fields remain allowed for provider
// extensions. Stored records enforce their required fields separately.
func UnmarshalJSON(body []byte, value any) error {
	return jsonv2.Unmarshal(body, value)
}

// normalizeJSON updates the freshly decoded argument tree in place.
func normalizeJSON(value any) any {
	switch v := value.(type) {
	case json.Number:
		return json.Number(jsonDecimal(v.String()).canonical())
	case map[string]any:
		for key, child := range v {
			v[key] = normalizeJSON(child)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = normalizeJSON(child)
		}
		return v
	}
	return value
}

type decimalJSON struct {
	negative bool
	digits   string
	exponent *big.Int
}

func jsonDecimal(raw string) decimalJSON {
	d := decimalJSON{exponent: new(big.Int)}
	if strings.HasPrefix(raw, "-") {
		d.negative, raw = true, raw[1:]
	}
	mantissa, exponent, exists := strings.Cut(strings.ToLower(raw), "e")
	if exists {
		d.exponent.SetString(exponent, 10)
	}
	fraction := 0
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		fraction = len(mantissa) - dot - 1
		mantissa = mantissa[:dot] + mantissa[dot+1:]
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		d.digits, d.negative = "0", false
		d.exponent.SetInt64(0)
		return d
	}
	d.digits = strings.TrimRight(mantissa, "0")
	d.exponent.Add(d.exponent, big.NewInt(int64(len(mantissa)-len(d.digits)-fraction)))
	return d
}

func (d decimalJSON) canonical() string {
	sign := ""
	if d.negative {
		sign = "-"
	}
	if d.digits == "0" {
		return "0"
	}
	// All native integer fields fit uint64. Do not expand hostile exponents.
	if d.exponent.IsInt64() && d.exponent.Int64() >= 0 && d.exponent.Int64() <= 20 && len(d.digits)+int(d.exponent.Int64()) <= 20 {
		return sign + d.digits + strings.Repeat("0", int(d.exponent.Int64()))
	}
	return sign + d.digits + "e" + d.exponent.String()
}

func compareJSONNumbers(left, right json.Number) int {
	a, b := jsonDecimal(left.String()), jsonDecimal(right.String())
	if a.digits == "0" && b.digits == "0" {
		return 0
	}
	if a.negative != b.negative {
		if a.negative {
			return -1
		}
		return 1
	}
	sign := 1
	if a.negative {
		sign = -1
	}
	if a.digits == "0" {
		return -sign
	}
	if b.digits == "0" {
		return sign
	}
	am := new(big.Int).Add(a.exponent, big.NewInt(int64(len(a.digits))))
	bm := new(big.Int).Add(b.exponent, big.NewInt(int64(len(b.digits))))
	if c := am.Cmp(bm); c != 0 {
		return sign * c
	}
	width := max(len(a.digits), len(b.digits))
	return sign * strings.Compare(a.digits+strings.Repeat("0", width-len(a.digits)), b.digits+strings.Repeat("0", width-len(b.digits)))
}

func parseJSONSchema(schema []byte) (map[string]any, error) {
	raw, err := DecodeJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	rule, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be an object")
	}
	return rule, nil
}

// decodeValidatedJSON reuses embedded rules and retains the parsed arguments for
// normalization. Changed or caller-supplied schemas are parsed independently.
func decodeValidatedJSON(schema, body []byte) (any, error) {
	rules, err := contractSchemas()
	if err != nil {
		return nil, err
	}
	rule, ok := rules[string(schema)]
	if !ok {
		rule, err = parseJSONSchema(schema)
		if err != nil {
			return nil, err
		}
	}
	value, err := DecodeJSON(body)
	if err != nil {
		var syntax *jsontext.SyntacticError
		if errors.As(err, &syntax) {
			return nil, JSONSchemaViolation{Path: schemaJSONPath(rule, syntax.JSONPointer), Reason: fmt.Sprintf("invalid JSON syntax at byte offset %d; %s", syntax.ByteOffset, jsonSyntaxHint(syntax))}
		}
		return nil, err
	}
	if err := checkJSONSchema(rule, value, "$"); err != nil {
		return nil, err
	}
	return value, nil
}

// JSONSchemaViolation contains only a schema path and a fixed validation reason,
// never argument values or caller-defined object keys. It is safe to return to
// the model as actionable feedback for its rejected call.
type JSONSchemaViolation struct {
	Path   string
	Reason string
}

func (e JSONSchemaViolation) Error() string { return e.Path + ": " + e.Reason }

// Raw v2 diagnostics can contain argument bytes and caller-defined keys.
// Translate them to fixed guidance, retaining only the offset and schema path.
func jsonSyntaxHint(err *jsontext.SyntacticError) string {
	message := err.Err.Error()
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "incomplete JSON value; finish the value and close its strings, objects and arrays"
	case errors.Is(err, jsontext.ErrDuplicateName):
		return "duplicate object property; include each property only once"
	case errors.Is(err, jsontext.ErrNonStringName):
		return "object property names must be quoted JSON strings"
	case strings.Contains(message, "after top-level value"):
		return "extra data after the complete JSON value; remove trailing characters and submit exactly one value"
	case strings.Contains(message, "after object name"):
		return "expected ':' between the property name and its value"
	case strings.Contains(message, "after object value"):
		return "expected ',' between properties or '}' to close the object"
	case strings.Contains(message, "after array element"):
		return "expected ',' between elements or ']' to close the array"
	case strings.Contains(message, "escape sequence") || strings.Contains(message, "surrogate"):
		return "invalid string escape; use JSON escapes and paired Unicode surrogates"
	case strings.Contains(message, "UTF-8"):
		return "invalid UTF-8; supply valid Unicode text"
	case strings.Contains(message, "max depth"):
		return "JSON nesting is too deep; reduce object and array nesting"
	case strings.Contains(message, "in string"):
		return "invalid string character; escape quotes, backslashes and control characters"
	case strings.Contains(message, "in number"):
		return "invalid JSON number; use a complete decimal number without leading zeros"
	default:
		return "invalid JSON token; use quoted property names and valid JSON values"
	}
}

func schemaJSONPath(rule map[string]any, pointer jsontext.Pointer) string {
	path := "$"
	for token := range pointer.Tokens() {
		if item, ok := rule["items"].(map[string]any); ok {
			index, err := strconv.ParseUint(token, 10, 64)
			if err != nil {
				break
			}
			path += fmt.Sprintf("[%d]", index)
			rule = item
			continue
		}
		properties, _ := rule["properties"].(map[string]any)
		child, known := properties[token].(map[string]any)
		if !known {
			return path + ".<additional property>"
		}
		path += "." + token
		rule = child
	}
	return path
}

func schemaValueHint(label string, value any) string {
	if body, err := json.Marshal(value); err == nil && len(body) <= 160 {
		return "; " + label + ": " + string(body)
	}
	return "; see the tool schema for " + label
}

// checkJSONSchema covers the fixed tool contracts, not general JSON Schema.
func checkJSONSchema(rule map[string]any, value any, path string) error {
	fail := func(message string) error { return JSONSchemaViolation{Path: path, Reason: message} }
	if allowed, ok := rule["enum"].([]any); ok {
		// Tool enums contain strings only.
		text, ok := value.(string)
		found := false
		for _, candidate := range allowed {
			if ok && candidate == text {
				found = true
				break
			}
		}
		if !found {
			return fail("value is not in enum" + schemaValueHint("allowed values", allowed))
		}
	}
	if kind, _ := rule["type"].(string); kind != "" {
		valid := false
		switch kind {
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "integer":
			n, ok := value.(json.Number)
			valid = ok && jsonDecimal(n.String()).exponent.Sign() >= 0
		default:
			return fail("unsupported schema type")
		}
		if !valid {
			if kind == "object" && path == "$" {
				return fail("expected object; pass tool parameters directly, not a JSON-encoded string or an arguments wrapper")
			}
			return fail("expected " + kind)
		}
	}
	checkLength := func(length int, minimum, maximum string) error {
		actual := json.Number(strconv.Itoa(length))
		if n, ok := rule[minimum].(json.Number); ok && compareJSONNumbers(actual, n) < 0 {
			return fail("less than " + minimum + " (" + n.String() + ")")
		}
		if n, ok := rule[maximum].(json.Number); ok && compareJSONNumbers(actual, n) > 0 {
			return fail("greater than " + maximum + " (" + n.String() + ")")
		}
		return nil
	}
	switch v := value.(type) {
	case json.Number:
		for _, bound := range []struct {
			name    string
			minimum bool
		}{{"minimum", true}, {"maximum", false}} {
			if n, ok := rule[bound.name].(json.Number); ok {
				c := compareJSONNumbers(v, n)
				if (bound.minimum && c < 0) || (!bound.minimum && c > 0) {
					return fail("outside " + bound.name + " (" + n.String() + ")")
				}
			}
		}
	case string:
		if err := checkLength(utf8.RuneCountInString(v), "minLength", "maxLength"); err != nil {
			return err
		}
		if pattern, ok := rule["pattern"].(string); ok {
			matched, err := regexp.MatchString(pattern, v)
			if err != nil || !matched {
				return fail("pattern mismatch" + schemaValueHint("required pattern", pattern))
			}
		}
	case []any:
		if err := checkLength(len(v), "minItems", "maxItems"); err != nil {
			return err
		}
		if child, ok := rule["items"].(map[string]any); ok {
			for i, item := range v {
				if err := checkJSONSchema(child, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		properties, _ := rule["properties"].(map[string]any)
		if required, ok := rule["required"].([]any); ok {
			for _, item := range required {
				name, ok := item.(string)
				if !ok {
					return fmt.Errorf("invalid required schema property")
				}
				if _, found := v[name]; !found {
					reason := "missing required property"
					if _, wrapped := v["arguments"]; path == "$" && wrapped {
						reason += "; pass tool parameters directly at the root, without an arguments wrapper"
					}
					return JSONSchemaViolation{Path: path + "." + name, Reason: reason}
				}
			}
		}
		// Keep the first reported failure stable across retries and replay.
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			item := v[name]
			child, known := properties[name].(map[string]any)
			if !known {
				if allowed, ok := rule["additionalProperties"].(bool); ok && !allowed {
					allowedNames := make([]string, 0, len(properties))
					for name := range properties {
						allowedNames = append(allowedNames, name)
					}
					sort.Strings(allowedNames)
					return fail("unknown property; use only properties declared in the schema" + schemaValueHint("allowed properties", allowedNames))
				}
			}
			if known {
				if err := checkJSONSchema(child, item, path+"."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
