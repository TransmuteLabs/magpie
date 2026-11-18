# Extension SDK acceptance

The verification covers the SDK and its host integrations on upstream
`889991b15f9ed3af3ed4e7e783dc832ef9c3e929`. The tested source is Git tree
`47de090e5ecd937615d145b3a5dc7d7317be03c0` with 11,318 files. The public
SDK distribution follows the application version. Wire API `1.12`,
native ABI `1` and native account codec `0.5` have independent
compatibility rules.

## Host and SDK checks

| Check | Result |
| --- | --- |
| Broad Go suite | Go 1.26.8 on macOS arm64, `-tags nogui -p=2 -count=1 -timeout=60m`, exit 0 with 112 passing packages, 33,795 pass events and 219 skip events. The three exclusions below were checked separately. |
| Platform compatibility | Vet and build passed on macOS arm64 and for the Linux and Windows amd64 cross targets. Cross builds used CGO disabled. |
| JavaScript contract | 347 Node passes, 282 Bun passes, conformance in both runtimes and strict TypeScript checking. |
| Targeted Go races | 21 pass events for changes to provider and Settings paths. |
| Rendered interface | 96 focused browser tests passed across Chromium and WebKit. Full browser acceptance covers 1,603 test files and is pending. |

The broad suite excluded `TestTreeRSS`,
`TestCombinedOutputOwnAnswersForItself` and
`TestScriptFindsClaudeMoved`. All three passed 20 repetitions in guarded
fixtures. The Desktop discovery fixture isolates installed applications.

Two post-freeze changes repaired validation links and the OMP target-path
assertion. The test compares canonical destination paths on macOS. The
ordinary native OMP cases passed three repetitions with the corrected test.

The full browser attempt found four Settings About timeouts. A focused
tab lost keyboard focus when sidebar icons loaded and the list redrew.
The regression failed on the prior application in Chromium and WebKit
and passed in both engines after the focus fix. The nearby unroll test
holds the animation until the installed-plugin count is drawn. It
passed in both engines, and Chromium reproduced the selection offset
when the row-size rule was removed. Full browser acceptance applies to
the successor tree with these fixes.

Representative regression controls exercise selected-account callback
admission, final refusal and retry budgets, credential redaction, Settings
interactions and native session export. Retained verification receipts
identify the source, commands and isolation for each check.

## Verification boundary

These checks use local fixtures. Native Linux and Windows execution, live
vendor sign-ins and real-client interactions require platform and module
acceptance of their own.

- [Subsystem references](../subsystems/README.md)
- [Extension contracts and boundaries](../subsystems/extension-surface-coverage.md)
