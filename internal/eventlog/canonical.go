package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const digestPrefix = "sha256:"

var semanticNumberPattern = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)
var localPortPattern = regexp.MustCompile(`(?i)\bport\s*(?:=|:|\s)\s*[0-9]{1,5}\b`)

// SemanticJSONError reports malformed input or a value that cannot be
// represented under the v2 semantic-json rules.
type SemanticJSONError struct {
	Reason string
}

func (e *SemanticJSONError) Error() string { return "semantic JSON: " + e.Reason }

// CanonicalJSONError means syntactically valid JSON was not encoded in the
// required semantic-json byte form.
type CanonicalJSONError struct {
	Reason string
}

func (e *CanonicalJSONError) Error() string { return "non-canonical JSON: " + e.Reason }

// RawBytesDigest is the raw-bytes digest class. Blob files and event-log file
// bytes use this address format; semantic JSON first canonicalizes its input.
func RawBytesDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return digestPrefix + hex.EncodeToString(sum[:])
}

// SemanticJSONBytes implements deterministic key ordering, no HTML escaping,
// and exact-decimal number normalization. It intentionally has no field
// exclusion or envelope-stripping mode.
func SemanticJSONBytes(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return SemanticJSONBytesRaw(body)
}

// SemanticJSONBytesRaw canonicalizes one strict JSON value supplied as bytes.
func SemanticJSONBytesRaw(body []byte) ([]byte, error) {
	node, err := parseSemanticJSON(body)
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	if err := node.appendTo(&result); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

// SemanticJSONDigest hashes canonical semantic JSON.
func SemanticJSONDigest(value any) (string, error) {
	body, err := SemanticJSONBytes(value)
	if err != nil {
		return "", err
	}
	return RawBytesDigest(body), nil
}

// SemanticJSONDigestBytes strictly parses then hashes semantic JSON.
func SemanticJSONDigestBytes(body []byte) (string, error) {
	canonical, err := SemanticJSONBytesRaw(body)
	if err != nil {
		return "", err
	}
	return RawBytesDigest(canonical), nil
}

// ValidateCanonicalJSON rejects valid-but-noncanonical bytes and returns the
// canonical form so callers never need a separate canonicalization pass.
func ValidateCanonicalJSON(body []byte) ([]byte, error) {
	canonical, err := SemanticJSONBytesRaw(body)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(body, canonical) {
		return nil, &CanonicalJSONError{Reason: "bytes differ from semantic-json form"}
	}
	return canonical, nil
}

// DecodeCanonicalJSON verifies strict canonical JSON before decoding a typed
// boundary structure. The decoder rejects unknown fields in that structure.
func DecodeCanonicalJSON(body []byte, destination any) error {
	node, err := parseSemanticJSON(body)
	if err != nil {
		return err
	}
	var canonical bytes.Buffer
	if err := node.appendTo(&canonical); err != nil {
		return err
	}
	if !bytes.Equal(body, canonical.Bytes()) {
		return &CanonicalJSONError{Reason: "bytes differ from semantic-json form"}
	}
	// Go's encoding/json deliberately rejects exponent notation for integer
	// fields, while semantic-json canonicalizes 10 as 1e1. Decode an exact
	// decimal expansion only after validating the original canonical bytes.
	var decoding bytes.Buffer
	if err := node.appendDecodingTo(&decoding); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoding.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return &SemanticJSONError{Reason: "contains a second top-level value"}
	}
	return nil
}

type semanticKind uint8

const (
	semanticNull semanticKind = iota
	semanticBool
	semanticString
	semanticNumber
	semanticArray
	semanticObject
)

type semanticNode struct {
	kind    semanticKind
	boolean bool
	text    string
	array   []semanticNode
	object  []semanticMember
}

type semanticMember struct {
	key   string
	value semanticNode
}

func parseSemanticJSON(body []byte) (semanticNode, error) {
	if !utf8.Valid(body) {
		return semanticNode{}, &SemanticJSONError{Reason: "input is not valid UTF-8"}
	}
	parser := semanticParser{body: body}
	parser.skipSpace()
	if parser.atEnd() {
		return semanticNode{}, &SemanticJSONError{Reason: "input is empty"}
	}
	node, err := parser.parseValue()
	if err != nil {
		return semanticNode{}, err
	}
	parser.skipSpace()
	if !parser.atEnd() {
		return semanticNode{}, &SemanticJSONError{Reason: "contains trailing content"}
	}
	return node, nil
}

type semanticParser struct {
	body []byte
	pos  int
}

func (p *semanticParser) atEnd() bool { return p.pos >= len(p.body) }

