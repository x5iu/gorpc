# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Repository: github.com/x5iu/gorpc

Overview
- Experimental project layering bidirectional streaming RPC over Go's net/rpc using custom ClientCodec/ServerCodec and a simple gob-framed wire format. See README.md and DESIGN.md for user-facing docs and protocol details.
- Key goals: retain net/rpc’s concurrency and lifecycle while adding channel-driven streaming (client→server, server→client, bidi) with minimal framing.

Commands (use these during development)
- Lint/static analysis (preferred):
  - go vet ./...
- Run all tests with race detector (no cache):
  - go test -race -count=1 ./...
- Run a single test by name (regex, no cache):
  - go test -v -count=1 -run '^TestUnary$' ./...
- List tests:
  - go test -list '.*' ./...
- Optional coverage:
  - go test -race -count=1 -cover ./...
Note: This repo is a library module (no main). Prefer go vet to validate rather than go build.

High-level architecture
- Wire unit: frame struct gob-encoded per message; self-delimiting without length prefix (codec.go:26-38). Types include REQUEST_HEADER/BODY, RESPONSE_HEADER/BODY, STREAM_DATA, STREAM_RESET, STREAM_WINDOW_UPDATE (reserved), PING/PONG. StreamID defaults to Sequence when zero.
- Codecs:
  - ClientCodec (codec.go:67-98) and ServerCodec (codec.go:100-122) implement net/rpc.{ClientCodec,ServerCodec}. Constructors: NewClientCodec (codec.go:188-219), NewServerCodec (codec.go:221-241).
  - Client also provides NewClient(rawURL) for tcp dialing with optional timeout/backoff (codec.go:430-464, 466-475) and reconnection helpers (connect/backoff: codec.go:250-296, 298-309).
- Concurrency model:
  - Single reader goroutine per side dispatches decoded frames by type/sequence; application-facing Read*/Write* never read the socket directly (client readLoop codec.go:777-848; server readLoop codec.go:850-894).
  - Writers never dial; all writes share a mutex to preserve ordering (client writeFrame codec.go:716-739 and writeFrameWithGen codec.go:747-775; server writeFrame codec.go:741-744).
- Streaming semantics (channel-triggered):
  - If args is chan T, client initiates c→s stream; if reply is *chan T, server initiates s→c stream; both together enable bidi (see DESIGN.md §6 and tests).
  - Handshake via *_BODY immediately after *_HEADER; for channel cases *_BODY carries no payload and spawns pumps.
  - Pumps: send side reads from user chan and emits STREAM_DATA; close sends EndOfStream (client/server pumpSendChan codec.go:1188-1221, 1224-1257). Recv pumps decode to user chan and close on EOS/RESET/timeouts (codec.go:1260-1340, 1342-1422).
- Ordering and validation:
  - Header required before body; violations trigger STREAM_RESET (client codec.go:815-841; server codec.go:862-887).
  - Direction checks for STREAM_*; mismatches reset (client codec.go:833-841; server codec.go:880-887).
- Timeouts and robustness:
  - Header watchdog to avoid indefinite waits when connection is lost between write and header (codec.go:329-355).
  - Optional streamTimeout governs handshake wait and stream idle; leads to TIMEOUT resets and resource cleanup (client handshake timeout codec.go:1106-1132; server handshake timeout codec.go:1159-1185; client stream idle codec.go:1271-1290; server stream idle codec.go:1353-1372).
  - Client reconnection with exponential backoff; connection generation prevents split request frames across reconnects (codec.go:250-296, 298-309, 747-775).
- Encoding helpers:
  - encodeGob/decodeGob use small buffer/reader pools (codec.go:1478-1501). Only exported fields/types are encoded (see DESIGN.md §10).

Tests as executable spec
- codec_test.go exercises: unary success/error, stream directions and half-close, protocol violations producing RESET, decode/encode error paths, timeout behavior, default StreamID, duplicate header protection, reconnection path. Use targeted runs when iterating (e.g., go test -run '^TestBidiStream$' -v).

Protocol references
- DESIGN.md is authoritative for frame fields and sequencing (see §3.1 types, §6 half-close, §7 concurrency). README.md summarizes features, risks, and gRPC comparison.

Conventions for this repo
- Code comments in English.
- Prefer go vet over go build for quick validation.
- Keep the README’s "experimental" nature accurate; avoid expanding scope (security, full flow control) inside this module unless explicitly planned in DESIGN.md.

Extension points (where to look/change)
- Flow control: STREAM_WINDOW_UPDATE frames are parsed but ignored; delivery sites (client/server deliverStream codec.go:955-1002, 1004-1051) are where enforcement could be added.
- Heartbeats: PING/PONG handling in read loops (client codec.go:842-844; server codec.go:889-891) if you need custom liveness.
- Dialing/security: NewClient uses a dialer hook (codec.go:128-137, 430-464); wrap it for TLS or custom transports without changing codecs.
- Reconnect policy: Backoff interface and exponentialBackoff impl (codec.go:148-151, 153-176) can be swapped via WithReconnectBackoff.

Anti-drift tip (finding refs when lines shift)
- Use ripgrep to locate definitions quickly when line numbers change:
  - rg -n "type clientCodec|type serverCodec|func NewClient|func NewClientCodec|func NewServerCodec|writeFrameWithGen|readLoop|deliverStream|pumpSendChan|pumpRecvToChan" gorpc/codec.go
