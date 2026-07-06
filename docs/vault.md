# External vaults

Orkano can sync secrets out of an external vault instead of having you type
them into the dashboard. Under the hood this is the
[External Secrets Operator](https://external-secrets.io) (ESO), vendored and
scoped so it can only write Secrets in `orkano-apps` (ADR-0018): a connected
store is an ESO `SecretStore`, each sync is an `ExternalSecret`, and what the
UI shows is exactly what `kubectl get secretstores,externalsecrets -n
orkano-apps` shows. Secret values never touch Orkano's database (INV-03) —
they flow from your vault into ordinary Kubernetes Secrets that apps
reference by name.

## Enabling it

Vault support is opt-in. Re-run the installer with the flag — the re-run
converges an existing install, it does not reinstall:

```sh
orkano init --secrets-vault ...your original flags...
```

Until then the dashboard's Vault page, the wizard's secrets step, and
`orkano doctor` all print exactly that one-liner. Disabling is deliberately
manual: the deployed set includes the synced Secrets' controller, so removing
it is a decision to make with `kubectl`, not a flag flip.

## Connecting HashiCorp Vault

Vault is the flagship, UI-connectable provider. On the **Vault** page,
*Connect a store* asks for:

- **Vault server** — `https://` only; the credential travels over this
  connection.
- **Mount path** — the KV secrets engine's mount, usually `secret`.
- **KV version** — v2 (versioned, the default) or v1.
- **Token** — stored write-only in the Kubernetes Secret
  `<store>-credentials`, which the dashboard can rotate but never read back.

*Rotate* on a connected store updates the endpoint or token (an empty token
keeps the current credential). *Disconnect* removes the store and its
credentials Secret together.

## A scoped policy for the token

A dashboard compromise can read whatever slice of the vault the store's
credential reaches (threat-model accepted risk #10), so scope the token to a
dedicated prefix and put only the secrets Orkano should sync under it.

```hcl
# orkano-read.hcl — read-only on the orkano/ prefix of a KV v2 mount
path "secret/data/orkano/*" {
  capabilities = ["read"]
}
```

For KV v1 drop the `data/` segment: `path "secret/orkano/*"`.

```sh
vault policy write orkano-read orkano-read.hcl
vault token create -policy=orkano-read -orphan -period=768h -display-name=orkano
```

Two details that matter:

- **Keep the default policy attached** (the command above does — don't pass
  `-no-default-policy`). ESO health-checks the credential with a token
  self-lookup, which the default policy grants; without it every read would
  still work but the store would report not-Ready.
- **Rotate before the period lapses.** ESO treats the token as a static
  credential and does not renew it, so a periodic token expires `period`
  after it was created. Rotate it from the Vault page before then. An expired
  credential is loud, not silent: the store goes not-Ready on the Vault page,
  in the wizard's secrets step, and in `orkano doctor`'s
  `secrets.store-health`.

`-orphan` keeps the token alive if the admin token that created it is revoked
(it needs sudo capability on token creation; a root token has it).

## Syncing secrets into apps

*New sync* pulls up to 64 named keys out of a store into one Kubernetes
Secret, re-read on the refresh interval (default 1h). The sync's name **is**
the produced Secret's name. Each row maps a vault key — a path under the
mount, like `orkano/api/stripe` — to an environment-style key in the Secret.

To use it from an app: env editor → *Add variable* → kind *Secret
reference* → the sync's name and key. Removing a sync removes its Secret
(ESO owns what it creates and never writes into a Secret it didn't —
`creationPolicy: Owner`), so remove the app references first.

## Other providers

The mechanism is provider-agnostic; what differs is how much Orkano has
proven for you:

| Tier | Providers | How to connect |
|---|---|---|
| Flagship (stable, release-tested) | HashiCorp Vault | the dashboard's connect form |
| Stable recipes | AWS, GCP, Azure | author the `SecretStore` with `kubectl` |
| Alpha (community-maintained upstream) | Keeper, 1Password, Doppler | author the `SecretStore` with `kubectl` |

A `kubectl`-authored store follows the same pair as
[`docs/examples/07-secretstore-vault.yaml`](examples/07-secretstore-vault.yaml)
— swap the `provider` block and the credential Secret's keys per
[ESO's provider docs](https://external-secrets.io/latest/provider/aws-secrets-manager/).
Keep the credentials in a Secret named `<store>-credentials`: it matches what
the dashboard writes and what future admission policy will pin. Every store
in `orkano-apps` appears on the Vault page regardless of how it was created;
alpha-tier providers are labeled there, and ESO validates all of them the
same way.
