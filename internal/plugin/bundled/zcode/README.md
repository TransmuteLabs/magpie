# Built-in ZCode runtime

This directory contains Magpie's Core-owned `builtin:zcode` vendor runtime. It is loaded internally for the `zcode` provider; it is not an external, installable plugin. The protocol reference is ZCode 3.18.0 (`b95ebaff`). See `NOTICE`, `LICENSE.MIT`, `LICENSE.ZCode`, and `NOTICE.ZCode` for attribution and terms.

The runtime keeps the Z.ai and BigModel families and Personal, Team, Start, and legacy-key selections distinct. Business credentials, the ZCode session, and project access tokens have separate roles and lifetimes. A legacy key may support inference without establishing an account identity for financial operations.

Quota, reports, reset, billing, marketing, Off-Peak, Highspeed, and official MCP requests are scoped to the selected account. Magpie Core owns reset and claim durability, execution admission, ticket bindings, and any ordinary-route fallback decision; this transport does not independently schedule claims or spending. MCP destinations are checked before credentials are read. Proprietary captcha or request-signature mechanisms not present in the public source are not fabricated; operations requiring them must be completed in ZCode.

Local fixture tests exercise these boundaries without vendor traffic. They do not demonstrate live-account availability, entitlements, or production parity.
