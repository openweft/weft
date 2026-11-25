# Changelog

## v0.12.0

- `secretflag.AddFrom` registers only `--<name>-from` (no deprecated plain
  twin), for a secret introduced after the plain flag was deprecated.
- `secretflag.AddFromList` registers a repeatable `--<name>-from`; `Resolve`
  appends every resolved secret to a `*[]string`, in order (key rotation).
- Same trimming, empty-secret refusal and value-free errors as `Add`.

## v0.11.0

- New `Delete(ctx, ref) error`, the counterpart of `Store`, so a caller that
  keeps a token in the keychain can remove it on logout. `keychain:` and
  `secretservice:SERVICE/ACCOUNT` remove the item (several Secret Service
  matches: `ErrAmbiguous`, nothing deleted); `keepassxc:URL` uses the browser
  protocol's `delete-entry` (BrowserAction.cpp:105, handleDeleteEntry :473,
  BrowserService.cpp:980 in 2.8.0): KeePassXC asks for confirmation and moves
  the entry to its Recycle Bin, so the password stays in the database until
  the bin is emptied.
- An absent entry is an error that is both `ErrNotFound` and `fs.ErrNotExist`,
  so "already gone" can be treated as success on purpose.
- New `ErrNoDelete`: `file:`, `systemd-creds:` and `secretservice:KEY=VALUE`
  are refused (this package never manages a file or credential it did not
  store, and an attribute lookup names no single item).
- The entry is looked up before it is deleted (the managers have no existence
  call); a keychain or Secret Service value is wiped at once and a KeePassXC
  reply is decoded without its password. Nothing secret is returned or logged.

## v0.10.0

- New package `github.com/go-authn/secretsource/secretflag`: a secret flag
  `--<name>` (deprecated, visible in `ps`) with its `--<name>-from <ref>` twin,
  mutually exclusive, resolved through `secretsource.Open`. `Add` registers
  the pair on a cobra command, `AddStd` on a standard-library `flag.FlagSet`
  (with aliases such as `-t`); `Resolve` reads the references. It replaces the
  copies in weft-ha-forgejo, weft-ha-irods and go-signage. No new dependency:
  cobra was already required.

## v0.9.0

- **Behaviour change, security:** `file:` no longer follows a symbolic link at
  the last component. On Unix it opens with `O_NOFOLLOW` (the kernel refuses a
  link, dangling or not, so there is no window between a check and the open);
  the error says the path is a symlink and to point at the file it names, and
  never contains the secret. A link could reach a 0600 file owned by another
  account, or be planted in a writable directory. Three downstream modules had
  added their own `Lstat` refusal for this.
- On Unix the opened file must also be owned by the calling user (effective
  uid); until now only the mode was checked. Root is not exempted: a root-owned
  0600 file cannot be opened by anyone else, and a root process should not take a
  secret from another account's file.
- Windows: the file is opened with `FILE_FLAG_OPEN_REPARSE_POINT` and refused if
  its attributes on the open handle show a reparse point (a symbolic link, a
  junction, a mount point), before the v0.7.0 access-list check. Gap: a reparse
  point of a kind some tool uses on ordinary files (for example a cloud-files
  placeholder) is refused too; point at a plain local copy.
- No no-follow read primitive exists in go-authn/harden `privfile` (v0.2.0 has
  `Write` only), so the open lives here, beside `openNoFollow`.
- Documentation: the README states what `systemd-creds:` requires of
  `$CREDENTIALS_DIRECTORY` (absolute, no access for "other"; systemd makes it
  0700).
- **Migrating:** if a `file:` reference goes through a symlink, replace the path
  by the file the link names (`readlink -f PATH`). Symlinked directories earlier
  in the path still work.

## v0.8.1

- The KeePassXC association file is written by
  `github.com/go-authn/harden/privfile` v0.2.0 (the temporary file, sync and
  rename moved there from `writeAtomic`). On Windows the file now gets an
  owner-only protected DACL, as in go-authn/kinit v0.3.2; the directory is
  checked again at the write, as well as before it.

