# Public contract version registry

Convo Relay dispatches every public successor payload from its declared
`schema_version`. Readers never infer a version from the presence of fields.
The implementation registry in `internal/contracts` is authoritative and is
also the source for `convo-relay capabilities --json`.

| Contract | Supported versions |
|---|---|
| normalized recipe | numeric `1`, `2` |
| root recipe plan | numeric `1`, `2` |
| integration bundle | `relay-integration-bundle-v1`, `relay-integration-bundle-v2` |
| selected integration contract | numeric `1`, `2` |
| root artifact | numeric `1`, `2` |
| execution workspace | numeric `1`, `2` |
| root session result | numeric `1`, `2` |
| prompt policy | `prompt-policy/v1`, `prompt-policy/v2` |
| prompt-context projection | `relay-prompt-context-v1` |
| provider retry policy | `relay-provider-retry-policy-v1` |
| provider invocation | `relay-provider-invocation-v1` |
| rendered prompt | `relay-rendered-prompt-v1` |
| digest profile | `relay-root-digests-v1` |
| workspace isolation report | `relay-workspace-isolation-v1` |
| portable export | `relay-root-portable-export-v1` |
| capability advertisement | `relay-capabilities-v1` |

Existing v1 payloads retain their released field sets and digest meanings.
Successor-only fields are rejected when placed under a v1 identifier. Unknown
versions fail with the typed `unsupported_contract_version` diagnostic.

## `relay-root-digests-v1`

The successor digest profile has three classes:

- `raw-bytes` is SHA-256 over the bytes exactly as supplied, including a final
  newline when one exists.
- `semantic-json` first requires one valid UTF-8 JSON value with no duplicate
  object members or trailing value. Objects are ordered by UTF-8 key bytes;
  arrays retain order. Strings use JSON escaping without HTML escaping (the
  Unicode line and paragraph separators are written as `\u2028` and `\u2029`).
  Exact decimal numbers use a minimal significand and an optional lowercase
  base-10 exponent, so `1`, `1.0`, and `1e0` are identical and negative zero
  is `0`. The canonical bytes have no trailing newline.
- `storage-envelope` applies the semantic JSON rules after removing only the
  exact JSON Pointers registered for that artifact kind. The initial profile
  excludes `/identity`, `/source/git_root`, `/source/launch_cwd`,
  `/source_after/git_root`, and `/source_after/launch_cwd` from
  `execution_workspace`; every other registered root-artifact kind has an
  empty exclusion list. A semantic member named `path`, `created_at`, or
  `storage_id` is therefore always bound unless its exact pointer is listed.

All three classes use the lowercase form `sha256:<64 lowercase hex>`. The
manifest-inventory digest is the `semantic-json` digest of the ordered typed
payload inventory. Unknown profile ids fail closed. Released v1 payloads keep
using the legacy recursive exclusion algorithm and retain their existing
bytes and digest values.

The Go tests and the small Node verifier consume the same fixture file at
`testdata/contracts/relay-root-digests-v1.json`.
