# `vault-ha`

Three HashiCorp Vault members with Raft (Integrated Storage) HA and
KMS auto-unseal.

When you want **a secrets store that survives a DC outage** and ties
its unseal trust root back to the same KMS your OIDC issuer already
uses.

## What it does

- Creates a dedicated `vault` network (`10.53.0.0/24`, NAT) and a
  `vault-secrets` SG (8200 ingress, 8201 inter-replica Raft, 443
  egress to KMS, 53 DNS).
- Creates **three** micro-VMs (2 vCPU, 4 GiB RAM, 10 GiB root +
  20 GiB `raft` at `/vault/data` + 5 GiB `logs` at `/vault/logs`).
- Each replica auto-unseals at boot from the KMS URI ; no human in
  the loop after the first `operator init`.

## Inputs

| Input                | Required | Secret | Default                | Notes                                            |
|----------------------|----------|--------|------------------------|--------------------------------------------------|
| `image`              | no       | no     | `hashicorp/vault:1.18` | Open-source build only — no enterprise           |
| `oidc_issuer`        | no       | no     | `""`                   | Empty = leave OIDC auth disabled at bootstrap    |
| `unseal_key_kms_uri` | yes      | no     | —                      | `awskms://`, `gcpckms://`, `transit://...`       |
| `audit_log_path`     | no       | no     | `/vault/logs/audit.log`| Audit file path inside the guest                 |
| `cluster_name`       | no       | no     | `weft-vault`           | Raft cluster identifier                          |

## Operator pre-flight

1. **Provision a KMS key.** Any KMS Vault's seal plugins support:
   AWS KMS, GCP KMS, Azure Key Vault, an upstream Vault's transit
   engine, OCI / Aliyun. Grab the URI and **the IAM policy that
   lets the Vault replicas Encrypt/Decrypt** that one key.

2. **Size the quota.** 3 × 2 vCPU + 3 × 4 GiB RAM + 3 × 25 GiB
   persistent storage.

3. **Install.**

   ```
   weft plugin install vault-ha \
     --project security \
     --input unseal_key_kms_uri=awskms:///alias/weft-vault \
     --input oidc_issuer=https://dex.example.com
   ```

4. **Initialize (one-time, post-install).** Replicas come up
   sealed-but-empty ; you still call `vault operator init` once:

   ```
   export VAULT_ADDR=https://vault-ha-<short>-vault-0.weft:8200
   vault operator init -recovery-shares=5 -recovery-threshold=3
   ```

   Vault prints **5 recovery keys + a root token**. With KMS unseal
   those keys are only needed for recovery ops (regenerate root,
   rekey). Stash them — splitting 1-per-DC is a common policy.

5. **Mark the instance initialized** so sibling plugins discover it:
   `weft plugin status vault-ha --set initialized=true`.

## Verify

```
export VAULT_ADDR=https://vault-ha-<short>-vault-0.weft:8200
vault status                       # Initialized:true Sealed:false HA Enabled:true
vault operator raft list-peers     # 3 voter peers, one per DC
```

## What's NOT included

- **Enterprise replication** (Performance / DR Secondary): not in
  the open-source build.
- **Secret-engine bootstrap**: no `kv`, `pki`, `database` engines
  mounted ; do that via `vault secrets enable` after init.
- **Agent sidecars / auto-renew**: clients must use Vault Agent or
  the SDK's token-renew helper.
- **Raft snapshot backup**: schedule `vault operator raft snapshot save` to versitygw-ha or your backup target ; not automated here.
