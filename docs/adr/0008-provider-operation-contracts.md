# Declare provider operations separately from conversational protocols

Date: 2026-11-13
Status: Accepted; execution evidence is recorded separately.

The initial SDK inferred execution from conversational URL suffixes.
Embeddings, reranking and image/audio/video operations were rejected before
the author's handler. Catalogs also lost their kinds and modalities.

SDK API 1.3 providers declare a finite `operations` list separately from
`protocols`: embeddings, rerank, image generation/edits, audio
transcription/translation/speech, and video create/get/content/delete/remix.
They use the OpenAI-compatible `chat` wire family and explicit
`ExecuteRequest.operation`. Existing conversation calls retain an empty
operation. Anthropic token counting and Gemini generation/streaming/counting
retain their existing native paths.

The adapter matches exact method/path combinations, preserving raw bodies,
content type, query, decoded and escaped paths under existing byte limits.
Both the actual leased host descriptor and the author SDK check operations
before dispatch. Older API versions cannot declare them. Omitting the list
retains existing conversational scope.

Models independently describe kind, modalities, maximum/default context,
release date, temperature, tiers, subscription rates and reasoning defaults.
Unknown modalities and explicit empty lists remain distinct through public
manifest round trips. Core user overrides remain authoritative. Catalog
metadata alone grants no operation permission.

Ordinary media selection consumes these facts and declared operations.
A video continuation carries a stable module/provider/account mark:
switching accounts cannot transfer a video; credential rotation within the
original account preserves the mark. Ordinary caller guards still apply.

Retrieval, images and video creation share an extension attempt across
fallback. Accepted, uncertain or completed execution cannot automatically
start on another provider/account. Explicit not-started refusals retain
normal fallback. The existing durable journal owns replay policy.

This registry does not add public routes the gateway has not registered,
nor implement vendor media. Dedicated decisions, provider options, check-in,
background keepalive and inbound native session/assignment operations retain
separate contracts.

Rejected: Arbitrary URLs | no bounded or generation-specific admission.
Rejected: One conversation protocol per endpoint | wire family and operation
describe different facts.
Rejected: Modalities as permission | catalog metadata is not transport support.
Rejected: Unjournaled media retries | accepted paid operations could run twice.

Required verification covers catalog projection, binary preservation, exact
endpoint admission, actual leased-generation checks before journaling,
Go/JS parity, ordinary routes, video account affinity and accepted-operation
fallback suppression. Real-vendor media and Windows native execution require
separate evidence.
