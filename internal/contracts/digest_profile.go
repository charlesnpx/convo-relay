package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type DigestClass string

const (
	DigestClassRawBytes        DigestClass = "raw-bytes"
	DigestClassSemanticJSON    DigestClass = "semantic-json"
	DigestClassStorageEnvelope DigestClass = "storage-envelope"
)

var semanticNumberPattern = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// Runtime-only storage fields are excluded by exact JSON Pointer. An empty
// list is intentional: most root artifacts bind their complete envelope.
var storageEnvelopeExclusions = map[string][]string{
	RootArtifactKindRootRecipePlan:      {},
	RootArtifactKindIntegrationBundle:   {},
	RootArtifactKindIntegrationContract: {},
	RootArtifactKindNamedInputManifest:  {},
	RootArtifactKindNamedInputContent:   {},
	RootArtifactKindRetainedInputs:      {},
	RootArtifactKindExecutionWorkspace: {
		"/identity",
		"/source/git_root",
		"/source/launch_cwd",
		"/source_after/git_root",
		"/source_after/launch_cwd",
	},
	RootArtifactKindRootCheckpoint:     {},
	RootArtifactKindReducerAttempt:     {},
	RootArtifactKindRawResult:          {},
	RootArtifactKindResultValidation:   {},
	RootArtifactKindCanonicalResult:    {},
	RootArtifactKindRenderedPrompt:     {},
	RootArtifactKindProviderInvocation: {},
	RootArtifactKindProviderResult:     {},
	RootArtifactKindIsolationReport:    {},
}

func RawBytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// SemanticJSONBytes implements the canonical JSON form published as
// relay-root-digests-v1. It differs from the released v1 canonical form only
// for successor payloads: numbers are normalized by exact decimal value and
// no member names receive special treatment.
func SemanticJSONBytes(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := appendSemanticJSON(&buffer, Materialize(value)); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func SemanticJSONDigest(value any) (string, error) {
	canonical, err := SemanticJSONBytes(value)
	if err != nil {
		return "", err
	}
	return RawBytesDigest(canonical), nil
}

// SemanticJSONDigestBytes is the untrusted-byte entry point. Strict decoding
// rejects invalid UTF-8, duplicate keys, and trailing values before hashing.
func SemanticJSONDigestBytes(data []byte) (string, error) {
	value, err := DecodeStrictJSONBytes(data)
	if err != nil {
		return "", err
	}
	return SemanticJSONDigest(value)
}

func StorageEnvelopeDigest(kind string, value any) (string, error) {
	exclusions, ok := storageEnvelopeExclusions[kind]
	if !ok {
		return "", NewValidationError("digest profile %s has no storage-envelope projection for kind %q", DigestProfileV1, kind)
	}
	projected, ok := Materialize(value).(map[string]any)
	if !ok {
		return "", NewValidationError("storage-envelope payload for %s must be an object", kind)
	}
	for _, pointer := range exclusions {
		// The first profile has one exclusion. Keep this dispatch explicit so a
		// future exclusion cannot silently become a recursive name rule.
		switch pointer {
		case "/identity":
			delete(projected, "identity")
		case "/source/git_root":
			deleteNestedMember(projected, "source", "git_root")
		case "/source/launch_cwd":
			deleteNestedMember(projected, "source", "launch_cwd")
		case "/source_after/git_root":
			deleteNestedMember(projected, "source_after", "git_root")
		case "/source_after/launch_cwd":
			deleteNestedMember(projected, "source_after", "launch_cwd")
		default:
			return "", NewValidationError("unsupported storage-envelope exclusion %q", pointer)
		}
	}
	return SemanticJSONDigest(projected)
}

func deleteNestedMember(object map[string]any, parent string, member string) {
	nested, _ := object[parent].(map[string]any)
	delete(nested, member)
}

// PayloadDigest preserves the released ContractDigest for v1 and dispatches
// only explicit successor payloads to relay-root-digests-v1.
func PayloadDigest(value any) (string, error) {
	object, ok := Materialize(value).(map[string]any)
	if !ok {
		return ContractDigest(value)
	}
	if declared, exists := object["digest_profile"]; exists && declared != DigestProfileV1 {
		return "", NewValidationError("unsupported digest profile %q", declared)
	}
	kind, _ := object["kind"].(string)
	version, numericVersion := exactJSONInteger(object["schema_version"])
	if numericVersion && version == RootArtifactSchemaVersionV2 {
		if _, rootKind := rootArtifactSpecForKind(kind); rootKind {
			if object["digest_profile"] != DigestProfileV1 {
				return "", NewValidationError("%s schema_version 2 requires digest_profile %s", kind, DigestProfileV1)
			}
			return StorageEnvelopeDigest(kind, object)
		}
		if kind == "recipe" {
			return SemanticJSONDigest(object)
		}
	}
	if object["digest_profile"] == DigestProfileV1 {
		return SemanticJSONDigest(object)
	}
	return ContractDigest(object)
}

func appendSemanticJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		if !utf8.ValidString(typed) {
			return NewValidationError("semantic JSON strings must be valid UTF-8")
		}
		encoded, err := encodeSemanticJSONString(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case json.Number:
		return appendNormalizedSemanticNumber(buffer, string(typed))
	case int:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatInt(int64(typed), 10))
	case int8:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatInt(int64(typed), 10))
	case int16:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatInt(int64(typed), 10))
	case int32:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatInt(int64(typed), 10))
	case int64:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatInt(typed, 10))
	case uint:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatUint(uint64(typed), 10))
	case uint8:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatUint(uint64(typed), 10))
	case uint16:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatUint(uint64(typed), 10))
	case uint32:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatUint(uint64(typed), 10))
	case uint64:
		return appendNormalizedSemanticNumber(buffer, strconv.FormatUint(typed, 10))
	case float32:
		return appendSemanticFloat(buffer, float64(typed), 32)
	case float64:
		return appendSemanticFloat(buffer, typed, 64)
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := appendSemanticJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return NewValidationError("semantic JSON object keys must be valid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := encodeSemanticJSONString(key)
			if err != nil {
				return err
			}
			buffer.Write(encoded)
			buffer.WriteByte(':')
			if err := appendSemanticJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return NewValidationError("semantic JSON does not support value type %T", value)
	}
	return nil
}

