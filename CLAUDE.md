# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Repository: github.com/x5iu/gorpc

Overview
- Experimental project layering bidirectional streaming RPC over Go's net/rpc using custom ClientCodec/ServerCodec and a simple gob-framed wire format. See README.md for user-facing docs and protocol details.
- Key goals: retain net/rpc's concurrency and lifecycle while adding channel-driven streaming (client→server, server→client, bidi) with minimal framing.
- Built with Claude Code + GPT-5 as an experimental exploration of streaming over net/rpc.

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
- Wire unit: frame struct gob-encoded per message; self-delimiting without length prefix (codec.go:~26-38). Types include REQUEST_HEADER/BODY, RESPONSE_HEADER/BODY, STREAM_DATA, STREAM_RESET, STREAM_WINDOW_UPDATE (reserved), PING/PONG. StreamID defaults to Sequence when zero.
- Codecs:
  - ClientCodec (codec.go:~67-100) and ServerCodec (codec.go:~102-126) implement net/rpc.{ClientCodec,ServerCodec}. Constructors: NewClientCodec, NewServerCodec.
  - Client also provides NewClient(rawURL) for tcp dialing with optional timeout/backoff and reconnection helpers (connect/backoff logic).
- Concurrency model:
  - Single reader goroutine per side (readLoop) dispatches decoded frames by type/sequence; application-facing Read*/Write* never read the socket directly.
  - Writers never dial; all writes share a mutex to preserve ordering (writeFrame, writeFrameWithGen).
  - Connection tracking: readLoop saves the connection it's currently reading from (readingRwc) to ensure Close() can interrupt blocking Decode operations even during reconnection.
- Streaming semantics (channel-triggered):
  - If args is chan T, client initiates c→s stream; if reply is *chan T, server initiates s→c stream; both together enable bidi (see tests).
  - Handshake via *_BODY immediately after *_HEADER; for channel cases *_BODY carries no payload and spawns pumps.
  - Pumps: send side reads from user chan and emits STREAM_DATA; close sends EndOfStream (pumpSendChan). Recv pumps decode to user chan and close on EOS/RESET/timeouts (pumpRecvToChan).
- Ordering and validation:
  - Header required before body; violations trigger STREAM_RESET.
  - Direction checks for STREAM_*; mismatches reset.
- Timeouts and robustness:
  - Header watchdog to avoid indefinite waits when connection is lost between write and header.
  - Optional streamTimeout governs handshake wait and stream idle; leads to TIMEOUT resets and resource cleanup.
  - Client reconnection with exponential backoff; connection generation prevents split request frames across reconnects.
  - Close() with timeout protection: waits up to 2 seconds for readLoop to exit; closes both main connection and readLoop's active connection to interrupt blocking Decode.
- Lifecycle callbacks:
  - Both client and server support cancel function callbacks (WithClientCancelFunc/WithCancelFunc) that are invoked when the codec is closed (explicitly or due to connection loss).
  - Useful for context cancellation, resource cleanup, and connection pool management.
- Encoding helpers:
  - encodeGob/decodeGob use small buffer/reader pools. Only exported fields/types are encoded.

Tests as executable spec
- codec_test.go exercises: unary success/error, stream directions and half-close, protocol violations producing RESET, decode/encode error paths, timeout behavior, default StreamID, duplicate header protection, reconnection path, cancel function callbacks, goroutine leak detection.
- Key test patterns:
  - TestClientNoGoroutineLeak: verifies no goroutine leaks after Close() in reconnection scenarios
  - TestClientWithCancelFunc_* and TestServerWithCancelFunc_*: verify lifecycle callbacks work for both explicit close and connection loss
  - TestClientReconnectsAfterConnectionLoss: tests client reconnection behavior (note: this test was historically flaky but has been fixed with connection tracking improvements)
- Use targeted runs when iterating (e.g., go test -run '^TestBidiStream$' -v).

Protocol references
- README.md contains the protocol overview, API surface, feature list, limitations, and comparison with gRPC.

Conventions for this repo
- Code comments in English.
- Prefer go vet over go build for quick validation.
- Keep the README's "experimental" nature accurate; avoid expanding scope (security, full flow control) inside this module unless explicitly planned.

Extension points (where to look/change)
- Flow control: STREAM_WINDOW_UPDATE frames are parsed but ignored; delivery sites (deliverStream in readLoop) are where enforcement could be added.
- Heartbeats: PING/PONG handling in read loops if you need custom liveness.
- Dialing/security: NewClient uses a dialer hook (WithDialer option); wrap it for TLS or custom transports without changing codecs.
- Reconnect policy: Backoff interface and exponentialBackoff impl can be swapped via WithReconnectBackoff.
- Lifecycle hooks: WithClientCancelFunc and WithCancelFunc provide callbacks for connection close events (useful for context cancellation, cleanup, etc).

API surface (Client)
- NewClient(rawURL string) (*rpc.Client, error) - Creates client with reconnection support; URL format: "go://host:port?timeout=<duration>"
- NewClientCodec(rwc io.ReadWriteCloser, opts ...ClientOption) rpc.ClientCodec
- Client options:
  - WithTimeout(d time.Duration) - Sets stream timeout for handshake and idle detection
  - WithDialer(dial func() (io.ReadWriteCloser, error)) - Custom dialer for reconnection
  - WithReconnectBackoff(factory func() Backoff) - Custom backoff strategy
  - WithClientCancelFunc(cancel func()) - Callback invoked on codec close (explicit or connection loss)

API surface (Server)
- NewServerCodec(rwc io.ReadWriteCloser, opts ...ServerOption) rpc.ServerCodec
- Server options:
  - WithServerTimeout(d time.Duration) - Sets stream timeout
  - WithCancelFunc(cancel func()) - Callback invoked on codec close (explicit or connection loss)

Important implementation details
- Connection tracking fix: clientCodec tracks both the main connection (rwc) and the connection currently being read by readLoop (readingRwc). This prevents Close() from hanging when reconnection happens between acquiring the decoder and blocking on Decode().
- Close() timeout protection: waits up to 2 seconds for readLoop to exit; if timeout occurs, Close() returns anyway to prevent indefinite hangs.
- Cancel callbacks run in separate goroutines to avoid blocking Close() or readLoop.
- sync.Once ensures cancel callbacks run exactly once even if Close() is called multiple times.

Anti-drift tip (finding refs when lines shift)
- Use ripgrep to locate definitions quickly when line numbers change:
  - rg -n "type clientCodec|type serverCodec|func NewClient|func NewClientCodec|func NewServerCodec|writeFrameWithGen|readLoop|deliverStream|pumpSendChan|pumpRecvToChan|func With" codec.go
  - rg -n "TestClientReconnects|TestClientNoGoroutineLeak|TestClientWithCancelFunc|TestServerWithCancelFunc" codec_test.go
