package main

// attest_cli.go is the OPERATOR side of the attestation lifecycle : the
// `weft attest trust-ek <ek-pub-file>` admin command that enrols a node's
// Endorsement Key public area into the control-plane's EK registry. This is
// the PREREQUISITE for a TPM-bearing node (running `weft agent --client
// --attest-tpm`) to complete the Enroll handshake : the verifier's
// Verifier.Enroll rejects any node whose EK is not on this allowlist
// (attest.ErrUnknownEK), so the operator must trust the EK out-of-band first.
//
// Flow :
//
//  1. The node operator captures the node's EK public area — the raw bytes
//     attest.Node.EnrollRequest().EKPub carries (a marshalled ECC
//     TPMT_PUBLIC). On the node this is what `weft agent --attest-tpm` would
//     present ; an operator can dump it ahead of time from the same TPM.
//  2. The cluster operator runs, against the control plane's storage backend:
//
//     weft attest trust-ek --storage-backend etcd \
//     --etcd-endpoints host:2379 ./node-ek.pub
//
//  3. The node's subsequent Enroll succeeds, it completes Admit, and
//     RegisterHost (with --attestation-enabled on the server) accepts it.
//
// The EK registry is the SAME etcd-backed weft.EKRegistry the daemon loads
// (weftAttestGateFromConfig) under the "attest" KV namespace, so a TrustEK
// written here is immediately visible to a running control plane (it watches
// the prefix). The file backend has no per-record KV store, so — exactly
// like --attestation-enabled itself — this command requires an etcd /
// embed-etcd backend.

import (
	"context"
	"fmt"
	"os"

	weft "github.com/openweft/weft"
	"github.com/spf13/cobra"
)

// newAttestCmd builds the `weft attest` admin command group. Today it holds
// trust-ek ; future operator surfaces (list-eks, revoke-ek) hang off the
// same parent.
func newAttestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attest",
		Short: "TPM remote-attestation operator commands (EK enrolment).",
		Long: `Operator-side commands for the TPM remote-attestation host-admission
gate. The gate itself runs in the control plane (weft agent
--attestation-enabled) ; these commands manage the EK trust allowlist a
node must be on before it can attest.`,
	}
	cmd.AddCommand(newTrustEKCmd())
	return cmd
}

// newTrustEKCmd builds `weft attest trust-ek <ek-pub-file>`.
func newTrustEKCmd() *cobra.Command {
	var (
		configDir      string
		storageBackend string
		etcdEndpoints  []string
		etcdUsername   string
		etcdPassword   string
		etcdKeyPrefix  string
	)
	cmd := &cobra.Command{
		Use:   "trust-ek <ek-pub-file>",
		Short: "Enrol a node's EK public area into the control-plane trust allowlist.",
		Long: `Reads a node's Endorsement Key public area (the raw bytes the node's
attest handshake presents as EnrollRequest.EKPub) from <ek-pub-file> and
adds it to the control plane's EK registry. After this the node may run the
Enroll/Admit handshake (weft agent --client --attest-tpm) and register.

Requires an etcd or embed-etcd storage backend — the file backend has no
per-record KV store for the EK registry.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ekPub, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read EK pub file %q: %w", args[0], err)
			}
			if len(ekPub) == 0 {
				return fmt.Errorf("EK pub file %q is empty", args[0])
			}

			// Reuse the daemon's storage resolution so the EK registry lands
			// in exactly the namespace a running control plane reads.
			t := fileConfigTargets{
				configDir:      configDir,
				storageBackend: storageBackend,
				etcdEndpoints:  etcdEndpoints,
				etcdUsername:   etcdUsername,
				etcdPassword:   etcdPassword,
				etcdKeyPrefix:  etcdKeyPrefix,
			}
			sf, err := buildStorageFactory(t)
			if err != nil {
				return fmt.Errorf("open storage: %w", err)
			}
			defer func() { _ = sf.close() }()
			if sf.newKV == nil {
				return fmt.Errorf("trust-ek requires an etcd storage backend (--storage-backend etcd|embed-etcd) for the EK registry; the file backend has no per-record KV store")
			}

			ctx := context.Background()
			reg, err := weft.LoadEKRegistry(ctx, sf.newKV("attest"))
			if err != nil {
				return fmt.Errorf("load EK registry: %w", err)
			}
			if reg.Trusted(ekPub) {
				fmt.Fprintf(cmd.OutOrStdout(), "EK already trusted (%d bytes); no change.\n", len(ekPub))
				return nil
			}
			if err := reg.TrustEK(ekPub); err != nil {
				return fmt.Errorf("trust EK: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Trusted EK (%d bytes). The node may now enrol.\n", len(ekPub))
			return nil
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", "state/hcl", "Path to the weft config/state directory (embed-etcd data dir roots here).")
	cmd.Flags().StringVar(&storageBackend, "storage-backend", "", `Storage backend the control plane uses: "etcd" or "embed-etcd". The "file" backend is unsupported for the EK registry.`)
	cmd.Flags().StringSliceVar(&etcdEndpoints, "etcd-endpoints", nil, "etcd endpoints (host:port), required when --storage-backend etcd.")
	cmd.Flags().StringVar(&etcdUsername, "etcd-username", "", "etcd username (optional).")
	cmd.Flags().StringVar(&etcdPassword, "etcd-password", "", "etcd password (optional).")
	cmd.Flags().StringVar(&etcdKeyPrefix, "etcd-key-prefix", "", "etcd key prefix (optional; must match the control plane).")
	return cmd
}
