# gorpc (experimental)

Built with Claude Code + GPT-5.

Gob-framed transport for Go's net/rpc with unary and bidirectional streaming. It reuses net/rpc concurrency/lifecycle and only replaces the wire format and per-call stream semantics. Each frame is a standalone gob value (no length prefix).
> Experimental project: an attempt to layer bidirectional streaming RPC over the standard net/rpc package using custom ClientCodec/ServerCodec and a simple gob-framed protocol. Not production-ready. APIs and the wire format may change without notice.

Status: Experimental

## Why this exists
- Add streaming (client→server, server→client, bidi) to net/rpc without changing the registration/call model
- Reuse net/rpc concurrency/lifecycle; try a minimal, self‑delimiting gob‑framed transport

## Non-goals
- Not a drop‑in replacement for gRPC/HTTP/2 stacks
- No built-in security, service discovery, or load balancing

## Features
- Simple wire protocol: a single `frame` type, gob-encoded with self-delimiting boundaries
- Unary + streaming: client→server, server→client, bidirectional
- Half-close and reset: `EndOfStream` and `STREAM_RESET`
- Heartbeats: `PING`/`PONG`
- Optional client reconnection with exponential backoff; connection generations prevent split writes across reconnects
- Lifecycle callbacks: optional cancel functions for context management and cleanup on connection close

## Limitations & caveats
- Gob-encodable exported types only; use `gob.Register` for concrete types
- No enforced stream flow control; rely on channel buffering and application logic
- No context propagation or per-RPC deadlines (only simple timeouts in the codec)
- Behavior/APIs may change during iteration

## Risks (experimental)
- API/wire format churn during experimentation
- Potential memory growth without flow control (mis-sized buffers or slow consumers)
- Goroutine leaks if channels are not drained (note: codec Close() has timeout protection to prevent indefinite hangs)
- Disconnects may surface as TIMEOUT/connection-lost; prefer idempotent operations
- Head-of-line blocking for very large frames due to a single decoder goroutine

## Install
```bash
# Go modules
go get github.com/x5iu/gorpc@latest
```

## Quick start

Server
```go
package main

import (
	"log"
	"net"
	"net/rpc"

	"github.com/x5iu/gorpc"
)

type Args struct{ A, B int }



type Filter struct{ N int }

type Event struct{ X int }

type In struct{ V int }

type Out struct{ V int }

type Svc struct{}

// Add adds two integers.
func (s *Svc) Add(args *Args, reply *int) error {
	*reply = args.A + args.B
	return nil
}

// Watch streams events to the client (reply *chan).
func (s *Svc) Watch(args *Filter, reply *chan Event) error {
	ch := make(chan Event, 16)
	*reply = ch
	go func() {
		defer close(ch)
		for i := 0; i < args.N; i++ {
			ch <- Event{X: i}
		}
	}()
	return nil
}

// Pipe performs bidirectional streaming (args chan, reply *chan).
func (s *Svc) Pipe(args chan In, reply *chan Out) error {
	out := make(chan Out, 16)
	*reply = out
	go func() {
		defer close(out)
		for v := range args { out <- Out{V: v.V * 2} }
	}()
	return nil
}

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil { log.Fatal(err) }
	s := rpc.NewServer()
	if err := s.RegisterName("Svc", &Svc{}); err != nil { log.Fatal(err) }
	for {
		conn, err := ln.Accept()
		if err != nil { log.Println("accept:", err); continue }
		go s.ServeCodec(gorpc.NewServerCodec(conn))
	}
}
```

Client
```go
package main

import (
	"fmt"
	"log"

	"github.com/x5iu/gorpc"
)

type Args struct{ A, B int }

type Filter struct{ N int }

type Event struct{ X int }

type In struct{ V int }

type Out struct{ V int }

func main() {
	// Dial with reconnection + timeout support
	cli, err := gorpc.NewClient("go://127.0.0.1:8080?timeout=1s")
	if err != nil { log.Fatal(err) }
	defer func() { _ = cli.Close() }()

	// Unary
	var sum int
	if err := cli.Call("Svc.Add", &Args{A: 7, B: 8}, &sum); err != nil { log.Fatal(err) }
	fmt.Println("sum=", sum)

	// server→client streaming
	var events chan Event
	if err := cli.Call("Svc.Watch", &Filter{N: 3}, &events); err != nil { log.Fatal(err) }
	for ev := range events { fmt.Println("event:", ev.X) }

	// Bidirectional streaming
	in := make(chan In, 16)
	var out chan Out
	go func() { in <- In{V: 5}; in <- In{V: 7}; close(in) }()
	if err := cli.Call("Svc.Pipe", in, &out); err != nil { log.Fatal(err) }
	for v := range out { fmt.Println("out:", v.V) }
}
```

