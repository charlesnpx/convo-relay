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

## `relay-root-portable-export-v1`

A portable export is a closed directory containing `manifest.json` and only
the JSON files named by its ascending `payload_inventory` under
`payloads/<kind>/<portable-id>.json`. Each inventory entry records its type,
portable id, relative path, byte count, `raw-bytes` digest, media type, and the
path-safe source artifact id when the payload originated in the session.

The manifest records the relay version, digest profile, terminal status and
stop reason, and the inventory paths for the portable root projection,
participant transcript, and diagnostics. `inventory_digest` is the
`semantic-json` digest of the complete ordered inventory. `manifest_digest`
is the `semantic-json` digest of the manifest with only `manifest_digest`
removed. Payload files use the released canonical JSON representation so
legacy numeric spellings remain verifiable.

Source artifact refs are resolved and digest-checked before export, then
rewritten as `{ "kind": "portable_payload_ref", "portable_id": "..." }`.
Portable payload identity omits these runtime-only fields:

- execution-workspace identity, source Git root, and source launch CWD;
- named-input source paths and retained-input directory/materialized paths;
- runtime-snapshot settings path and input/transient-source paths; and
- provider-state CWD and settings path.

All other semantic content remains bound. A verifier rejects unknown manifest
fields, unsupported versions or digest profiles, duplicate ids or paths,
unlisted files, symlinks, missing payload links, source-session artifact refs,
and byte-count or digest mismatches. Verification uses no source-session path,
so relocation and source cleanup do not affect the result.
