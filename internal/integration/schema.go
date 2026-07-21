package integration

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const inlineSchemaURL = "https://convo-relay.invalid/inline-schema.json"

var supportedSchemaKeywords = []string{
	"type",
	"required",
	"properties",
	"items",
	"enum",
	"const",
	"minLength",
	"maxLength",
	"minItems",
	"maxItems",
	"minimum",
	"maximum",
	"exclusiveMinimum",
	"exclusiveMaximum",
	"additionalProperties",
	"oneOf",
	"allOf",
	"if",
	"then",
	"else",
	"$defs",
	"$ref",
}

var supportedSchemaTypes = map[string]bool{
	"null":    true,
	"boolean": true,
	"object":  true,
	"array":   true,
	"number":  true,
	"integer": true,
	"string":  true,
}

// CompiledSchema holds a normalized inline schema and its Draft 2020-12
// validator. Its document is cloned on construction and access.
type CompiledSchema struct {
	document  map[string]any
	validator *jsonschema.Schema
}

type schemaReference struct {
	path string
	ref  string
}

func CompileSchema(value any, path string) (*CompiledSchema, error) {
	document, ok := value.(map[string]any)
	if !ok {
		return nil, preflightError(
			DiagnosticCodeInvalidSchema,
			path,
			"Inline JSON Schema must be an object; standalone boolean schemas are not supported.",
			nil,
		)
	}
	document = cloneMap(document)
	refs := make([]schemaReference, 0)
	schemaPositions := make(map[string]bool)
	if err := validateSchemaObject(document, path, "", &refs, schemaPositions); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if err := validateSchemaReference(document, ref, schemaPositions); err != nil {
			return nil, err
		}
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(disabledSchemaLoader{})
	if err := compiler.AddResource(inlineSchemaURL, document); err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeInvalidSchema,
			path,
			"Inline JSON Schema could not be registered.",
			map[string]any{"error": err.Error()},
		)
	}
	validator, err := compiler.Compile(inlineSchemaURL)
	if err != nil {
		return nil, wrapPreflightError(
			err,
			DiagnosticCodeInvalidSchema,
			path,
			"Inline JSON Schema is invalid.",
			map[string]any{"error": err.Error()},
		)
	}
	return &CompiledSchema{document: document, validator: validator}, nil
}

func (s *CompiledSchema) Document() map[string]any {
	if s == nil {
		return nil
	}
	return cloneMap(s.document)
}

func (s *CompiledSchema) Validate(value any) error {
	if s == nil || s.validator == nil {
		return preflightError(DiagnosticCodeInvalidSchema, "", "Compiled JSON Schema is required.", nil)
	}
	if err := s.validator.Validate(contracts.Materialize(value)); err != nil {
		path := validationErrorPath(err)
		diagnostic := contracts.NewDiagnostic(
			DiagnosticCodeSchemaMismatch,
			contracts.DiagnosticPhaseSchema,
			path,
			"JSON value does not match the declared schema.",
			map[string]any{"error": err.Error()},
		)
		return contracts.WrapDiagnosticError(err, "JSON value does not match the declared schema.", diagnostic)
	}
	return nil
}

type disabledSchemaLoader struct{}

func (disabledSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema loading is disabled: %s", url)
}

