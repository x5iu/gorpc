# gorpc (experimental)

Built with Claude Code + GPT-5.

Gob-framed transport for Go's net/rpc with unary and bidirectional streaming. It reuses net/rpc concurrency/lifecycle and only replaces the wire format and per-call stream semantics. Each frame is a standalone gob value (no length prefix). See DESIGN.md for details.

> Experimental project: an attempt to layer bidirectional streaming RPC over the standard net/rpc package using custom ClientCodec/ServerCodec and a simple gob-framed protocol. Not production-ready. APIs and the wire format may change without notice.

Status: Draft · Version: v0.9-gob

## Why this exists
- Explore whether net/rpc can support streaming semantics (client→server, server→client, bidi) without changing its registration/call model
- Reuse net/rpc’s proven concurrency, request lifecycle and multiplexing, while experimenting with a minimal, self-delimiting wire format (gob frames)

## Non-goals
- Not a drop-in replacement for gRPC or HTTP/2-based systems
- No built-in security (TLS/mTLS/auth), service discovery or load balancing
- No production-grade flow control yet (WINDOW_UPDATE is reserved but disabled)
- No backward-compatibility guarantee for the wire format during experimentation

## Features
- Unary plus three streaming modes: client→server, server→client, bidirectional
- Simple wire protocol: a single `frame` type, gob-encoded with self-delimiting boundaries
- Half-close and reset: `EndOfStream` and `STREAM_RESET`
- Heartbeats: `PING`/`PONG`
- Optional client reconnection with exponential backoff; connection generations prevent split writes across reconnects

## Limitations & caveats
- Only gob-encodable exported types/fields are supported; consider `gob.Register` for concrete types
- Stream flow control is not enforced; back pressure relies on channel buffering and application logic
- No context propagation or per-RPC deadlines beyond simple timeouts inside the codec
- Protocol, APIs and behavior may change as the design evolves

## Risks (experimental)
- API and wire format churn: breaking changes can happen without deprecation during the experiment
- Backpressure and memory growth: lack of enforced flow control means mis-sized buffers or slow consumers can grow memory
- Blocking user code: if application code stops draining channels, goroutines can leak and streams can deadlock
- Reconnection semantics: in-flight requests during disconnects may surface as TIMEOUT/connection-lost; design for idempotency
- Security: no TLS/mTLS/auth; do not send sensitive data over untrusted links
- Performance trade-offs: gob reflection overhead; a single decoder goroutine per connection can create head-of-line effects for very large frames
- Operability: no built-in observability/interceptors/metrics; limited ecosystem around net/rpc compared to modern stacks

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

type Item struct{ X int }

type Ack struct{ OK bool }

type Filter struct{ N int }

type Event struct{ X int }

type In struct{ V int }

type Out struct{ V int }

type Svc struct{}

// Unary example
func (s *Svc) Add(args *Args, reply *int) error {
	*reply = args.A + args.B
	return nil
}

// Server->Client streaming (reply *chan)
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

// Bidirectional streaming (args chan, reply *chan)
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
	defer cli.Close()

	// Unary
	var sum int
	if err := cli.Call("Svc.Add", &Args{A: 7, B: 8}, &sum); err != nil { log.Fatal(err) }
	fmt.Println("sum=", sum)

	// Server->Client streaming
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
- Client options: `WithTimeout(d)`, `WithDialer(dial)`, `WithReconnectBackoff(factory)`
- Server options: `WithServerTimeout(d)`

Timeout query parameter accepts Go durations like `500ms`, `2s`, or integer seconds like `1`.

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

## Design & roadmap
- Design: see [DESIGN.md](./DESIGN.md)
- Milestones:
  1) gob frames and single reader goroutine
  2) ClientCodec/ServerCodec: channel handling and `STREAM_*` forwarding
  3) Flow control/heartbeats/errors (current: heartbeats and basic errors; flow control reserved)
  4) Tests/benchmarks

---
For runnable examples and edge cases, see `codec_test.go`.

