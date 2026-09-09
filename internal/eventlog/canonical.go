package eventlog

import "github.com/charlesnpx/convo-relay/v2/internal/semanticjson"

const digestPrefix = "sha256:"

// SemanticJSONError reports malformed input or a value that cannot be
// represented under the v2 semantic-json rules.
type SemanticJSONError = semanticjson.SemanticJSONError

// CanonicalJSONError means syntactically valid JSON was not encoded in the
// required semantic-json byte form.
type CanonicalJSONError = semanticjson.CanonicalJSONError

// PortableValueError identifies a prohibited machine-local durable value.
type PortableValueError = semanticjson.PortableValueError

// RawBytesDigest returns the digest for the raw-bytes digest class.
func RawBytesDigest(body []byte) string { return semanticjson.RawBytesDigest(body) }

// SemanticJSONBytes returns canonical semantic-JSON bytes for value.
func SemanticJSONBytes(value any) ([]byte, error) { return semanticjson.SemanticJSONBytes(value) }

// SemanticJSONBytesRaw canonicalizes one strict JSON value supplied as bytes.
func SemanticJSONBytesRaw(body []byte) ([]byte, error) {
	return semanticjson.SemanticJSONBytesRaw(body)
}

// SemanticJSONDigest returns the digest of canonical semantic JSON.
func SemanticJSONDigest(value any) (string, error) { return semanticjson.SemanticJSONDigest(value) }

// SemanticJSONDigestBytes strictly parses then hashes semantic JSON.
func SemanticJSONDigestBytes(body []byte) (string, error) {
	return semanticjson.SemanticJSONDigestBytes(body)
}

// ValidateCanonicalJSON rejects valid-but-noncanonical bytes.
func ValidateCanonicalJSON(body []byte) ([]byte, error) {
	return semanticjson.ValidateCanonicalJSON(body)
}

// DecodeCanonicalJSON validates canonical JSON and decodes its typed value.
func DecodeCanonicalJSON(body []byte, destination any) error {
	return semanticjson.DecodeCanonicalJSON(body, destination)
}

// DecodeJSON decodes strict semantic JSON after expanding exact decimal
// numbers for typed fields.
func DecodeJSON(body []byte, destination any) error {
	return semanticjson.DecodeJSON(body, destination)
}

// PlainSemanticNumber expands one validated semantic number without changing
// its exact value.
func PlainSemanticNumber(value string) (string, error) {
	return semanticjson.PlainSemanticNumber(value)
}

// ValidatePortableValue rejects machine-local values in a durable record.
func ValidatePortableValue(value any, explicitLocalValues ...string) error {
	return semanticjson.ValidatePortableValue(value, explicitLocalValues...)
}