func validateSchemaObject(schema map[string]any, path string, pointer string, refs *[]schemaReference, schemaPositions map[string]bool) error {
	schemaPositions[pointer] = true
	if err := rejectUnknownFields(schema, supportedSchemaKeywords, path, "JSON Schema", DiagnosticCodeUnsupportedSchema); err != nil {
		return err
	}
	for _, keyword := range sortedKeys(schema) {
		value := schema[keyword]
		keywordPath := appendPointer(path, keyword)
		keywordPointer := appendPointer(pointer, keyword)
		switch keyword {
		case "type":
			if err := validateSchemaType(value, keywordPath); err != nil {
				return err
			}
		case "required":
			items, ok := value.([]any)
			if !ok {
				return invalidSchemaValue(keywordPath, "required must be an array of unique strings.")
			}
			seen := map[string]bool{}
			for index, item := range items {
				name, ok := item.(string)
				if !ok || seen[name] {
					return invalidSchemaValue(appendPointer(keywordPath, strconv.Itoa(index)), "required must contain unique strings.")
				}
				seen[name] = true
			}
		case "properties", "$defs":
			children, ok := value.(map[string]any)
			if !ok {
				return invalidSchemaValue(keywordPath, keyword+" must be an object.")
			}
			for _, name := range sortedKeys(children) {
				child := children[name]
				childPath := appendPointer(keywordPath, name)
				childPointer := appendPointer(keywordPointer, name)
				childSchema, ok := child.(map[string]any)
				if !ok {
					return invalidSchemaValue(childPath, "Schema positions must contain schema objects, not standalone booleans or other values.")
				}
				if err := validateSchemaObject(childSchema, childPath, childPointer, refs, schemaPositions); err != nil {
					return err
				}
			}
		case "items", "if", "then", "else":
			childSchema, ok := value.(map[string]any)
			if !ok {
				return invalidSchemaValue(keywordPath, "Schema positions must contain schema objects, not standalone booleans or other values.")
			}
			if err := validateSchemaObject(childSchema, keywordPath, keywordPointer, refs, schemaPositions); err != nil {
				return err
			}
		case "enum":
			items, ok := value.([]any)
			if !ok || len(items) == 0 {
				return invalidSchemaValue(keywordPath, "enum must be a non-empty array.")
			}
		case "const":
			// Every JSON value is valid for const.
		case "minLength", "maxLength", "minItems", "maxItems":
			if _, ok := nonNegativeJSONInteger(value); !ok {
				return invalidSchemaValue(keywordPath, keyword+" must be a non-negative integer.")
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
			if !isJSONNumber(value) {
				return invalidSchemaValue(keywordPath, keyword+" must be a number.")
			}
		case "additionalProperties":
			switch typed := value.(type) {
			case bool:
			case map[string]any:
				if err := validateSchemaObject(typed, keywordPath, keywordPointer, refs, schemaPositions); err != nil {
					return err
				}
			default:
				return invalidSchemaValue(keywordPath, "additionalProperties must be a boolean or a supported schema object.")
			}
		case "oneOf", "allOf":
			items, ok := value.([]any)
			if !ok || len(items) == 0 {
				return invalidSchemaValue(keywordPath, keyword+" must be a non-empty array of schema objects.")
			}
			for index, item := range items {
				itemPath := appendPointer(keywordPath, strconv.Itoa(index))
				itemPointer := appendPointer(keywordPointer, strconv.Itoa(index))
				childSchema, ok := item.(map[string]any)
				if !ok {
					return invalidSchemaValue(itemPath, "Schema positions must contain schema objects, not standalone booleans or other values.")
				}
				if err := validateSchemaObject(childSchema, itemPath, itemPointer, refs, schemaPositions); err != nil {
					return err
				}
			}
		case "$ref":
			ref, ok := value.(string)
			if !ok || !strings.HasPrefix(ref, "#/$defs/") {
				return preflightError(
					DiagnosticCodeInvalidSchemaReference,
					keywordPath,
					"Schema references must be local #/$defs/... references.",
					map[string]any{"ref": value},
				)
			}
			*refs = append(*refs, schemaReference{path: keywordPath, ref: ref})
		}
	}
	return nil
}

func validateSchemaType(value any, path string) error {
	validate := func(name string, itemPath string) error {
		if !supportedSchemaTypes[name] {
			return invalidSchemaValue(itemPath, fmt.Sprintf("unsupported JSON Schema type %q.", name))
		}
		return nil
	}
	switch typed := value.(type) {
	case string:
		return validate(typed, path)
	case []any:
		if len(typed) == 0 {
			return invalidSchemaValue(path, "type must be a string or a non-empty array of unique strings.")
		}
		seen := map[string]bool{}
		for index, item := range typed {
			name, ok := item.(string)
			itemPath := appendPointer(path, strconv.Itoa(index))
			if !ok || seen[name] {
				return invalidSchemaValue(itemPath, "type must contain unique strings.")
			}
			if err := validate(name, itemPath); err != nil {
				return err
			}
			seen[name] = true
		}
		return nil
	default:
		return invalidSchemaValue(path, "type must be a string or a non-empty array of unique strings.")
	}
}

func validateSchemaReference(root map[string]any, reference schemaReference, schemaPositions map[string]bool) error {
	segments, err := decodeLocalReference(reference.ref)
	if err != nil {
		return wrapPreflightError(
			err,
			DiagnosticCodeInvalidSchemaReference,
			reference.path,
			"Schema reference is not a valid local JSON Pointer.",
			map[string]any{"ref": reference.ref},
		)
	}
	var current any = root
	targetPointer := ""
	for _, segment := range segments {
		targetPointer = appendPointer(targetPointer, segment)
		switch typed := current.(type) {
		case map[string]any:
			value, exists := typed[segment]
			if !exists {
				return unresolvedSchemaReference(reference)
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return unresolvedSchemaReference(reference)
			}
			current = typed[index]
		default:
			return unresolvedSchemaReference(reference)
		}
	}
	if _, ok := current.(map[string]any); !ok || !schemaPositions[targetPointer] {
		return unresolvedSchemaReference(reference)
	}
	return nil
}

func decodeLocalReference(ref string) ([]string, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("reference must begin with #/")
	}
	fragment, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
	if err != nil {
		return nil, fmt.Errorf("invalid percent escape: %w", err)
	}
	rawSegments := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	segments := make([]string, len(rawSegments))
	for index, raw := range rawSegments {
		var builder strings.Builder
		for offset := 0; offset < len(raw); offset++ {
			if raw[offset] != '~' {
				builder.WriteByte(raw[offset])
				continue
			}
			if offset+1 >= len(raw) {
				return nil, fmt.Errorf("incomplete JSON Pointer escape")
			}
			offset++
			switch raw[offset] {
			case '0':
				builder.WriteByte('~')
			case '1':
				builder.WriteByte('/')
			default:
				return nil, fmt.Errorf("invalid JSON Pointer escape")
			}
		}
		segments[index] = builder.String()
	}
	return segments, nil
}

func unresolvedSchemaReference(reference schemaReference) error {
	return preflightError(
		DiagnosticCodeInvalidSchemaReference,
		reference.path,
		"Schema reference does not resolve to a local schema object.",
		map[string]any{"ref": reference.ref},
	)
}

func invalidSchemaValue(path string, message string) error {
	return preflightError(DiagnosticCodeInvalidSchema, path, message, nil)
}

func nonNegativeJSONInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		return parsed, err == nil && parsed >= 0
	case int:
		return int64(typed), typed >= 0
	case int8:
		return int64(typed), typed >= 0
	case int16:
		return int64(typed), typed >= 0
	case int32:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func isJSONNumber(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		_, ok := new(big.Rat).SetString(typed.String())
		return ok
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return !float32IsInvalid(typed)
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0)
	default:
		return false
	}
}

func float32IsInvalid(value float32) bool {
	return math.IsNaN(float64(value)) || math.IsInf(float64(value), 0)
}

func validationErrorPath(err error) string {
	validationError, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return ""
	}
	for len(validationError.Causes) > 0 {
		validationError = validationError.Causes[0]
	}
	path := ""
	for _, segment := range validationError.InstanceLocation {
		path = appendPointer(path, segment)
	}
	return path
}
