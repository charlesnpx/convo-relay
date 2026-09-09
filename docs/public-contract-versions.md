# Public formats

Convo Relay has three durable public formats. `convo-relay version --json`
reports them under `formats` and reports the supported digest classes under
`digest_classes`. It does not inspect a session or run a provider.

| Format | Purpose | Durable location |
|---|---|---|
| `relay.plan/v1` | Immutable compiled execution plan | `session.json` |
| `relay.event/v1` | Canonical append-only execution record | `events.jsonl` |
| `relay.bundle/v1` | Portable closure manifest | `manifest.json` in an exported bundle |

## `relay.plan/v1`

A plan is the complete, immutable execution shape selected before the session
starts. Its root contains `"kind":"relay.plan/v1"` and the session's one
`schema_version`, currently `1`. Nested plan records do not carry independent
version fields.

The plan records actors, schedule, lifecycle policy, workspace mode, named
inputs, and result policy. Payload-bearing plan fields use the shared BlobRef
shape below. Resuming a session may add events; it never mutates this plan.

The direct launch form is `convo-relay run --plan FILE`, with `--blobs DIR`
when the plan references payloads. A plan's generic instruction block is
`instructions`. Each named input carries its payloads in `contents`, an
ordered list of BlobRef values, rather than a single `content` value. The
supplied plan is validated as written; launch defaults are not applied.

## `relay.event/v1`

The event log is a canonical JSONL stream. Each record has
`"kind":"relay.event/v1"`, a positive `seq`, an `event_id`, a UTC `time`, a
closed `type`, and that type's typed `payload`. Event payloads do not carry
separate version fields.

Readers reject malformed middle records, invalid type/payload combinations,
and non-canonical JSON. A truncated final record is recoverable as an
interrupted tail. The event sequence, together with the immutable plan and
referenced blobs, is the authority for all session views.

## `relay.bundle/v1`

A portable bundle is a closed directory with a `manifest.json` whose
`kind` is `relay.bundle/v1`. Its manifest names exactly three exported
payloads: the root-session projection, participant transcript, and diagnostics
projection. `payload_inventory` is sorted by path and each entry has:

```json
{
  "kind": "root_session",
  "portable_id": "session",
  "path": "payloads/root_session/session.json",
  "blob": {
    "sha256": "<64 lowercase hexadecimal characters>",
    "size": 123,
    "media_type": "application/json"
  }
}
```

The verifier rejects a missing payload, altered payload, symlink, unexpected
file, unknown manifest field, duplicate identity, out-of-order inventory, or
digest mismatch. `inventory_digest` is the semantic JSON digest of the ordered
inventory. `manifest_digest` is the semantic JSON digest of the manifest with
only `manifest_digest` omitted.

## Shared BlobRef

Every payload reference has the same nested form:

```json
{"sha256":"<64 lowercase hexadecimal characters>","size":123,"media_type":"text/plain; charset=utf-8"}
```

`sha256` is the SHA-256 of the raw payload bytes, `size` is the byte count, and
`media_type` describes the referenced bytes. It is deliberately independent
of a local path or session directory.

## Digest classes

There are two digest classes:

- `raw-bytes` hashes bytes exactly as supplied. BlobRef values use this class.
- `semantic-json` requires one strict UTF-8 JSON value, rejects duplicate keys
  and trailing content, canonicalizes member order and exact decimal spelling,
  then hashes the canonical bytes. Plans and bundle manifest digests use this
  class.

Semantic JSON digest strings use `sha256:<64 lowercase hexadecimal
characters>`. A semantic JSON value has no trailing newline. These are the
only digest meanings exposed by the public formats.

## Compatibility boundary

Only the formats above are accepted for new durable data. There is no registry
of parallel public contract families and no conversion path for retired
durable versions. Configuration files and command JSON reports are inputs or
projections, not additional durable format families.
