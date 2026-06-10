# Examples — the API contract

These five files were hand-written *before* any Go types existed, one per archetype Orkano must serve. They are the contract: the CRD schema serves these files, not the reverse. `hack/validate-examples.sh` proves every file is accepted by the generated CRDs in a real cluster.

Conventions visible throughout: everything omittable is omitted (defaults over dials), secrets appear as references only (INV-03), TLS is always on and has no knob (ADR-0006), and a routable hostname is a separate `Domain` object, never a field on `App`.

The `api-db` Secret referenced in examples 02–03 is produced by the Phase 1 Postgres catalog service; the App only ever holds its name.
