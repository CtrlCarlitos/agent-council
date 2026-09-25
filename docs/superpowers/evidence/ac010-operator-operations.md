# AC-010 operator and controller operations (#10)

These routes close the two P1 review findings against `8c635a6` and amend
spec §14.17–18. Tests use fixtures only; no live Agy run or evidence Stage
A/B/C has been performed by the agent.

## Record the first protection attestation

On the service's configured local transport, send an operator-authenticated
`POST /v1/runs/{run_id}/agy/attestations`. Authentication is the service's
existing `Authorization: Bearer …` transport credential. A controller lease
is insufficient; the restricted controller bridge exposes no such operation.

The JSON envelope is:

```json
{
  "op_id": "operator-chosen-stable-operation-id",
  "actor": "operator-identity",
  "attestation_id": "optional-recomputed-cprot-v2-digest",
  "attestation": {}
}
```

The empty object above is a placeholder and will be rejected. Supply the
complete `codex.ProtectionAttestation` JSON value, also used for Agy:
`CodexVersion` (the Agy CLI version), `PlatformOS`, `PlatformFamily`,
`ManifestDigest`, `ProfileDigest`, `ProbeRecords`, `ApprovalDenies`,
`ProbedAt` and `Actor`. Nested probe/approval records use the exported
field names in `internal/adapter/codex/attestation.go`. These existing field
names are case sensitive in this documented contract. The record set must
cover the frozen run profile exactly; use the reviewed Stage C evidence,
not an invented or partial record set. `actor` must match `attestation.Actor`.
Omit `attestation_id` to let the service compute it, or supply the exact digest.

The route invokes the existing validation and journal operation even when
the service reports `awaiting_attestation`. It returns HTTP 200 with
`instance_id`, `op_id`, and `receipt`; `receipt.payload` is the attestation
digest. Resending the same operation and evidence returns the same receipt;
changed evidence under that operation ID is rejected. Authorization is
checked on every call, including replay. No child starts while recording.

Restart the service after recording, then inspect `GET /v1/status`. A valid
covering row enables construction; all other eligibility and toolkit gates
still apply. There is no hot reload. Stages A/B/C remain operator-only.

## Abandon an uncertain turn

The connected controller can use `ControllerBridge.ResolveAgyTurnUncertainty`
or `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/agy-disposition`
with the normal authenticated bridge transport:

```json
{
  "op_id": "controller-chosen-stable-operation-id",
  "controller_lease": "current-controller-lease",
  "expected_generation": 1,
  "expected_version": 5,
  "attempt_id": "exact-attempt-from-turn-details",
  "disposition": "abandoned",
  "reason": "How the controller confirmed retirement of the old execution"
}
```

Use the current controller generation, session version and exact released
attempt. Only `abandoned` is accepted. The service refuses a locally live
execution, including before its init handshake. After service/host loss,
the controller must confirm the old execution has retired before submitting
the disposition; absence of a process result does not establish that fact.

One transaction records the attempt disposition, marks the Council turn
`interrupted`, resolves its dispatch intent, parks the session, closes its
recovery episode and journals the reason and controller generation. The
native attempt remains nonterminal and uncertain, with its evidence intact.
This is administrative retirement, not verified cancellation or completion.

HTTP 200 returns a journal receipt. Exact retries replay it; changed input,
wrong attempt, stale version/generation, disconnected or superseded authority,
and repeated disposition under a new operation ID are refused. Replacement
work requires a separately authorized queue/release with a new turn key.
Release then uses the same bound native conversation and verifies init again.