## v0.8.0

- New scheme `systemd-creds:NAME` (Linux): a systemd credential, read from
  `$CREDENTIALS_DIRECTORY/NAME` or, failing that, decrypted from a
  `credstore.encrypted` directory with `systemd-creds decrypt`, so a secret
  never has to be written in plaintext on disk (a TPM2- or host-key-sealed
  credential with `LoadCredentialEncrypted=`). Measured on systemd 257 (Debian
  13, aarch64) with `--with-key=host`; not measured with a TPM2 key.
- Binary-safe: the bytes come back exactly as stored (no trailing-newline
  stripping, unlike `file:`), so keytabs, PEM keys and HMAC keys survive; tested
  with a payload holding every byte value, a NUL and a trailing newline.
- A failed decryption says the credential may need re-sealing; the README
  explains the PCR-binding trade-offs (`--tpm2-pcrs=7` can lock every
  credential after a firmware update) and the ways back.
- `NAME` is validated (no `/`, `.`, `..`, NUL or control characters, at most
  255 bytes) and fuzzed (`FuzzCredName`, corpus committed).

## v0.7.0

Fixes #18.

- `file:` works on Windows. It was always refused there, because the Go runtime
  synthesises a 0666 mode that says nothing about who can read the file. The
  access list of the open file is read instead (`GetSecurityInfo` on the handle,
  so the file cannot be swapped between check and read) and the file is refused
  when any entry grants read, write, append, delete or ownership rights to a SID
  other than the owner, SYSTEM and the Administrators group, when the list is
  null, or when an entry cannot be read (object entries). Deny entries are
  ignored, which can only refuse more; entries that only inherit to children are
  skipped; inherited entries are already in the list read from the handle.
  The owner is not itself required to be the current user, as on Unix.
- Tested on Windows 11 ARM64 and in CI with real files (a private file is read,
  one readable by Everyone, Authenticated Users or Users is refused).

## v0.6.1

Fixes #16. No API change.

- `keepassxc:` association file (the fallback store, used when neither the macOS
  Keychain nor a Secret Service is there): the key is written to a new temporary
  file in the same directory and renamed onto the final name, so a symbolic link
  left at that name is replaced, not followed, and a file of looser mode comes out
  0600. On Unix the directory is checked first, as the socket's is: owned by this
  user, not writable by group or others, not a symbolic link; otherwise nothing is
  written.
- Tests: every guard that a mutation sweep found unheld (four survivors) is now
  asserted by the error it returns on both sides; nine guards that only a nil
  dereference after the missing check turned red remain, see the pull request.

## v0.6.0

Security audit 2026-11-21.

**Upgrade:** a `file:` reference whose file is empty (or holds only a line ending)
now fails with `ErrEmpty` instead of returning an empty secret; a caller that relied
on an empty file meaning "no secret" must handle that error. The legacy
`$XDG_RUNTIME_DIR/…BrowserServer` symlink is no longer followed by `keepassxc:`.

- `keepassxc:` (Unix): the socket is refused when it is a symbolic link, or when its
  directory is owned by another user or writable by group or others (on Linux the
  fallback is `/tmp/runtime-$USER`, which anyone can pre-create), and the connected
  peer's uid is checked after the dial (`SO_PEERCRED`, `LOCAL_PEERCRED`; macOS and
  Linux), so a path re-pointed between the check and the dial is caught. The legacy
  `$XDG_RUNTIME_DIR/org.keepassxc.KeePassXC.BrowserServer` symlink is no longer
  followed; the real socket under `app/org.keepassxc.KeePassXC/` is.
- `file:` on a FIFO no longer blocks past the context: the file is opened with
  `O_NONBLOCK` and refused as not a regular file.
- `file:` holding nothing (or only a line ending) is refused with `ErrEmpty`, like
  every other backend, instead of returning an empty secret: a truncated password
  file used to become an empty PIN. Behaviour change.

