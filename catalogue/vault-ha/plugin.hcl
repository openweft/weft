# HashiCorp Vault HA — three Vault members in Raft (Integrated
# Storage) HA. Auto-unseal handed off to a KMS the operator provides,
# typically the same KMS / HSM that the OIDC issuer trusts so the
# unseal path piggybacks the existing trust root.
#
# This is the open-source HashiCorp Vault binary (MPL 2.0) ; no
# enterprise features. Replication = Raft only ; no DR cluster, no
# performance secondary. See docs/catalogue/vault-ha.md for the
# bootstrap dance after install (`vault operator init`).
#
# Image : `hashicorp/vault:1.18` upstream — operator can pin a newer
# patch via the `image` input. No openweft fork.
#
# Operator pre-flight (see docs/catalogue/vault-ha.md):
#   1. Provision a KMS key (AWS KMS / GCP KMS / Azure Key Vault /
#      OCI-compatible local) and grab its URI.
#   2. weft plugin install vault-ha \
#        --project security \
#        --input unseal_key_kms_uri=awskms:///alias/weft-vault \
#        --input oidc_issuer=https://dex.example.com

plugin "vault-ha" {
  version     = "v1"
  kind        = "secrets"
  description = "Three Vault members with Raft HA and KMS auto-unseal, one per DC"
  layout      = "ha-3dc"

  # -----------------------------------------------------------------
  # Inputs
  # -----------------------------------------------------------------

  input "image" {
    type    = "string"
    default = "hashicorp/vault:1.18"
    help    = "Vault OCI image. Open-source build only — enterprise replication is out of scope."
  }

  input "oidc_issuer" {
    type    = "string"
    default = ""
    help    = "OIDC issuer URL Vault should trust for the userpass-OIDC auth method. Empty = leave OIDC auth disabled at bootstrap."
  }

  input "unseal_key_kms_uri" {
    type     = "string"
    required = true
    help     = "Auto-unseal KMS URI. Examples: awskms:///alias/weft-vault, gcpckms://projects/p/locations/l/keyRings/r/cryptoKeys/k, transit://https://upstream-vault:8200/transit/keys/weft."
  }

  input "audit_log_path" {
    type    = "string"
    default = "/vault/logs/audit.log"
    help    = "In-guest audit log path. Mounts onto the per-replica `logs` volume so audit survives restarts."
  }

  input "cluster_name" {
    type    = "string"
    default = "weft-vault"
    help    = "Raft cluster identifier. Must match across all three replicas — the plugin stamps it identically."
  }

  # -----------------------------------------------------------------
  # Network
  # -----------------------------------------------------------------

  network "vault" {
    cidr = "10.53.0.0/24"
    type = "nat"
    dns  = ["1.1.1.1", "9.9.9.9"]
  }

  # -----------------------------------------------------------------
  # Security groups
  # -----------------------------------------------------------------

  security_group "vault-secrets" {
    description = "Vault — 8200/tcp API from tenants, 8201/tcp Raft + cluster traffic between replicas, KMS egress."
    networks    = ["vault"]

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8200
      port_max    = 8200
      remote_cidr = "10.0.0.0/8"
      description = "Vault HTTPS API from tenant networks."
    }

    rule "ingress" {
      protocol    = "tcp"
      port_min    = 8201
      port_max    = 8201
      remote_cidr = "10.53.0.0/24"
      description = "Raft cluster port — inter-replica request forwarding + log replication."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 8201
      port_max    = 8201
      remote_cidr = "10.53.0.0/24"
      description = "Outbound Raft."
    }

    rule "egress" {
      protocol    = "tcp"
      port_min    = 443
      port_max    = 443
      remote_cidr = "0.0.0.0/0"
      description = "KMS auto-unseal + OIDC discovery + token endpoint."
    }

    rule "egress" {
      protocol    = "udp"
      port_min    = 53
      port_max    = 53
      remote_cidr = "0.0.0.0/0"
      description = "DNS."
    }
  }

  # -----------------------------------------------------------------
  # VMs
  # -----------------------------------------------------------------

  vm "vault" {
    image    = "hashicorp/vault:1.18"
    replicas = 3
    cpu      = 2
    mem_mb   = 4096
    disk_gb  = 10
    network  = "vault"

    placement {
      az   = "different"
      host = "different"
    }

    env_from "oidc_issuer"        { env_name = "VAULT_OIDC_ISSUER" }
    env_from "unseal_key_kms_uri" { env_name = "VAULT_SEAL_KMS_URI" }
    env_from "audit_log_path"     { env_name = "VAULT_AUDIT_LOG_PATH" }
    env_from "cluster_name"       { env_name = "VAULT_CLUSTER_NAME" }

    volume "raft" {
      size_gib = 20
      format   = "raw"
      mount    = "/vault/data"
    }

    volume "logs" {
      size_gib = 5
      format   = "raw"
      mount    = "/vault/logs"
    }
  }
}