func (p *semanticParser) skipSpace() {
	for !p.atEnd() {
		switch p.body[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *semanticParser) parseValue() (semanticNode, error) {
	p.skipSpace()
	if p.atEnd() {
		return semanticNode{}, &SemanticJSONError{Reason: "value is truncated"}
	}
	switch p.body[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		value, err := p.parseString()
		return semanticNode{kind: semanticString, text: value}, err
	case 't':
		if p.consumeLiteral("true") {
			return semanticNode{kind: semanticBool, boolean: true}, nil
		}
	case 'f':
		if p.consumeLiteral("false") {
			return semanticNode{kind: semanticBool, boolean: false}, nil
		}
	case 'n':
		if p.consumeLiteral("null") {
			return semanticNode{kind: semanticNull}, nil
		}
	default:
		if p.body[p.pos] == '-' || p.body[p.pos] >= '0' && p.body[p.pos] <= '9' {
			return p.parseNumber()
		}
	}
	return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("invalid value at byte %d", p.pos)}
}

func (p *semanticParser) consumeLiteral(literal string) bool {
	if len(p.body)-p.pos < len(literal) || string(p.body[p.pos:p.pos+len(literal)]) != literal {
		return false
	}
	p.pos += len(literal)
	return true
}

func (p *semanticParser) parseObject() (semanticNode, error) {
	p.pos++ // {
	p.skipSpace()
	if !p.atEnd() && p.body[p.pos] == '}' {
		p.pos++
		return semanticNode{kind: semanticObject, object: []semanticMember{}}, nil
	}
	members := []semanticMember{}
	seen := make(map[string]struct{})
	for {
		p.skipSpace()
		if p.atEnd() || p.body[p.pos] != '"' {
			return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("object key expected at byte %d", p.pos)}
		}
		key, err := p.parseString()
		if err != nil {
			return semanticNode{}, err
		}
		if _, exists := seen[key]; exists {
			return semanticNode{}, &SemanticJSONError{Reason: "object contains a duplicate key"}
		}
		seen[key] = struct{}{}
		p.skipSpace()
		if p.atEnd() || p.body[p.pos] != ':' {
			return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("object colon expected at byte %d", p.pos)}
		}
		p.pos++
		value, err := p.parseValue()
		if err != nil {
			return semanticNode{}, err
		}
		members = append(members, semanticMember{key: key, value: value})
		p.skipSpace()
		if p.atEnd() {
			return semanticNode{}, &SemanticJSONError{Reason: "object is truncated"}
		}
		if p.body[p.pos] == '}' {
			p.pos++
			return semanticNode{kind: semanticObject, object: members}, nil
		}
		if p.body[p.pos] != ',' {
			return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("object comma expected at byte %d", p.pos)}
		}
		p.pos++
	}
}

func (p *semanticParser) parseArray() (semanticNode, error) {
	p.pos++ // [
	p.skipSpace()
	if !p.atEnd() && p.body[p.pos] == ']' {
		p.pos++
		return semanticNode{kind: semanticArray, array: []semanticNode{}}, nil
	}
	items := []semanticNode{}
	for {
		value, err := p.parseValue()
		if err != nil {
			return semanticNode{}, err
		}
		items = append(items, value)
		p.skipSpace()
		if p.atEnd() {
			return semanticNode{}, &SemanticJSONError{Reason: "array is truncated"}
		}
		if p.body[p.pos] == ']' {
			p.pos++
			return semanticNode{kind: semanticArray, array: items}, nil
		}
		if p.body[p.pos] != ',' {
			return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("array comma expected at byte %d", p.pos)}
		}
		p.pos++
	}
}

func (p *semanticParser) parseString() (string, error) {
	start := p.pos
	p.pos++ // opening quote
	for !p.atEnd() {
		character := p.body[p.pos]
		switch character {
		case '"':
			p.pos++
			var decoded string
			if err := json.Unmarshal(p.body[start:p.pos], &decoded); err != nil {
				return "", &SemanticJSONError{Reason: err.Error()}
			}
			if !utf8.ValidString(decoded) {
				return "", &SemanticJSONError{Reason: "string is not valid UTF-8"}
			}
			return decoded, nil
		case '\\':
			p.pos++
			if p.atEnd() {
				return "", &SemanticJSONError{Reason: "string escape is truncated"}
			}
			escaped := p.body[p.pos]
			switch escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos++
			case 'u':
				if p.pos+4 >= len(p.body) {
					return "", &SemanticJSONError{Reason: "unicode escape is truncated"}
				}
				for index := 1; index <= 4; index++ {
					if !isHex(p.body[p.pos+index]) {
						return "", &SemanticJSONError{Reason: "unicode escape is invalid"}
					}
				}
				p.pos += 5
			default:
				return "", &SemanticJSONError{Reason: "string escape is invalid"}
			}
		default:
			if character < 0x20 {
				return "", &SemanticJSONError{Reason: "string contains an unescaped control character"}
			}
			p.pos++
		}
	}
	return "", &SemanticJSONError{Reason: "string is truncated"}
}