## v0.5.0

- `store` writes `secretservice:KEY=VALUE[,...]` (go-freedesktop/secretservice v0.4.0
  `SetByAttributes`): an existing match is refused unless `--replace`.

## v0.4.0

- `secretsource store REF [--group G] [--login L] [--replace] [--generate N]` and the library
  `Store(ctx, ref, secret, StoreOptions)` / `Generate(n)`: write a secret into `keychain:`,
  `secretservice:SERVICE/ACCOUNT` or `keepassxc:URL` (browser protocol `set-login`: the entry
  carries the URL by construction; duplicate check by URL; `--replace` updates that entry by its
  uuid; several matches are refused and named). `file:` and `secretservice:KEY=VALUE` are refused.
  The secret is typed twice with echo off or read from stdin, never from argv or the environment.
  `--title` is refused for KeePassXC: 2.8.0 ignores it.
- New errors `ErrExists`, `ErrNoStore`. The "no scheme" hint lists `keepassxc:`.
- `FuzzResolveGroup` (the get-database-groups tree) with committed corpus.

## v0.3.1

- Fix: `secretsource check` printed `secretsource: secretsource: bad reference ...`
  (the library's errors already carry the prefix).
- The container reproduction of the KeePassXC 2.8.0 crash is a Go program
  (`repro/keepassxc-2.8.0-crash/kpxrepro`, tested) instead of shell scripts; the
  upstream report notes that the crash is keepassxreboot/keepassxc#13865.

## v0.3.0

- **Fix: v0.2.0 could crash KeePassXC.** `keepassxc:by-uuid/...` and
  `keepassxc:by-path/...` asked KeePassXC for `keepassxc://by-uuid/...` and
  `keepassxc://by-path/...`; KeePassXC 2.8.0 dereferences the end of an empty
  list (`BrowserService::sortPriority`, BrowserService.cpp:1347, via
  `sortEntries`/`findEntries`/`handleGetLogins`) when that lookup finds an entry
  with no URL, and dies with SIGSEGV. Reproduced under gdb on a throwaway
  KeePassXC 2.8.0 built from the tag (`repro/keepassxc-2.8.0-crash/`). Those two
  forms are now refused (`ErrBadRef`) before any connection; use
  `keepassxc:URL` and give the entry that URL (an entry found by its URL has one).
  Breaking for the v0.2.0 forms (below v1: minor).
- A fake KeePassXC in the tests models the crash; the old request is replayed and
  shown to kill it, the new client never sends it.

## v0.2.0

- `keepassxc:by-uuid/HEX`, `keepassxc:by-path/Group/Title`, `keepassxc:URL`: read an entry
  through KeePassXC's browser-integration protocol (NaCl box over its Unix
  socket; macOS and Linux; Windows named pipe not yet). `Associate` and
  `secretsource keepassxc associate` register the machine once, approved in
  KeePassXC; the association is kept in the Keychain (macOS), the Secret Service
  (Linux) or a 0600 file. A lookup never creates trust (`ErrNotAssociated`).
- `secretservice:KEY=VALUE[,KEY=VALUE...]` reads an item by arbitrary attributes
  (go-freedesktop/secretservice v0.3.0 `GetByAttributes`); several matches are
  `ErrAmbiguous`. `secretservice:SERVICE/ACCOUNT` is unchanged, but a value
  containing `=` is now read as attributes.
- `cmd/secretsource`: `keepassxc associate`, `check REF` (never prints a secret).
- New errors `ErrAmbiguous`, `ErrNotAssociated`, `ErrDenied`; `Ref.Entry`, `Ref.Attrs`.
- Fuzz corpora committed for `FuzzParse` and `FuzzDecodeFrame`.

## v0.1.0

- `Open(ctx, ref)` and `Parse(ref)`; schemes `file:`, `keychain:`, `secretservice:`.
- No command scheme; no scheme guessing; 4096-byte bound; context/2-minute timeout.
