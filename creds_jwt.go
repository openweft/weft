package weft

// creds_jwt.go grows the per-project credentials story from
// "raw NKey seed" (Phase 2/3 of [[weft-tenant-event-access]]) to
// a proper operator/account/user JWT hierarchy that nats-server
// accepts under `operator: ...` + `resolver: MEMORY` config.
//
// Trust chain :
//
//   operator NKey + JWT (root of trust ; one per Weft deploy)
//        signs ─▶
//   account NKey + JWT (one per project, or one shared for now)
//        signs ─▶
//   user NKey + JWT (one per tenant — what `weft-driver-vz`
//        materialises into <vmDir>/nats.creds for the workload)
//
// Today this file ships the minting helpers ; the rendering
// side that emits the `resolver_preload { ... }` block consumes
// them. The bigger end-state — dex issues these JWTs as part of
// the OIDC flow — comes when [[oidc-server-dex]] gains a NATS
// JWT integration ; the local-minting path stays as the
// single-host / dev / cluster-bootstrap fallback.
//
// Why pure-Go local minting + a future dex path coexist :
//
//   * Bootstrapping a brand-new cluster needs SOMETHING to mint
//     the operator before dex is even up — chicken and egg.
//   * Long-lived production uses dex so user JWTs can be
//     rotated / revoked through the same flow as every other
//     credential.
//   * The mint helpers in this file ARE the dev-mode signer
//     and the bootstrap signer ; the resulting JWTs are
//     wire-compatible with dex-issued ones.

import (
	"fmt"

	jwt "github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// OperatorCreds bundles a freshly-minted operator's NKey +
// signed JWT. The seed is the long-lived secret that signs
// account JWTs ; keep it offline / in a sealed store. The
// public key (`operatorKP.PublicKey()`) becomes the
// `operator: ...` reference in nats-server config.
type OperatorCreds struct {
	JWT    string // decorated JWT (BEGIN/END NATS OPERATOR JWT blocks)
	Seed   []byte // operator-NKey seed (`SO...`)
	Public string // operator-NKey public key (`O...`)
}

// AccountCreds is the per-tenant signing identity. Multiple
// users (Weft projects) can live under one account, or each
// project can have its own — the renderer picks.
type AccountCreds struct {
	JWT    string // decorated JWT signed by the operator
	Seed   []byte // account-NKey seed (`SA...`)
	Public string // account-NKey public key (`A...`)
}

// UserCreds is the leaf identity a workload connects to NATS
// with. The signed JWT goes into the `.creds` file alongside
// the user NKey seed ; the workload's `nats.UserCredentials(...)`
// pulls both from the same file.
type UserCreds struct {
	JWT    string // decorated JWT signed by the account
	Seed   []byte // user-NKey seed (`SU...`)
	Public string // user-NKey public key (`U...`)
}

// MintOperator generates a fresh operator-NKey + signs a
// self-referential operator JWT (operators self-sign). `name`
// is the operator's human-readable identifier (typically the
// cluster name — "weft-prod", "weft-staging", …).
//
// The returned seed must be persisted somewhere safe. Losing
// it means re-issuing every downstream account + user JWT.
func MintOperator(name string) (OperatorCreds, error) {
	kp, err := nkeys.CreateOperator()
	if err != nil {
		return OperatorCreds{}, fmt.Errorf("create operator nkey: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return OperatorCreds{}, fmt.Errorf("operator pubkey: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return OperatorCreds{}, fmt.Errorf("operator seed: %w", err)
	}
	claims := jwt.NewOperatorClaims(pub)
	claims.Name = name
	token, err := claims.Encode(kp)
	if err != nil {
		return OperatorCreds{}, fmt.Errorf("operator encode: %w", err)
	}
	return OperatorCreds{JWT: token, Seed: seed, Public: pub}, nil
}

// MintAccount creates a fresh account-NKey under the given
// operator. `name` is the account label (typically the project
// name or "weft-default" for a shared account).
func MintAccount(operatorSeed []byte, name string) (AccountCreds, error) {
	opKP, err := nkeys.FromSeed(operatorSeed)
	if err != nil {
		return AccountCreds{}, fmt.Errorf("operator seed: %w", err)
	}
	kp, err := nkeys.CreateAccount()
	if err != nil {
		return AccountCreds{}, fmt.Errorf("create account nkey: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return AccountCreds{}, fmt.Errorf("account pubkey: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return AccountCreds{}, fmt.Errorf("account seed: %w", err)
	}
	claims := jwt.NewAccountClaims(pub)
	claims.Name = name
	token, err := claims.Encode(opKP)
	if err != nil {
		return AccountCreds{}, fmt.Errorf("account encode: %w", err)
	}
	return AccountCreds{JWT: token, Seed: seed, Public: pub}, nil
}

// MintUser issues a user JWT signed by the given account. The
// JWT carries subject permissions matching
// [[weft-tenant-event-access]] : subscribe on the project's
// event mirror, publish on the project's app namespace. Pass
// the project's expected subjects via subscribeAllow +
// publishAllow ; the function attaches them to the claims.
//
// `name` is the user identifier (typically `project:<uuid>`).
func MintUser(accountSeed []byte, name string, subscribeAllow, publishAllow []string) (UserCreds, error) {
	accKP, err := nkeys.FromSeed(accountSeed)
	if err != nil {
		return UserCreds{}, fmt.Errorf("account seed: %w", err)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		return UserCreds{}, fmt.Errorf("create user nkey: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return UserCreds{}, fmt.Errorf("user pubkey: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return UserCreds{}, fmt.Errorf("user seed: %w", err)
	}
	claims := jwt.NewUserClaims(pub)
	claims.Name = name
	if len(subscribeAllow) > 0 {
		claims.Sub.Allow.Add(subscribeAllow...)
	}
	if len(publishAllow) > 0 {
		claims.Pub.Allow.Add(publishAllow...)
	}
	token, err := claims.Encode(accKP)
	if err != nil {
		return UserCreds{}, fmt.Errorf("user encode: %w", err)
	}
	return UserCreds{JWT: token, Seed: seed, Public: pub}, nil
}

// FormatCredsFile returns the contents of a NATS `.creds` file
// for the given user identity. Format matches what nats-server's
// `nats.UserCredentials(...)` client option reads :
//
//   -----BEGIN NATS USER JWT-----
//   <jwt>
//   ------END NATS USER JWT------
//
//   ************************* IMPORTANT *************************
//   NKEY Seed printed below ...
//
//   -----BEGIN USER NKEY SEED-----
//   <seed>
//   ------END USER NKEY SEED------
//
// Centralised here so the renderer + the per-VM credentials
// materialiser ([[weft-tenant-event-access]] Phase 2 in
// `adapter.go`) emit identical output.
func FormatCredsFile(u UserCreds) ([]byte, error) {
	return jwt.FormatUserConfig(u.JWT, u.Seed)
}
