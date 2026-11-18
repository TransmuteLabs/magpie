# App updates

## Responsibilities and sources of truth

[`internal/update/update.go`](../../internal/update/update.go) owns release
lookup, asset selection, download, macOS bundle staging and installation,
and bare-binary replacement. [`signed.go`](../../internal/update/signed.go)
owns release-manifest verification. The [CLI](../../internal/cli/update_cli.go)
and [app updater](../../internal/gui/update.go) share those operations.

## Normal update path

Release lookup uses `MAGPIE_UPDATE_FEED` when set, otherwise the normal site
feed. Downloaded assets must match their SHA-256. `Release.verify` validates any
Ed25519 release-manifest signature and requires one for applicable versions on
the normal feed. A custom feed keeps the existing exception to the requirement
for a manifest signature; a signature it supplies must still verify. Local
code signing does not establish an archive's origin: selecting a custom feed is
an explicit trust decision by the user.

macOS staging keeps the existing `sameSigner` behavior:
verify the downloaded app's strict signature integrity, then compare its team
with the installed app's team. This includes the existing handling of ad-hoc
apps with empty team identifiers. Bare-binary updates retain their normal path.

With automatic downloads enabled, the first restart checks the selected feed
and channel for the newest version and can replace an older staged download.
The restart following that check's download does not check again. With automatic
downloads disabled, restart retains the payload explicitly downloaded. A
metadata-only dialog or notes-language refresh also preserves its staged payload.
Replacement retains ordinary archive verification and platform-specific
installation checks.

A notes-language lookup that finds a newer release with no staged payload can
start downloading it. The resulting check and download retain the lookup owner's
cancellation. A check queued during another download also belongs to that owner's
joined background work; stopping the owner cancels and joins those follow-ups
before its state or test resources are released. A metadata-only refresh preserves
its staged payload across a concurrent channel-setting change; an ordinary
explicit channel check still replaces it with the selected channel's release.

Every version, Daily and Stable retain upstream channel selection. The updater
captures the installed version when its owner starts; comparisons, urgent
lookups and background work use that captured version. A later change to the
package-global version does not rebase an already running lookup.

## Historical scope

Dated ADRs may describe additional fork delivery and local macOS signing.
Those qualifications remain historical evidence; the features and their
deployment/signing claims are not part of this SDK-only selection.

## Verification

Run the retained updater and GUI checks in an isolated HOME with pinned caches
as required by the [code standards](../code-standards.md), followed by the
projected source's ordinary static, build and full-suite gates. Keep the
GUI updater's fresh/owned-refresh/staging tests with their corresponding
changes, and `download_test.go` with the common update request path.
The SDK shell-function planner and `shellfn_plan_test.go` remain an
independent required agent-binding dependency inside the update package.

The retained update-source and preference-publication browser fixtures
keep both engines, authored languages/widths, exact setting writes and
page-lifetime assertions. Removed signing tests supply no acceptance for
this projection. These fixture checks do not install or restart an app,
access the live Keychain or establish live update-feed acceptance.
