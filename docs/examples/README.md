# Examples — the API contract

Files 01–05 were hand-written *before* any Go types existed, one per App archetype Orkano must serve; 06 and 09 follow the same rule for the Postgres and MongoDB catalog kinds (ADR-0014 and ADR-0020). They are the contract: the CRD schema serves these files, not the reverse. `hack/validate-examples.sh` proves every file is accepted by the generated CRDs in a real cluster.

Conventions visible throughout: everything omittable is omitted (defaults over dials), secrets appear as references only (INV-03), TLS is always on and has no knob (ADR-0006), and a routable hostname is a separate `Domain` object, never a field on `App`.

The `api-db` Secret referenced in examples 02–03 is produced by the `Postgres` object in 06 — the service catalog kind. The App only ever holds its name; the operator is the Secret's single writer (ADR-0014).

Example 09 shows the MongoDB equivalent. A `Mongo` object produces a same-named Secret, and the App consumes only its `uri` key as `MONGODB_URI` (ADR-0020).
