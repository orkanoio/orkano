# ADR-0020: Add MongoDB as an engine-specific Mongo catalog kind

- Status: Accepted
- Date: 2026-07-19
- Amended: 2026-07-19 — optional authenticated Mongo Express development UI

## Context

An application being dogfooded through Orkano requires `MONGODB_URI`, so the Postgres-only catalog cannot provision everything the application needs. MongoDB was a documented v1 cut-line candidate, but the user explicitly promoted it into the active milestone on 2026-07-19. This is a new public Kubernetes API and therefore needs the same deliberately small, engine-specific contract ADR-0014 established for Postgres.

MongoDB now publishes both long-lived major release lines and faster-moving minor release lines. The catalog needs a predictable, manually controlled lifecycle appropriate for a self-hosted solo-maintainer project, not an implicit sequence of frequent minor upgrades.

## Decision

- Add a namespaced `Mongo` kind in `orkano.io/v1alpha1`, plural `mongoes`, category `orkano`. The product and dashboard call it MongoDB; the Kubernetes kind follows ADR-0014's bare engine noun and its explicit statement that future engines are sibling kinds.
- Keep the database contract to `version` and `storageSize`, matching the proven Postgres catalog shape. `version` supports and defaults to `"8.0"`, the MongoDB major release line, and is immutable. `storageSize` defaults to `10Gi` and is grow-only in the reconciler. The one additive tool field is optional `mongoExpress.enabled`; absent and false are equivalent.
- Render a single-node, authentication-enabled MongoDB StatefulSet backed by a persistent volume. Replication, backups, tuning, extra users/databases, TLS, and exposure remain additive future fields.
- Name the connection Secret exactly `metadata.name` and use the same frozen six keys as Postgres: `uri`, `host`, `port`, `database`, `username`, and `password`. `uri` uses `mongodb://.../<database>?authSource=admin` and is the value applications reference as `MONGODB_URI`. No credential value appears in a custom resource or Orkano's metadata database.
- Resolve `8.0` to a digest-pinned, amd64+arm64 image index and verify the pin with the existing image-pin CI guard.
- Keep cross-kind names unique in the dashboard API across `App`, `Postgres`, and `Mongo`. Kubernetes cannot atomically enforce uniqueness across different resource kinds without the admission webhook ADR-0010 rejected, so direct `kubectl` creates retain the controllers' foreign-owner conflict as the honest backstop.
- When `mongoExpress.enabled` is true, reconcile a digest-pinned Mongo Express Deployment, ClusterIP Service, session-secret Secret, and default-deny NetworkPolicy. Only the dashboard pod may reach its HTTP port; it may egress only to DNS and its owning MongoDB pod. It gets the Mongo connection URI through a Secret reference, never a custom-resource field.
- Mongo Express stays behind an authenticated dashboard reverse proxy. The proxy fixes the namespace, Service, and port from operator-owned status, strips the browser's Cookie and Authorization headers before forwarding, strips upstream cookies from responses, and exposes no Ingress or NodePort. A response Content Security Policy restricts browser connections and form submissions to that Mongo Express instance's path so its same-origin page cannot call unrelated dashboard APIs. Enable and disable require step-up authentication; opening uses the existing Orkano session.
- `Ready` continues to describe MongoDB only. `MongoExpressReady` separately reports the optional tool's availability and `status.mongoExpressServiceName` names its operator-owned internal Service.

## Consequences

- Applications can consume MongoDB by adding an environment variable named `MONGODB_URI` whose Secret reference is `<mongo-name>/uri`; the dashboard explains this exact wiring.
- `Mongo` is additive beside `Postgres`; no stored Postgres object or Secret contract changes.
- The immutable `8.0` enum means Orkano must keep a working image pin while this API version is served. A future major release is an additive enum value only after its upgrade and compatibility story is deliberately accepted.
- Delete-and-recreate deletes the data PVC. The dashboard therefore keeps deletion behind step-up authentication and states that data loss is permanent.
- A direct Kubernetes client can still create cross-kind duplicate names. The losing controller refuses to adopt or overwrite the other resource's child object and reports the conflict in `Ready`; avoiding a validating webhook keeps ADR-0010 intact.
- A single authenticated root-style application user is simple but broad inside that MongoDB instance. Per-database least privilege and replica sets are future additions, not hidden v1 behavior.
- Disabling Mongo Express deletes only its stateless Deployment, Service, NetworkPolicy, and session Secret. The MongoDB StatefulSet, connection Secret, and data PVC are untouched.
- Docker marks the official Mongo Express image deprecated due to maintainer inactivity, and upstream says it should be used privately for development because document handling can execute JavaScript on the server. Orkano therefore labels the feature experimental, keeps it disabled by default, pins the last official multi-architecture image, applies restricted container settings and tight network policy, and never offers public exposure. The admin still accepts the upstream risk explicitly by enabling it.

## Alternatives considered

- **Generic `Database` with an engine enum** — rejected by ADR-0014: engine-specific lifecycle, workload, and Secret semantics do not fit an honest lowest-common-denominator resource.
- **MongoDB minor release track (8.2/8.3)** — rejected because it requires more frequent sequential upgrades; the major 8.0 line gives a predictable support window and manual upgrade control.
- **A validating admission webhook for global names** — rejected because it would reverse ADR-0010 and add an availability-critical component for a dashboard-detectable usability issue.
- **Unauthenticated MongoDB inside the namespace** — rejected because any compromised app pod could read or mutate every database with no credential boundary.
- **A public Mongo Express Ingress or credentials embedded in the launch URL** — rejected because either would violate Orkano's private-by-default dashboard posture and leak a long-lived database credential into browser history, logs, or referrers.
- **Dashboard-created Deployment/Service objects** — rejected because the dashboard never mutates workloads. The toggle writes desired state to the Mongo CR; the narrow-RBAC operator reconciles it.
