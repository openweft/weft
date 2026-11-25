# secretsource

Resolve a secret (a FIDO2 PIN, a container password, a recovery code) from a
**reference** instead of a plaintext file left on disk.

```go
s, err := secretsource.Open(ctx, "keepassxc:https://fido.invalid/yubikey-5c")
if err != nil { ... }
defer s.Close()          // overwrites the bytes
use(s.Bytes())
```

| Reference | Reads |
|---|---|
| `file:PATH` | a regular file readable by its owner alone, **not a symbolic link** (Unix: opened with `O_NOFOLLOW`, owned by the calling user, mode 0600 or stricter; Windows: opened with `FILE_FLAG_OPEN_REPARSE_POINT` and refused if it is a reparse point, and an access list that grants no read or write right to anyone but the owner, SYSTEM and Administrators); one trailing newline is stripped |
| `keychain:SERVICE/ACCOUNT` | a macOS Keychain generic-password item (pure Go, [go-macos/keychain](https://github.com/go-macos/keychain)) |
| `secretservice:SERVICE/ACCOUNT` | a freedesktop Secret Service item with attributes `service=SERVICE`, `account=ACCOUNT` |
| `secretservice:KEY=VALUE[,KEY=VALUE...]` | the one Secret Service item carrying all those attributes, whatever else it has (a KeePassXC entry exposed by its Secret Service integration: `Title=...`, `UserName=...`; or `secret-tool store`). No escaping: keys and values hold no `,`. Several matches are refused |
| `keepassxc:URL` | the KeePassXC entry whose **URL field** is that URL (any `scheme://host...`; `https://NAME.invalid/...` is a good, never-resolving choice), through KeePassXC's browser-integration protocol. Two entries with the same URL are refused as ambiguous |
| `systemd-creds:NAME` | (Linux) a systemd credential, returned **byte for byte** (no trailing-newline stripping, unlike `file:`); see [systemd credentials](#systemd-credentials-systemd-credsname). First the file `NAME` in `$CREDENTIALS_DIRECTORY` (what `LoadCredential=`, `LoadCredentialEncrypted=` and `SetCredentialEncrypted=` place there): opened with `O_NOFOLLOW`, a regular file, owned by root or the caller, no access for "other" (group bits are accepted: systemd gives a `User=` service a root-owned `0440` file with an ACL), the directory likewise. If the variable is unset or the directory lacks `NAME`, the credential `NAME` or `NAME.cred` is found in `/etc`, `/run`, `/usr/lib` `credstore.encrypted` and decrypted by `systemd-creds decrypt --name=NAME PATH -` (run directly, no shell, empty environment, secret on stdout only, output capped at 4096 bytes; a non-root caller tries `--user`, systemd 256 or later, then system scope). `NAME` is a plain file name: no `/`, not `.` or `..`, no control character, no `.cred` suffix, 255 bytes at most. A symlink, a loose mode or a name that does not match the one embedded in the credential is refused, not skipped. Refused with `ErrUnavailable` on other systems |

`keepassxc:by-uuid/...` and `keepassxc:by-path/...` (KeePassXC's own lookups) are
**refused**: KeePassXC 2.8.0 crashes (SIGSEGV, `BrowserService::sortPriority`,
`std::max_element` of an empty list) when a lookup finds an entry with no URL,
and a PIN entry often has none. Found by URL, an entry has one by construction.

A reference never holds the secret, so it may sit in a flag, a script or a
shell history. There is **no scheme that runs a command**: a flag must never be
able to start a process. A reference with no scheme is refused, not guessed to
be a path (`file:` is how a path is written).

**Migrating from v0.8 (symlinked secret files).** From v0.9.0 `file:` refuses a
symbolic link in the last component of the path (dangling or not), and on Unix a
file the calling user does not own. A link could point at a 0600 file of another
account, or be planted in a writable directory. If you used
`file:/etc/app/pin` with `pin -> /run/secrets/pin`, write `file:/run/secrets/pin`
(`readlink -f` prints the real path). Links in the directories before the last
component are still followed.

Bounds: a secret longer than 4096 bytes is refused; an empty one from a manager
is refused; a call ends with its context (2 minutes when the context has no
deadline) even when the manager is waiting for you to unlock it, and a late
answer is wiped, never used. Errors are typed: `ErrNotFound`, `ErrAmbiguous`,
`ErrUnavailable` (no manager, locked, unlock dismissed), `ErrNotAssociated`,
`ErrDenied`, `ErrBadRef`, `ErrScheme`, `ErrEmpty`, `ErrTooLarge`. (The Go
runtime keeps a copy of a secret that crossed a JSON string until the garbage
collector reclaims it; the buffers this package owns are wiped.)

## systemd credentials (`systemd-creds:NAME`)

Grammar, exactly: the scheme `systemd-creds` in lower case, a colon, then `NAME`,
the credential name as given to `LoadCredential=` / `LoadCredentialEncrypted=` /
`SetCredentialEncrypted=` / `systemd-creds encrypt --name=`. For example
`password_from = "systemd-creds:smtp"`.

- `NAME` is a plain file name of 1 to 255 bytes. Letters, digits, `.`, `-` and `_`
  are what systemd credential names use; other characters pass through, except
  `/`, NUL and control characters, which are refused.
- No path (`systemd-creds:/etc/credstore.encrypted/smtp` is refused), not `.` or
  `..`, and no `.cred` suffix (`systemd-creds:smtp.cred` is refused): the
  suffix belongs to the file, the reference names the credential. `smtp` finds
  the file `smtp` or `smtp.cred`.

Resolution:

1. `$CREDENTIALS_DIRECTORY/NAME` (what the service manager placed there). Opened
   with `O_NOFOLLOW`, a regular file, owned by root or the caller, no access for
   "other"; the directory likewise. Group bits are accepted: systemd gives a
   `User=` service a root-owned `0440` file with an ACL for that user (measured
   on systemd 257). A symlink or a loose mode is refused, never skipped.
   `$CREDENTIALS_DIRECTORY` itself must be an absolute path with no access for
   "other" (a mode with any `o` bit, such as 0755, is refused with
   `ErrUnavailable`); systemd creates it as 0700, so a test fixture should make
   the directory 0700 too.
2. If the variable is unset or the directory lacks `NAME`: `NAME` or `NAME.cred`
   in `/etc/credstore.encrypted`, `/run/credstore.encrypted`,
   `/usr/lib/credstore.encrypted`, decrypted by `systemd-creds decrypt
   --name=NAME PATH -`, run directly (no shell, empty environment, no stdin), the
   secret read from stdout only, never from argv or the environment, at most 4096
   bytes, ended with the context. A non-root caller tries `--user` (systemd 256
   or later) first, then system scope. A credential whose embedded name differs
   from `NAME` is refused by systemd-creds.
3. Any other system: `ErrUnavailable`.

**No trimming.** `Open("systemd-creds:NAME")` returns the exact bytes: a trailing
newline, NUL bytes and non-UTF-8 bytes are kept, so keytabs, PEM keys and HMAC
keys survive. This differs from `file:`, which strips one trailing newline.
There is one entry point, `Open`; for `systemd-creds:` it never trims. Create a
password credential without a newline: `printf %s 's3cret' | systemd-creds
encrypt --name=smtp - /etc/credstore.encrypted/smtp.cred`. A credential of zero
bytes is `ErrEmpty`; one longer than 4096 bytes is `ErrTooLarge`.

### Choosing what a credential is sealed to

`systemd-creds encrypt --with-key=` takes `host`, `tpm2`, `host+tpm2`, `null`
and `auto` (the TPM2 key when a TPM2 device is found, else the host key on
persistent media). **`--tpm2-pcrs=` defaults to no PCRs.** Binding to PCRs makes
the credential unreadable whenever those measurements change, and the service
then fails closed:

- `--tpm2-pcrs=7` binds to the Secure Boot state: a firmware or shim update, or
  a VM migration on a vTPM, can make every credential sealed that way
  unreadable. Do not use a bare `--tpm2-pcrs=7` without a way back (below).
- `--with-key=host+tpm2` with no `--tpm2-pcrs`: the key derives from the TPM2 chip
  and the host key file (`/var/lib/systemd/credential.secret`); it survives
  firmware updates, and is lost with the TPM2 chip or that file.
- A signed PCR policy: `--tpm2-public-key=PATH` (a PEM RSA public key;
  `--tpm2-public-key-pcrs=`, default PCR 11), with signatures made by
  `systemd-measure` (`--tpm2-signature=`). A new boot component gets a new
  signature; credentials need no re-sealing.
- `systemd-pcrlock` predicts PCR values into a TPM2 policy: before a firmware
  update run `systemd-pcrlock unlock-firmware-code`, afterwards `make-policy`
  (see its manual; `systemd-cryptenroll` and `systemd-repart` take it with
  `--tpm2-pcrlock=`; check that your `systemd-creds` does before relying on it).
- **Re-sealing**: keep the secret in a place that does not depend on this
  machine (an offline copy) and, when decryption fails, run `systemd-creds
  encrypt` again from that copy. A failed decryption returns `ErrUnavailable`,
  says the credential may need re-sealing, and carries systemd-creds' own
  diagnostic, never the secret.

## KeePassXC (macOS, Linux)

KeePassXC has no Secret Service on macOS; it does have a browser-integration
socket, which this package speaks as a client: NaCl `crypto_box` over a local
Unix socket (`$TMPDIR`-independent per-user temporary directory on macOS,
`$XDG_RUNTIME_DIR/app/org.keepassxc.KeePassXC/` on Linux), after an association
that you approve once in KeePassXC. Windows (a named pipe) is not implemented
yet. The protocol was read from KeePassXC's `src/browser/` (BrowserHost,
BrowserShared, BrowserAction, BrowserMessageBuilder, BrowserService).

1. In KeePassXC: *Settings > Browser Integration > Enable browser integration*.
   Leave the database open (or let the request unlock it).
2. Associate this machine, once per database:

   ```
   go install github.com/go-authn/secretsource/cmd/secretsource@latest
   secretsource keepassxc associate
   ```

   KeePassXC asks you to name the connection (say `laptop`) and accept. The
   association (that name and an identification public key; no password) is kept
   in the macOS Keychain, in the Secret Service on Linux when one answers, and
   otherwise in a 0600 file under your configuration directory. A lookup never
   creates an association; without one it fails with `ErrNotAssociated`.
3. Create the entry (Title `Yubikey PIN`, Password = the PIN) and **fill its URL
   field** with `https://fido.invalid/yubikey-5c` (any URL not used by another
   entry: the reference is that URL). Then check the reference without printing
   anything:

   ```
   secretsource check 'keepassxc:https://fido.invalid/yubikey-5c'
   ```

   The first read of an entry shows KeePassXC's access dialog: allow it, and
   tick *Remember* to stop being asked.
4. Use it: `fido fingerprint list --pin-from 'keepassxc:https://fido.invalid/yubikey-5c'`.

KeePassXC answers whichever database is open; with a locked database it opens
its unlock dialog and waits for you (up to the 2-minute bound).

## Taking a secret from a command-line flag: `secretsource/secretflag`

A secret passed as a flag value is visible in `ps`. The `secretflag` package
gives a command the pair `--<name>` (deprecated, warns on stderr) and
`--<name>-from <ref>`, mutually exclusive, with the reference resolved by
`secretsource.Open`:

```go
var password string
b := secretflag.Add(cmd, "password", &password, "the admin password") // cobra
// or: secretflag.AddStd(fs, os.Stderr, "password", &password, "the admin password", "p")
// ... after parsing:
if err := secretflag.Resolve(ctx, b); err != nil { return err }
```

A secret that never had a plain flag registers only `--<name>-from`:
`secretflag.AddFrom(cmd, "seal-key", &key, "the sealing key")`, or
`secretflag.AddFromList(cmd, "seal-old-key", &olds, "previous keys")` for a
repeatable flag (key rotation).

## Storing a secret: `secretsource store`

No entry has to be made by hand:

```sh
secretsource store 'keepassxc:https://fido.invalid/yubikey-5c'        # typed twice, echo off
secretsource store 'keepassxc:https://fido.invalid/yubikey-5c' --generate 24   # draws 24 letters and digits, prints nothing secret
printf '%s' "$PIN" | secretsource store keychain:fido/yubikey-5c       # from stdin when it is not a terminal
```

The secret is never taken from an argument or the environment. An existing entry is refused
unless `--replace` (for KeePassXC that updates *that* entry, found by its URL, in place).
`keychain:`, `secretservice:SERVICE/ACCOUNT` and `secretservice:KEY=VALUE[,...]` write an item
(the last carries exactly those attributes; to replace one that has more attributes, name them
all, since the Secret Service replaces only an item with the same attributes); `file:` is
refused (no more plaintext files).

For `keepassxc:` it uses the browser protocol's `set-login` (KeePassXC 2.8.0:
`BrowserAction::handleSetLogin`, `BrowserService::addEntry`/`updateEntry`), so the entry always
carries the URL that finds it again. The login defaults to the URL's host (`--login`); 2.8.0 titles
the new entry after that host and ignores a title (BrowserService.cpp:891), so `--title` is refused;
the group is `KeePassXC-Browser Passwords` (created under the root) unless `--group Keys/FIDO`
(names below the root). The duplicate check is a lookup by URL, never by uuid or path. KeePassXC
shows its own dialogs (an access request for the existing entry, an update confirmation) and saves
the database according to its settings.

## Deleting an entry: `secretsource.Delete`

```go
err := secretsource.Delete(ctx, "keychain:fido/yubikey-5c")
if errors.Is(err, fs.ErrNotExist) { /* already gone: your call whether that is success */ }
```

| Reference | What `Delete` does |
| --- | --- |
| `keychain:SERVICE/ACCOUNT` | removes the generic-password item |
| `secretservice:SERVICE/ACCOUNT` | removes the item with `service=SERVICE`, `account=ACCOUNT`; if several match, `ErrAmbiguous` and nothing is deleted |
| `keepassxc:URL` | the one entry whose URL field is URL, by the browser protocol's `delete-entry` (KeePassXC 2.8.0: `BrowserAction::handleDeleteEntry`, `BrowserService::deleteEntry`); KeePassXC asks you to confirm, then moves it to the database's Recycle Bin, where the password stays until the bin is emptied; several entries: `ErrAmbiguous`, nothing deleted |
| `file:`, `systemd-creds:`, `secretservice:KEY=VALUE` | `ErrNoDelete`: this package never manages a file or credential it did not store, and an attribute lookup names no single item |

An absent entry is an error that is both `ErrNotFound` and `fs.ErrNotExist`. The managers have
no "does it exist" call, so the entry is looked up first, as `Store` does before it replaces
one: a keychain or Secret Service value passes through memory and is wiped at once, a KeePassXC
reply is decoded without its password field. The secret is never returned, printed or logged.

## Storing a secret by hand

macOS Keychain: `security add-generic-password -s fido -a yubikey-5c -w` (`-w`
last, with no value, asks on the terminal; the secret is not in argv), then
`keychain:fido/yubikey-5c`.

Linux: `secret-tool store --label "yubikey PIN" service fido account yubikey-5c`,
then `secretservice:fido/yubikey-5c`.

Used by `fido --pin-from` ([go-authn/fido](https://github.com/go-authn/fido))
and `theca --pin-from/--password-from/--recovery-from`
([go-theca/theca](https://github.com/go-theca/theca)).

BSD 3-Clause.
