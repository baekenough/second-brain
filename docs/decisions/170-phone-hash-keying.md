# Decision: phone-number HMAC migration review (#170)

Status: review completed; retain existing identity format for now. No key is
provisioned and no production privacy flag is enabled by this decision.

`ShortHash` is a truncated, unkeyed SHA-256 identifier. The phone-number input space
is small enough for offline enumeration. Its stable output is useful for joins and
deduplication but **does not provide phone-number confidentiality**. Default-off
number display hashing also does not hide raw content elsewhere in the data flow.
Adding HMAC to one metadata field while leaving an enumerable phone-derived
SourceID visible would not address that threat.

## Why a direct replacement is rejected

SMS and call SourceIDs embed the existing number hash. Calls also join their log
and transcript through that identity, while imports, updates and duplicate checks
rely on stable SourceIDs. Replacing `ShortHash` globally, or changing its key on
rotation, makes existing source events look new. The consequences include duplicate
documents and broken chunk/action/entity/feedback associations. A metadata-only
HMAC feature would add key operations without establishing the promised privacy.

## Prerequisites for a future migration

1. Inventory all identifiers, filenames, titles/content, metadata, logs, exports,
   indexes, embeddings, replicas and backups. Define the exact disclosure threat
   and retained raw-data policy. A key cannot redact existing content or backups.
2. Separate immutable internal identity from public pseudonyms. Keep an access-
   controlled mapping from legacy identity to an opaque stable ID. Event matching
   and reingest must consult that mapping before insert. Do not expose legacy
   SHA-derived aliases to readers of pseudonymized exports.
3. Version public tokens, e.g. `phone:v1:<key-id>:<HMAC>`, with domain-separated
   HMAC-SHA-256 over a documented canonical phone-number representation. Keep
   identity matching separate from display token generation. Choose truncation
   from the required collision budget, not from the legacy 16-hex format.
4. Provision a high-entropy server key in an appropriate secret store, separate
   from the database and ordinary backups. Validate format/length/key ID at startup
   and fail closed if HMAC mode is selected with a missing or invalid key. Never
   silently substitute unkeyed SHA. Restrict access and audit key use.
5. Support versioned dual-read during migration and single-write to the selected
   version. Rotation rekeys pseudonyms without changing internal identity or
   creating new source events. Retain old verification keys for the documented
   transition period; distinguish planned rotation from compromise response.
6. Back up the mapping and key material with separate access controls and a tested
   recovery procedure. Losing a key can make historic references unresolvable;
   retaining compromised old keys indefinitely defeats rotation. Document retention,
   revocation, disaster recovery and who can authorize each action.
7. Dry-run on a representative copy. Compare counts and dedup/joins across legacy
   reingest, key rotation, log/transcript arrival in either order, mixed-version
   reads, deletions and retries. Measure coverage of redaction across all sinks.
8. Migrate transactionally or in resumable batches with explicit checkpoints and
   verified manifests. Roll back application routing to the previous identity
   mapping/version without restoring duplicate inserts or casually re-exposing
   redacted raw fields. Keep rollback-compatible mapping/key backups until the
   validation and retention window closes.

The current retained risk is explicit: existing number-derived SourceIDs remain
stable and enumerable. Any future confidentiality claim requires the complete
identity/sink migration above, not merely a new HMAC helper. That implementation
needs a separately authorized migration and key-lifecycle plan.