## API surface
- `NewServerCodec(rwc io.ReadWriteCloser, opts ...ServerOption) rpc.ServerCodec`
- `NewClientCodec(rwc io.ReadWriteCloser, opts ...ClientOption) rpc.ClientCodec`
- `NewClient(rawURL string) (*rpc.Client, error)`
- Client options: `WithTimeout(d)`, `WithDialer(dial)`, `WithReconnectBackoff(factory)`, `WithClientCancelFunc(cancel)`
- Server options: `WithServerTimeout(d)`, `WithCancelFunc(cancel)`

Timeout query parameter accepts Go durations like `500ms`, `2s`, or integer seconds like `1`.

### Lifecycle callbacks
Both client and server codecs support cancel function callbacks that are invoked when the codec is closed:
- **Client**: `WithClientCancelFunc(cancel func())` - called when client codec closes (explicitly via Close() or due to connection loss)
- **Server**: `WithCancelFunc(cancel func())` - called when server codec closes (explicitly via Close() or due to connection loss)

Common use cases:
- Cancel context to stop related goroutines
- Clean up resources associated with the connection
- Remove connection from a connection pool
- Trigger reconnection logic or circuit breakers

Example:
```go
ctx, cancel := context.WithCancel(context.Background())
client, err := gorpc.NewClient(
    "go://localhost:8080",
    gorpc.WithClientCancelFunc(cancel), // auto-cancel context on close
)
if err != nil { log.Fatal(err) }
defer client.Close()

// Use ctx in goroutines that should stop when connection closes
go func() {
    <-ctx.Done()
    log.Println("Connection closed, cleaning up...")
}()
```

## Protocol overview (gob frames)
Each frame is a gob-encoded `frame` value:
- Request/response: `REQUEST_HEADER` → `REQUEST_BODY`; `RESPONSE_HEADER` → `RESPONSE_BODY`
- Stream data: `STREAM_DATA{direction, payload, end_of_stream}`
- Control: `STREAM_WINDOW_UPDATE` (reserved), `STREAM_RESET{reset_code, error_message}`, `PING/PONG{heartbeat_token}`
- If `StreamID` is zero it defaults to `Sequence`

When args or reply are channels:
- `args chan T` enables client→server streaming; `reply *chan T` enables server→client streaming; both together enable bidi
- Closing the originating channel sends `end_of_stream=true`; the peer closes that direction

## Reconnect & robustness
- The client read loop owns dialing; writers validate a connection generation number to avoid splitting one logical request across reconnections
- Optional exponential backoff; initial connect retries automatically

## Comparison with gRPC
- Transport & encoding: gRPC uses HTTP/2 + Protobuf by default; this project uses raw TCP + gob (Go-only, reflection-based)
- Streaming & flow control: gRPC has full-duplex streaming with HTTP/2 flow control; here streaming works via channels but flow control is not enforced
- APIs & ecosystem: gRPC offers IDL, codegen, interceptors, observability and wide polyglot support; this is Go-only with minimal surface
- Security & ops: gRPC has first-class TLS/mTLS, load balancing, health checks; this project leaves these concerns to the embedding application
- Performance: gRPC is generally faster and more efficient on the wire; gob can be convenient for Go types but may be slower and less compact
- Interop: gRPC is cross-language; this project targets Go applications only

## Testing
```bash
go vet ./...
go test -race ./...
```

## Design overview

- Transport: gob-framed frames; each frame is a standalone gob value with self-delimiting boundaries (no length prefix)
- Codecs: implements net/rpc ClientCodec and ServerCodec; single reader goroutine per side; writers are serialized to preserve ordering
- Streams via channels: args chan T enables client→server; reply *chan T enables server→client; both together enable bidirectional; closing the origin channel half-closes that direction; errors use STREAM_RESET
- Timeouts & liveness: optional stream timeout for handshake and idle; TIMEOUT resets clean up resources; heartbeats via PING/PONG
- Errors & resets: uses STREAM_RESET with ResetCode values such as PROTOCOL_ERROR, TIMEOUT, ENCODE_ERROR, DECODE_ERROR; half-close via EndOfStream; receivers close and clean up
- Reconnection: client read loop owns dialing; connection generation prevents splitting a single logical request across reconnects
- Robustness improvements:
  - Connection tracking: readLoop tracks the connection it's currently reading from to ensure Close() can interrupt blocking operations even during reconnection
  - Close() timeout protection: waits up to 2 seconds for readLoop to exit; if timeout occurs, Close() returns to prevent indefinite hangs
  - Lifecycle callbacks: optional cancel functions invoked on codec close for context cancellation and resource cleanup

## Roadmap
### Milestones
  1) gob frames and single reader goroutine
  2) ClientCodec/ServerCodec: channel handling and `STREAM_*` forwarding
  3) Flow control/heartbeats/errors (current: heartbeats and basic errors; flow control reserved)
  4) Tests/benchmarks

---
For runnable examples and edge cases, see `codec_test.go`.

