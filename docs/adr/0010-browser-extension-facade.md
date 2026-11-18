# Browser SDK facade over the existing page bridge

Date: 2026-11-13
Status: Implemented and verified as an isolated addition to SDK 1.3.

## Decision

Expose the existing page bridge through the browser-only
`@magpie/extension-sdk/ui` entry. Besides readiness, service, named backend
calls and config CAS, authors get typed theme/context/visibility state,
events, row actions, permission-checked reads and PNG copy/save.
Configuration values remain JSON objects, matching the root config owner.

The facade delegates identity, permissions and lifetime to `window.magpie`.
It adds no transport, URL selection, account selector or browser authority.
Frame disposal rejects pending calls through the existing bridge. Legacy
provider options remain outside the SDK entry.

## Verification

The isolated source is based on the
[SDK 1.3 acceptance source](0009-sdk13-validation.md) plus only the facade
and its fixtures. `result.json` records SHA256 for all seven relevant files.

- Node and Bun: 110 tests each, exit 0, no failures or skips.
- Four strict TypeScript fixtures: exit 0. Optional PNG filename typing was
  checked again after that declaration was relaxed to match the bridge.
- The real `internal/gui/pagebridge.js` runs in a VM with a synthetic parent
  and DOM. The fixture checks foreign-source refusal, queued actions,
  state/events, permission errors, PNG forwarding and disposal.
- Substituting the old `ui.js` while retaining the new fixture produces a
  behavioral assertion failure rather than a compilation or loader error.

This does not exercise a native clipboard, file-save dialog, browser engine
matrix or installed GUI. Later SDK changes need their own combined gates.
