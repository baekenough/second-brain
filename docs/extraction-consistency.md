# Relation and action consistency

The extraction prompt uses configured account addresses as identity anchors. A
message author is not automatically the account owner. Sender and recipient
metadata are evidence, not instructions or authority to redefine the owner.

For explicitly named actors and targets, commitment actions should have a
`committed_to` relation and scheduled interactions a `scheduled_with` relation.
Optional action `actor`, `actor_type`, `target`, and `target_type` fields identify
those endpoints. No relation is synthesized from a counterpart alone.

After a document is successfully marked extracted, the worker logs counts of
model-output actions with matching typed endpoints, unmatched supported actions,
and unsupported action kinds. Counts do not certify factual correctness or the
number of newly inserted graph rows. Existing unsupported-kind handling remains
unchanged; diagnostics do not drop otherwise supported incomplete actions.
No names, addresses, action summaries, or raw model relation types are logged by
this diagnostic.

## Existing data and targeted review

This change applies to future extraction attempts. It does not regenerate golden
questions, reset extraction markers, backfill relations, or rewrite action state.
Historical action/graph discrepancies may remain.

1. Review aggregate consistency and relation-type counts first. A high explicit
   `related_to` count is not evidence that unsupported relation names were
   downgraded; these are separate counters.
2. Select a bounded set of document IDs with a concrete reported discrepancy.
   Inspect their source evidence and existing relations/actions through an
   authorized private session; do not export source text to issue trackers.
3. Preserve a protected database backup and record existing relation provenance
   and action identity/state before proposing any repair.
4. Review the intended endpoint identities and relation direction. If evidence
   identifies only a counterpart, preserve the action without inventing an owner
   node or graph edge.
5. There is no new bulk or automatic repair command in this change. A future
   scoped repair must explicitly name the reviewed document IDs, preserve action
   identity/status, and verify before/after counts and evidence. Never reset all
   extraction markers to obtain a new aggregate score.

Synthetic live-provider probes are smoke tests, not a quality benchmark. Outputs
may vary even at temperature zero. Prompt compliance alone is not an entailment
or identity guarantee; human review remains necessary for historical repairs.