func appendNormalizedSemanticNumber(buffer *bytes.Buffer, raw string) error {
	number, err := normalizeSemanticNumber(raw)
	if err != nil {
		return err
	}
	buffer.WriteString(number)
	return nil
}

func appendSemanticFloat(buffer *bytes.Buffer, value float64, bits int) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return NewValidationError("semantic JSON numbers must be finite")
	}
	number, err := normalizeSemanticNumber(strconv.FormatFloat(value, 'g', -1, bits))
	if err != nil {
		return err
	}
	buffer.WriteString(number)
	return nil
}

func encodeSemanticJSONString(value string) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// normalizeSemanticNumber represents an exact JSON decimal as a minimal
// significand plus an optional base-10 exponent. Surface forms such as 1,
// 1.0, and 1e0 therefore have identical canonical bytes.
func normalizeSemanticNumber(raw string) (string, error) {
	parts := semanticNumberPattern.FindStringSubmatch(raw)
	if parts == nil {
		return "", NewValidationError("invalid semantic JSON number %q", raw)
	}
	digits := parts[2] + parts[3]
	first := strings.IndexFunc(digits, func(r rune) bool { return r != '0' })
	if first < 0 {
		return "0", nil
	}
	significand := strings.TrimRight(digits[first:], "0")
	exponent := new(big.Int)
	if parts[4] != "" {
		if _, ok := exponent.SetString(parts[4], 10); !ok {
			return "", NewValidationError("invalid semantic JSON exponent %q", parts[4])
		}
	}
	exponent.Add(exponent, big.NewInt(int64(len(parts[2])-first-1)))
	var result strings.Builder
	result.WriteString(parts[1])
	result.WriteByte(significand[0])
	if len(significand) > 1 {
		result.WriteByte('.')
		result.WriteString(significand[1:])
	}
	if exponent.Sign() != 0 {
		result.WriteByte('e')
		result.WriteString(exponent.String())
	}
	return result.String(), nil
}
