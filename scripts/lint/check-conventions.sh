#!/usr/bin/env -S pkgx bash
#
# check-conventions.sh
#
# Enforces openweft's documented project conventions. Run locally via
# pre-commit, and in CI via .github/workflows/conventions.yml.
#
# Rules enforced:
#   R1  cobra-only CLIs: any *.go under cmd/ that imports "flag" without
#       importing spf13/cobra fails (test files are exempt).
#   R2  shell shebangs: *.sh outside image/ should use
#       "#!/usr/bin/env -S pkgx bash" (warning only — cloud-init runcmd
#       and other legitimate exceptions exist; we never hard-fail here).
#   R3  no auto-publish on push to main: any *.yml under
#       .github/workflows whose name contains release|image|publish|oci
#       must NOT use `push: branches: [main]` without an accompanying
#       `tags:` filter (which scopes the publish to version tags).
#   R4  terraform-provider-weft uses plugin-framework only — any *.go
#       under a path matching tfprovider|terraform-provider-weft that
#       imports hashicorp/terraform-plugin-sdk/v2 fails.
#
# Usage:
#   bash scripts/lint/check-conventions.sh [-v]
#
# Exit codes:
#   0   no failures
#   1   one or more failures
#
# Portable across Linux and macOS (no GNU-only sed/grep extensions).

set -u

VERBOSE=0
if [ "${1:-}" = "-v" ] || [ "${1:-}" = "--verbose" ]; then
    VERBOSE=1
fi

# Anchor to repo root regardless of caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"

FAILURES=0
WARNINGS=0

log_verbose() {
    if [ "$VERBOSE" = "1" ]; then
        printf 'check: %s\n' "$1"
    fi
}

fail() {
    # fail <file> <line> <rule>
    printf 'FAIL %s:%s -- %s\n' "$1" "$2" "$3" >&2
    FAILURES=$((FAILURES + 1))
}

warn() {
    printf 'WARN %s:%s -- %s\n' "$1" "$2" "$3" >&2
    WARNINGS=$((WARNINGS + 1))
}

###############################################################################
# Rule 1 -- cobra-only CLIs
###############################################################################
check_go_cli_uses_cobra() {
    # Enumerate *.go files under cmd/, excluding vendor and tests.
    if [ ! -d cmd ]; then
        return 0
    fi

    # find -print0 + while-read is portable across BSD and GNU find.
    while IFS= read -r -d '' f; do
        case "$f" in
            */vendor/*) continue ;;
            *_test.go)  continue ;;
        esac
        log_verbose "$f"
        # Look for `"flag"` import line. We only care if cobra is absent.
        if grep -Eq '^[[:space:]]*"flag"[[:space:]]*$' "$f"; then
            if ! grep -q 'spf13/cobra' "$f"; then
                line=$(grep -n '^[[:space:]]*"flag"[[:space:]]*$' "$f" | head -1 | cut -d: -f1)
                fail "$f" "${line:-1}" "R1 cobra-only CLIs: cmd/ file imports flag without cobra (see feedback_cli_cobra)"
            fi
        fi
    done < <(find cmd -type f -name '*.go' -print0 2>/dev/null)
}

###############################################################################
# Rule 2 -- shell shebangs (warn-only)
###############################################################################
check_shell_shebangs() {
    while IFS= read -r -d '' f; do
        case "$f" in
            ./vendor/*)    continue ;;
            ./linux/*)     continue ;;
            ./image/*)     continue ;;   # in-VM init scripts: intentional bash
            ./.git/*)      continue ;;
        esac
        log_verbose "$f"
        # Read first line without invoking external head (portable).
        first_line=""
        IFS= read -r first_line < "$f" || true
        case "$first_line" in
            '#!/usr/bin/env -S pkgx bash') ;;  # canonical
            '#!'*)
                warn "$f" 1 "R2 shell shebang is '${first_line}' -- prefer '#!/usr/bin/env -S pkgx bash' (see feedback_pkgx_bash)"
                ;;
            *)
                # No shebang at all -- ignore (likely sourced).
                ;;
        esac
    done < <(find . -type f -name '*.sh' -print0 2>/dev/null)
}

###############################################################################
# Rule 3 -- no auto-publish on push to main
###############################################################################
check_no_autopublish_dev() {
    wf_dir=".github/workflows"
    if [ ! -d "$wf_dir" ]; then
        return 0
    fi
    for f in "$wf_dir"/*.yml "$wf_dir"/*.yaml; do
        [ -e "$f" ] || continue
        log_verbose "$f"
        # Only police publishing workflows by filename heuristic.
        case "$(basename "$f")" in
            *release*|*image*|*publish*|*oci*) ;;
            *) continue ;;
        esac
        # Look for a `push:` trigger block that pins `main` without
        # also constraining to `tags:`. The awk state machine tracks
        # indentation: we enter the push: block, record its indent, and
        # leave when we see a line at <= that indent.
        violation=$(awk '
            function indent(s,   i) {
                i = 0
                while (i < length(s) && substr(s, i+1, 1) == " ") i++
                return i
            }
            /^on:/ { in_on = 1; on_indent = 0; next }
            in_on && /^[^[:space:]#]/ && !/^on:/ { in_on = 0 }
            {
                ind = indent($0)
            }
            in_on && !in_push && $0 ~ /^[[:space:]]+push:[[:space:]]*$/ {
                in_push = 1; push_indent = ind; push_line = NR
                has_tags = 0; has_main = 0; next
            }
            in_push && $0 !~ /^[[:space:]]*$/ && ind <= push_indent {
                if (has_main && !has_tags) print push_line
                in_push = 0
                has_tags = 0; has_main = 0
            }
            in_push && /branches:/ && /main/ { has_main = 1 }
            in_push && /^[[:space:]]+-[[:space:]]+["'\'']?main["'\'']?[[:space:]]*$/ { has_main = 1 }
            in_push && /tags:/ { has_tags = 1 }
            END {
                if (in_push && has_main && !has_tags) print push_line
            }
        ' "$f")
        if [ -n "$violation" ]; then
            fail "$f" "$violation" "R3 publishing workflow triggers on push to main without a tags: filter (see feedback_no_autopublish_dev)"
        fi
    done
}

###############################################################################
# Rule 4 -- terraform-provider-weft uses plugin-framework only
###############################################################################
check_tfprovider_framework() {
    while IFS= read -r -d '' f; do
        case "$f" in
            *terraform-provider-weft*|*tfprovider*) ;;
            *) continue ;;
        esac
        case "$f" in
            */vendor/*) continue ;;
        esac
        log_verbose "$f"
        if grep -nq 'hashicorp/terraform-plugin-sdk/v2' "$f"; then
            line=$(grep -n 'hashicorp/terraform-plugin-sdk/v2' "$f" | head -1 | cut -d: -f1)
            fail "$f" "${line:-1}" "R4 terraform-provider must use plugin-framework, not sdk/v2 (see project_tfprovider_framework)"
        fi
    done < <(find . -type f -name '*.go' -print0 2>/dev/null)
}

check_go_cli_uses_cobra
check_shell_shebangs
check_no_autopublish_dev
check_tfprovider_framework

if [ "$FAILURES" -gt 0 ]; then
    printf '\n%s\n' "convention lint: $FAILURES failure(s), $WARNINGS warning(s)" >&2
    exit 1
fi

printf '%s\n' "convention lint: clean ($WARNINGS warning(s))"
exit 0
