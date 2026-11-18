# Go executable example

This plugin provides an echo model, a native request decoder at `/sdk-echo/rpc`
and a tool. It uses no vendor, external dependency or credential.

From the Magpie repository:

```sh
mkdir -p /tmp/sdk-echo
go build -o /tmp/sdk-echo/plugin ./examples/extensions/echo-go
cp examples/extensions/echo-go/magpie-plugin.json /tmp/sdk-echo/
magpie plugin sdk add /tmp/sdk-echo
magpie plugin sdk list --json
magpie models
```

On Windows build `plugin.exe` and set the distribution manifest's `entry` to
`plugin.exe`. The compiled manifest's packaging fields do not participate in
the public capability handshake.

Pick the model reference shown by `magpie models`. A native request has this
shape:

```json
{"model":"<the displayed provider>/echo","prompt":"hello","stream":false}
```

POST it to `/sdk-echo/rpc` on the normal gateway, using the same gateway key as
for Chat Completions. The response is a canonical Chat response in this simple
example. `{"models":true}` projects the caller's authorized model catalog.

For a plugin in another Go module, import
`github.com/yetone/magpie/pkg/extension` and use a development `replace` to this
checkout until its SDK commit is available remotely. No Magpie private package
is needed. Keep stdout exclusively for the SDK; put diagnostics on stderr.
