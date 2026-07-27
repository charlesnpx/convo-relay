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
