# Shell completion

`weft completion <shell>` emits a completion script on stdout for the
named shell. Cobra owns the script generators; we just expose them as a
top-level subcommand so operators can pipe the output anywhere.

Supported shells: `bash`, `zsh`, `fish`, `powershell`.

## Lazy install (current shell only)

Reload-time generation — no files on disk. Drop the line in your shell rc
file once you confirm the script works:

```bash
# bash
eval "$(weft completion bash)"

# zsh — needs compinit loaded first
autoload -Uz compinit && compinit
source <(weft completion zsh)

# fish
weft completion fish | source

# powershell
weft completion powershell | Out-String | Invoke-Expression
```

## Persistent install (system-wide)

### bash

```bash
weft completion bash | sudo tee /etc/bash_completion.d/weft >/dev/null
```

The cloud-init template at `examples/cloud-init/debian-host.yaml`
already runs this line at host bring-up time — every fresh weft host
ships with bash completion enabled.

### zsh

```bash
weft completion zsh | sudo tee /usr/local/share/zsh/site-functions/_weft >/dev/null
```

Cloud-init also handles this when `/usr/bin/zsh` exists on the host.
Make sure `fpath=(/usr/local/share/zsh/site-functions $fpath)` runs
before `compinit` in your zshrc.

### fish

```bash
weft completion fish > ~/.config/fish/completions/weft.fish
```

Not wired into cloud-init — fish is not part of the Debian default
install. Operators who run fish add the line by hand.

### powershell

```powershell
weft completion powershell | Out-File -Encoding utf8 `
  $PROFILE.CurrentUserAllHosts
```

Not wired into cloud-init — weft hosts are Linux, powershell completion
is an operator-workstation concern.

## Helm chart

The `weft-agent` Helm chart does NOT install completion scripts inside
the container image — the chart deploys a daemon, not an interactive
shell. Operators who exec into the pod can still run
`weft completion bash` ad hoc.

## Troubleshooting

Regenerate after every upgrade — the script reflects the binary's
subcommand tree at the time of generation.