func (p *semanticParser) parseNumber() (semanticNode, error) {
	start := p.pos
	for !p.atEnd() && !isJSONDelimiter(p.body[p.pos]) {
		p.pos++
	}
	raw := string(p.body[start:p.pos])
	if !semanticNumberPattern.MatchString(raw) {
		return semanticNode{}, &SemanticJSONError{Reason: fmt.Sprintf("invalid number %q", raw)}
	}
	return semanticNode{kind: semanticNumber, text: raw}, nil
}

func isJSONDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', ',', ']', '}':
		return true
	default:
		return false
	}
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func (node semanticNode) appendTo(buffer *bytes.Buffer) error {
	switch node.kind {
	case semanticNull:
		buffer.WriteString("null")
	case semanticBool:
		if node.boolean {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case semanticString:
		encoded, err := semanticQuote(node.text)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case semanticNumber:
		normalized, err := normalizeSemanticNumber(node.text)
		if err != nil {
			return err
		}
		buffer.WriteString(normalized)
	case semanticArray:
		buffer.WriteByte('[')
		for index, item := range node.array {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := item.appendTo(buffer); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case semanticObject:
		members := append([]semanticMember(nil), node.object...)
		sort.Slice(members, func(left, right int) bool { return members[left].key < members[right].key })
		buffer.WriteByte('{')
		for index, member := range members {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := semanticQuote(member.key)
			if err != nil {
				return err
			}
			buffer.Write(encoded)
			buffer.WriteByte(':')
			if err := member.value.appendTo(buffer); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return &SemanticJSONError{Reason: "unknown node kind"}
	}
	return nil
}

// appendDecodingTo writes semantically equivalent ordinary JSON that Go's
// typed decoder accepts for integer fields. It is never used for hashing or
// persistence, so canonical bytes remain the authority.
func (node semanticNode) appendDecodingTo(buffer *bytes.Buffer) error {
	switch node.kind {
	case semanticNull:
		buffer.WriteString("null")
	case semanticBool:
		if node.boolean {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case semanticString:
		encoded, err := semanticQuote(node.text)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case semanticNumber:
		plain, err := plainSemanticNumber(node.text)
		if err != nil {
			return err
		}
		buffer.WriteString(plain)
	case semanticArray:
		buffer.WriteByte('[')
		for index, item := range node.array {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := item.appendDecodingTo(buffer); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case semanticObject:
		buffer.WriteByte('{')
		for index, member := range node.object {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := semanticQuote(member.key)
			if err != nil {
				return err
			}
			buffer.Write(encoded)
			buffer.WriteByte(':')
			if err := member.value.appendDecodingTo(buffer); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return &SemanticJSONError{Reason: "unknown node kind"}
	}
	return nil
}

func semanticQuote(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, &SemanticJSONError{Reason: "string is not valid UTF-8"}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func normalizeSemanticNumber(raw string) (string, error) {
	parts := semanticNumberPattern.FindStringSubmatch(raw)
	if parts == nil {
		return "", &SemanticJSONError{Reason: fmt.Sprintf("invalid number %q", raw)}
	}
	digits := parts[2] + parts[3]
	first := strings.IndexFunc(digits, func(character rune) bool { return character != '0' })
	if first < 0 {
		return "0", nil
	}
	significand := strings.TrimRight(digits[first:], "0")
	exponent := new(big.Int)
	if parts[4] != "" {
		if _, ok := exponent.SetString(parts[4], 10); !ok {
			return "", &SemanticJSONError{Reason: "invalid exponent"}
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

func plainSemanticNumber(raw string) (string, error) {
	parts := semanticNumberPattern.FindStringSubmatch(raw)
	if parts == nil {
		return "", &SemanticJSONError{Reason: fmt.Sprintf("invalid number %q", raw)}
	}
	digits := parts[2] + parts[3]
	exponent := new(big.Int)
	if parts[4] != "" {
		if _, ok := exponent.SetString(parts[4], 10); !ok {
			return "", &SemanticJSONError{Reason: "invalid exponent"}
		}
	}
	decimal := new(big.Int).Add(exponent, big.NewInt(int64(len(parts[2]))))
	if !decimal.IsInt64() {
		return "", &SemanticJSONError{Reason: "number exponent is too large to decode"}
	}
	position := decimal.Int64()
	// A typed decode that would require megabytes of zero padding cannot fit any
	// v2 integer field anyway; reject it rather than allocating unbounded memory.
	if position > 1_000_000 || position < -1_000_000 {
		return "", &SemanticJSONError{Reason: "number exponent is too large to decode"}
	}
	var result strings.Builder
	result.WriteString(parts[1])
	switch {
	case position <= 0:
		result.WriteString("0.")
		result.WriteString(strings.Repeat("0", int(-position)))
		result.WriteString(digits)
	case position >= int64(len(digits)):
		result.WriteString(digits)
		result.WriteString(strings.Repeat("0", int(position)-len(digits)))
	default:
		index := int(position)
		result.WriteString(digits[:index])
		result.WriteByte('.')
		result.WriteString(digits[index:])
	}
	return result.String(), nil
}

// ValidatePortableValue prevents a typed durable record from embedding a
// machine-local value. The explicit roots are supplied by the caller (session
// root and relay home); current-process markers cover accidental local facts.
func ValidatePortableValue(value any, explicitLocalValues ...string) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	node, err := parseSemanticJSON(body)
	if err != nil {
		return err
	}
	markers := localMarkers(explicitLocalValues)
	return validatePortableNode(node, markers)
}

func localMarkers(explicit []string) []string {
	markers := append([]string(nil), explicit...)
	if home, err := os.UserHomeDir(); err == nil {
		markers = append(markers, home)
	}
	if temporary := os.TempDir(); temporary != "" {
		markers = append(markers, temporary)
	}
	if hostname, err := os.Hostname(); err == nil {
		markers = append(markers, hostname)
	}
	if current, err := user.Current(); err == nil {
		markers = append(markers, current.Username)
	}
	return markers
}

func validatePortableNode(node semanticNode, markers []string) error {
	switch node.kind {
	case semanticString:
		if looksLikeAbsolutePath(node.text) {
			return &PortableValueError{Value: node.text, Reason: "contains an absolute path"}
		}
		for _, marker := range markers {
			marker = strings.TrimSpace(marker)
			if len(marker) > 1 && strings.Contains(node.text, marker) {
				return &PortableValueError{Value: node.text, Reason: "contains a current-machine value"}
			}
		}
		if looksLikeLocalEndpoint(node.text) {
			return &PortableValueError{Value: node.text, Reason: "contains a local endpoint"}
		}
		if looksLikeCurrentProcessID(node.text) {
			return &PortableValueError{Value: node.text, Reason: "contains the current process id"}
		}
	case semanticArray:
		for _, item := range node.array {
			if err := validatePortableNode(item, markers); err != nil {
				return err
			}
		}
	case semanticObject:
		for _, member := range node.object {
			if err := validatePortableNode(member.value, markers); err != nil {
				return err
			}
		}
	}
	return nil
}

func looksLikeAbsolutePath(value string) bool {
	if strings.HasPrefix(value, "\\\\") || strings.HasPrefix(value, "~/") {
		return true
	}
	for index := 0; index < len(value); index++ {
		if value[index] == '~' && (index == 0 || pathBoundary(value, index)) && index+1 < len(value) && value[index+1] == '/' {
			return true
		}
		if value[index] == '/' && pathBoundary(value, index) && (index+1 == len(value) || value[index+1] != '/') {
			return true
		}
		if index+2 < len(value) && isASCIIAlpha(value[index]) && value[index+1] == ':' && (value[index+2] == '/' || value[index+2] == '\\') && pathBoundary(value, index) {
			return true
		}
	}
	return false
}

func pathBoundary(value string, index int) bool {
	if index == 0 {
		return true
	}
	switch value[index-1] {
	case ' ', '\t', '\n', '\r', '=', ':', '"', '\'', '(', '[', '{':
		return true
	default:
		return false
	}
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func looksLikeLocalEndpoint(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "localhost:") || strings.Contains(lower, "127.0.0.1:") || strings.Contains(lower, "[::1]:") || localPortPattern.MatchString(value)
}

func looksLikeCurrentProcessID(value string) bool {
	pid := strconv.Itoa(os.Getpid())
	if value == pid {
		return true
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "pid="+pid) ||
		strings.Contains(lower, "pid:"+pid) ||
		strings.Contains(lower, "pid "+pid) ||
		strings.Contains(lower, "process_id="+pid) ||
		strings.Contains(lower, "process id="+pid)
}

// PortableValueError identifies a prohibited machine-local durable value.
type PortableValueError struct {
	Value  string
	Reason string
}

func (e *PortableValueError) Error() string {
	return "non-portable durable value: " + e.Reason
}

// SemanticFloat is available to typed callers that need to verify finite
// floating-point input before semantic serialization. Plans intentionally do
// not use floating-point fields.
func SemanticFloat(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return &SemanticJSONError{Reason: "numbers must be finite"}
	}
	_, err := normalizeSemanticNumber(strconv.FormatFloat(value, 'g', -1, 64))
	return err
}
