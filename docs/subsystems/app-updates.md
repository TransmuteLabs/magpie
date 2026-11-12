# App updates and local macOS signing

## Responsibilities and sources of truth

[`internal/update/update.go`](../../internal/update/update.go) owns release
lookup, asset selection, download, macOS bundle staging and installation, and
bare-binary replacement. [`signed.go`](../../internal/update/signed.go) owns
release-manifest verification. The
[CLI](../../internal/cli/update_cli.go) and
[app updater](../../internal/gui/update.go) share those operations; local signing
belongs to the update package, so callers cannot skip it by choosing a different
update surface.

[`local_signing.go`](../../internal/update/local_signing.go) owns the optional
local signing policy, enrollment snapshot, staging receipt and installation
preflight. [`update_signing_cli.go`](../../internal/cli/update_signing_cli.go)
owns explicit selection of a Keychain identity. This feature signs a verified
staged macOS app before replacement. It does not sign the installed app in place,
export a private key, or change release publishing.

## Normal update path

Release lookup uses `MAGPIE_UPDATE_FEED` when set, otherwise the normal site
feed. Downloaded assets must match their SHA-256. `Release.verify` validates any
Ed25519 release-manifest signature and requires one for applicable versions on
the normal feed. A custom feed keeps the existing exception to the requirement
for a manifest signature; a signature it supplies must still verify. Local
code signing does not establish an archive's origin: selecting a custom feed is
an explicit trust decision by the user.

With local signing off, macOS staging keeps the existing `sameSigner` behavior:
verify the downloaded app's strict signature integrity, then compare its team
with the installed app's team. This includes the existing handling of ad-hoc
apps with empty team identifiers. Bare-binary updates retain their normal path.

## Fork release publishing

The [fork workflow](../../.github/workflows/fork-release.yml) publishes from
`devzeus` pushes. Its version gate selects a strict reachable upstream
`vX.Y.Z` tag, or validates an explicitly requested manual version. It reads
every page of the repository's published, non-prerelease releases and
compares the highest strict `f-vX.Y.Z` version before allowing publication.
An API failure stops the gate; drafts and prereleases do not suppress a
normal release. The app and feed use the same gate output.

Only the macOS app job is active. The parked Linux and Windows jobs are not
release dependencies. Local commits do not publish anything until pushed
or selected by an explicit workflow dispatch.

## Selecting a local identity

On macOS, Settings → Network shows **Local update signing** immediately after
**Update source**. Opening the menu lists usable local certificates; saving a
selection pins its full fingerprint. Nothing is chosen on a page read. Two
certificates with the same name show different fingerprint suffixes. The choice
is saved through `/api/update/signing`, outside the synchronized Settings.
Turning it off does not change the installed application's signature.

First install a version that supports these commands through the normal update
path. Run the command from the macOS app's `Contents/MacOS/magpie`, directly or
through a symlink to it. A standalone CLI copy cannot enroll a different app.
Set `MAGPIE_UPDATE_FEED` to the exact custom feed URL in the environment of both
the enrollment command and the app that will perform later updates.

```sh
magpie update signing
magpie update signing list
magpie update signing use <fullfingerprint>
magpie update signing off
```

The first command reports the saved policy. `list` prints usable Keychain
identities with their complete certificate fingerprints, names, teams and
expiration times. Choose one explicitly with its full 40-hex fingerprint, even
when two certificates have the same name or team. Listing never selects one.
`use` validates that identity against the app and saves a pin for future updates;
it does not replace or re-sign the installed app. The private key stays in
Keychain. Listing has a 45-second context and enrollment a 120-second context.
All four signing commands run before release lookup and make no update-feed
request.

The policy lives in `appdir.Config()/update-signing.json`, outside Settings and
the backup/sync collection. It contains public identifiers only: an exact leaf
certificate fingerprint, team, bundle ID, exact custom feed URL and a fresh
revision for each enrollment. An ad-hoc installed app also contributes its
bootstrap signature snapshot. The file is written atomically with mode `0600`.
A missing file means signing is off. A read or parse error means the policy is
unknown and stops the operation; it does not silently disable signing.

## Staging, bootstrap and installation

With a pin, the release's captured source feed, the current feed and the saved
feed must agree. The common preparation path downloads and checks the original
archive, extracts it into staging, and verifies the original app's signature
and supported layout before using the local key. Ad-hoc input is re-signed in
staging with the exact selected identity. Already certificate-signed input must
match the pin; a different signer is refused.

The supported profile is the current simple macOS app: the expected `magpie`
executable and bundle ID, no entitlements or hardened runtime, and no extra
helpers, frameworks, extensions or symlinked layout. Unexpected signing
metadata or layout is an error. The updater does not remove such metadata to
make an unsupported app fit the profile.

An enrolled ad-hoc app can make its first transition only while its installed
signature snapshot still matches the saved bootstrap snapshot. A successful
stage does not consume that permission. After transition, the installed app
must match the exact pinned leaf certificate, team and bundle ID. The next
payload must satisfy its stable designated requirement; a cdhash-bound
requirement is not accepted as a stable certificate identity.

After checking the signed result, staging writes a `0600` receipt beside the
payload, outside the bundle. It ties the policy revision/digest and feed to
snapshots of both the installed app and the prepared payload. Both `Install`
and `InstallAsAdmin` repeat the bounded preflight before changing bundles or
requesting elevation. They verify the receipt, current policy and feed,
strict signature integrity, exact identity, designated requirement and the
snapshots. Installation verifies only; it does not use the private key.
Enrollment, staging and installation share an interprocess lock and stop if it
cannot be acquired.

## Failure, recovery and compatibility

An unavailable, expired or refused key, a Keychain timeout, a changed feed,
a changed app or a failed signature check stops preparation. No other
certificate is selected automatically. These failures leave the installed app
in place. A stale or missing receipt under an active pin, or a receipt left
from a different or disabled policy, requires downloading the update again.
The app updater drops stale staging and offers a fresh download.

An ad-hoc installation can enroll a different intended identity before its
first transition by running `list` and `use` again, then downloading a fresh
update. Once the installed app has a certificate signature, enrollment must
match that certificate; automatic rotation to another certificate is not
supported. Re-enrolling the same certificate also requires a fresh download.
To disable the feature, run `off`; this removes the local policy and leaves
the installed app's signature as it is. Normal signature checks still apply,
so an update from a different team may be refused after a locally signed
installation. A malformed policy is reported as an error; explicit `off` is
the way to disable it. Linux and Windows do not provide local macOS signing
and keep their normal update behavior.

The first transition can require consent for macOS permissions again. TCC and
Full Disk Access continuity have not been verified; a stable signing identity
alone is not evidence that those permissions survive an update.

## Verification

Run checks in the repository's sandbox HOME with Go caches pinned as described
in the [code standards](../code-standards.md). Relevant package checks are
`go test -tags nogui -timeout 45m ./internal/update ./internal/cli ./internal/backup`,
followed by the standard formatting, build, vet and full-suite checks. CLI
signing dispatch tests replace all signing operations with mocks and do not use
the live Keychain, installed app or update feed. Update-package checks cover
policy/receipt failures and the shared staging and installation paths.

A real certificate-signing check uses temporary bundles and an explicitly
chosen identity, separately from the normal suite. A real app replacement,
restart or permission check requires its own agreed run; unit tests do not
establish TCC or Full Disk Access continuity.
