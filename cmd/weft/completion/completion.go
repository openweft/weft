// Package completion implements the `weft completion` sub-command, a thin
// wrapper around cobra's built-in shell-completion script generators. The
// generated scripts feed directly into the host shell — operators eval the
// output, or pipe it into the system-wide completion directory at host
// bring-up time (see examples/cloud-init/debian-host.yaml).
//
// Four shells are supported, matching cobra's coverage:
//
//	bash         — GenBashCompletionV2 (v2 emits per-command directive
//	               metadata for `weft <noun>` value completions).
//	zsh          — GenZshCompletion (includes descriptions).
//	fish         — GenFishCompletion.
//	powershell   — GenPowerShellCompletionWithDesc.
//
// The packaging hook in the cloud-init template wires bash + zsh into
// /etc/bash_completion.d and /usr/local/share/zsh/site-functions respectively;
// fish and powershell are operator-on-demand on Debian default installs.
//
// See docs/operations/completion.md for the full operator runbook.
package completion

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// validShells is the set of shells accepted as the single positional
// argument. Kept in sync with the four GenXxxCompletion methods cobra
// ships — adding a shell here without a matching switch case below
// would fall through to the "unreachable" error.
var validShells = []string{"bash", "zsh", "fish", "powershell"}

// Command returns the `weft completion <shell>` cobra command. The
// generated script is written to stdout so operators can either eval it
// directly (`eval "$(weft completion bash)"`) or redirect it into a
// system-wide completion file at bring-up time.
//
// We use MatchAll(ExactArgs(1), OnlyValidArgs) — cobra's ExactValidArgs
// helper is deprecated in favour of this composition (see
// vendor/github.com/spf13/cobra/args.go).
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:       "completion <bash|zsh|fish|powershell>",
		Short:     "Generate shell completion script",
		Long: `Generate the autocompletion script for the named shell.

Quick eval (lazy install, current shell only):

  bash:        eval "$(weft completion bash)"
  zsh:         source <(weft completion zsh)
  fish:        weft completion fish | source
  powershell:  weft completion powershell | Out-String | Invoke-Expression

System-wide install — see docs/operations/completion.md.`,
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: validShells,
		// The completion script generator is intentionally noisy on
		// stderr (cobra emits hints), so we keep usage-on-error off
		// here — a bad shell name surfaces as a clean error message,
		// not a wall of help text.
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			// The script must be generated against the ROOT command
			// so it covers the entire `weft …` tree, not just the
			// completion subcommand itself. cobra walks parents via
			// (*Command).Root(), which returns c when c has no parent
			// — in tests we attach a fake root, in production the
			// `weft` root.
			root := c.Root()
			switch args[0] {
			case "bash":
				// V2 emits per-command directive metadata so
				// dynamic value completions (e.g. project names
				// fetched from the agent in the future) work
				// out of the box. includeDesc=true keeps the
				// short help text alongside each completion.
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				// Unreachable: cobra.OnlyValidArgs filters this
				// out before RunE is called. Keep the branch as
				// a safety net so adding a new shell to
				// validShells without wiring its generator
				// surfaces as a clean runtime error rather
				// than a silent empty script.
				return fmt.Errorf("unsupported shell %q (valid: %v)", args[0], validShells)
			}
		},
	}
	return cmd
}
