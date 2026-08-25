package integration

import (
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestResultSchemaValidatesDocumentsWithJSONSchema(t *testing.T) {
	bundle, err := DecodeBundleBytes([]byte(`{
  "schema_version": "relay-integration-bundle-v2",
  "id": "result-schema-test",
  "contracts": {
    "result-schema": {
      "turns": [{"participant_turn": 1, "slot": "slot_0", "instructions": "Respond."}],
      "result": {
        "format": "json",
        "schema": {
          "type": "object",
          "required": ["value"],
          "properties": {"value": {"type": "string"}},
          "additionalProperties": false
        }
      }
    }
  }
}`))
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	contract, exists := bundle.Contract("result-schema")
	if !exists {
		t.Fatal("result-schema contract missing")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const schemaURL = "https://convo-relay.invalid/result-schema-test.json"
	if err := compiler.AddResource(schemaURL, contract.Result.Schema); err != nil {
		t.Fatalf("register result.schema: %v", err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile result.schema: %v", err)
	}
	if err := schema.Validate(map[string]any{"value": "valid"}); err != nil {
		t.Fatalf("valid result document rejected: %v", err)
	}
	if err := schema.Validate(map[string]any{"value": 1}); err == nil {
		t.Fatal("invalid result document accepted")
	}
}
