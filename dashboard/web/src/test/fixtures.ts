import type {
  AppResponse,
  AppSpec,
  AppStatus,
  Condition,
  DomainResponse,
  ExternalSecretItem,
  PostgresResponse,
  PostgresSpec,
  PostgresStatus,
  SecretStoreItem,
} from "@/lib/api";

// Shared builders for the App/catalog screen tests: a minimal valid object
// per kind, deep-overridable per test.

export function readyCondition(
  status: Condition["status"],
  reason = "",
  message = "",
): Condition {
  return { type: "Ready", status, reason, message };
}

export function makeApp(overrides?: {
  name?: string;
  spec?: Partial<AppSpec>;
  status?: AppStatus;
}): AppResponse {
  return {
    name: overrides?.name ?? "web",
    namespace: "orkano-apps",
    creationTimestamp: "2026-07-01T10:00:00Z",
    spec: {
      source: { github: { repo: "orkanoio/example" } },
      build: { strategy: "Dockerfile" },
      type: "Web",
      replicas: 1,
      ...overrides?.spec,
    },
    status: { ...overrides?.status },
  };
}

export function makeDomain(
  overrides?: Partial<DomainResponse>,
): DomainResponse {
  return {
    name: "web.example.com",
    namespace: "orkano-apps",
    creationTimestamp: "2026-07-01T10:00:00Z",
    spec: { host: "web.example.com", appRef: { name: "web" } },
    status: {},
    ...overrides,
  };
}

export function makePostgres(overrides?: {
  name?: string;
  spec?: Partial<PostgresSpec>;
  status?: PostgresStatus;
}): PostgresResponse {
  return {
    name: overrides?.name ?? "api-db",
    namespace: "orkano-apps",
    creationTimestamp: "2026-07-01T10:00:00Z",
    spec: { version: "16", storageSize: "10Gi", ...overrides?.spec },
    status: { ...overrides?.status },
  };
}

export function makeSecretStore(
  overrides?: Partial<SecretStoreItem>,
): SecretStoreItem {
  return {
    name: "team-vault",
    creationTimestamp: "2026-07-01T10:00:00Z",
    provider: "vault",
    server: "https://vault.internal.example:8200",
    path: "secret",
    ready: "True",
    ...overrides,
  };
}

export function makeExternalSecret(
  overrides?: Partial<ExternalSecretItem>,
): ExternalSecretItem {
  return {
    name: "api-stripe",
    creationTimestamp: "2026-07-01T10:00:00Z",
    storeName: "team-vault",
    refreshInterval: "1h",
    keys: [{ secretKey: "STRIPE_KEY", remoteKey: "apps/api/stripe" }],
    ready: "True",
    ...overrides,
  };
}
