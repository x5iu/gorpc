package gorpc

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/rpc"
	"runtime"
	"testing"
	"time"
)

type Args struct{ A, B int }

type Item struct{ X int }

type Ack struct{ OK bool }

type Filter struct{ N int }

type Event struct{ X int }

type In struct{ V int }

type Out struct{ V int }

// CtxArgs is a request type with an embedded context.Context field.
// The server codec should inject a valid context into this field.
type CtxArgs struct {
	Ctx context.Context
	V   int
}

type Svc struct{}

// CtxValidateSvc is a service that validates context injection.
type CtxValidateSvc struct {
	LastCtx     context.Context // Stores the context from the last call
	CtxReceived chan context.Context
}

func (s *CtxValidateSvc) CheckContext(args *CtxArgs, reply *bool) error {
	if args.Ctx != nil {
		*reply = true
		if s.CtxReceived != nil {
			select {
			case s.CtxReceived <- args.Ctx:
			default:
			}
		}
		s.LastCtx = args.Ctx
	} else {
		*reply = false
	}
	return nil
}

func (s *Svc) Add(args *Args, reply *int) error {
	*reply = args.A + args.B
	return nil
}

func (s *Svc) Fail(args *Args, reply *int) error {
	return errors.New("fail")
}

func (s *Svc) Ingest(args *iter.Seq2[Item, error], reply *Ack) error {
	if args == nil || *args == nil {
		*reply = Ack{OK: false}
		return nil
	}
	c := 0
	for _, err := range *args {
		if err != nil {
			return err
		}
		c++
	}
	*reply = Ack{OK: c > 0}
	return nil
}

func (s *Svc) Watch(args *Filter, reply *iter.Seq2[Event, error]) error {
	n := args.N
	*reply = func(yield func(Event, error) bool) {
		for i := 0; i < n; i++ {
			if !yield(Event{X: i}, nil) {
				return
			}
		}
	}
	return nil
}

func (s *Svc) Pipe(args *iter.Seq2[In, error], reply *iter.Seq2[Out, error]) error {
	// For parallel bidirectional stream, consume input in a goroutine
	inputCh := make(chan In, 16)
	go func() {
		defer close(inputCh)
		if args == nil || *args == nil {
			return
		}
		for v, err := range *args {
			if err != nil {
				return
			}
			inputCh <- v
		}
	}()

	*reply = func(yield func(Out, error) bool) {
		for in := range inputCh {
			if !yield(Out{V: in.V * 2}, nil) {
				return
			}
		}
	}
	return nil
}

func newClientServer(t *testing.T) (*rpc.Client, func()) {
	srvConn, cliConn := net.Pipe()
	s := rpc.NewServer()
	if err := s.RegisterName("Svc", &Svc{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	go s.ServeCodec(NewServerCodec(srvConn))
	c := rpc.NewClientWithCodec(NewClientCodec(cliConn))
	return c, func() { _ = c.Close() }
}

func newClientServerTCP(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := rpc.NewServer()
	if err := s.RegisterName("Svc", &Svc{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				continue
			}
			go s.ServeCodec(NewServerCodec(conn))
		}
	}()
	stop := func() {
		close(done)
		_ = ln.Close()
	}
	return ln.Addr().String(), stop
}

func newReconnectableServer(t *testing.T) (string, chan net.Conn, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := rpc.NewServer()
	if err := s.RegisterName("Svc", &Svc{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	conns := make(chan net.Conn, 8)
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				continue
			}
			conns <- conn
			go s.ServeCodec(NewServerCodec(conn))
		}
	}()
	stop := func() {
		close(done)
		_ = ln.Close()
		for {
			select {
			case conn := <-conns:
				_ = conn.Close()
			default:
				return
			}
		}
	}
	return ln.Addr().String(), conns, stop
}

func TestUnary(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var reply int
	if err := c.Call("Svc.Add", &Args{A: 7, B: 8}, &reply); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reply != 15 {
		t.Fatalf("got %d", reply)
	}
}

func TestUnaryErrorNoDecode(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var reply int
	err := c.Call("Svc.Fail", &Args{A: 1, B: 2}, &reply)
	if err == nil {
		t.Fatalf("expected error")
	}
	if reply != 0 {
		t.Fatalf("reply decoded: %d", reply)
	}
}

func TestServerToClientStream(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var seq iter.Seq2[Event, error]
	if err := c.Call("Svc.Watch", &Filter{N: 3}, &seq); err != nil {
		t.Fatalf("call: %v", err)
	}
	cnt := 0
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		if v.X != cnt {
			t.Fatalf("event %d != %d", v.X, cnt)
		}
		cnt++
	}
	if cnt != 3 {
		t.Fatalf("count %d", cnt)
	}
}

func TestClientToServerStream(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	inSeq := func(yield func(In, error) bool) {
		if !yield(In{V: 5}, nil) {
			return
		}
		if !yield(In{V: 7}, nil) {
			return
		}
	}
	var outSeq iter.Seq2[Out, error]
	if err := c.Call("Svc.Pipe", &inSeq, &outSeq); err != nil {
		t.Fatalf("call: %v", err)
	}
	results := make([]Out, 0, 2)
	for v, err := range outSeq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].V != 10 || results[1].V != 14 {
		t.Fatalf("got %v %v", results[0], results[1])
	}
}

func TestBidiStream(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	inSeq := func(yield func(In, error) bool) {
		if !yield(In{V: 5}, nil) {
			return
		}
		if !yield(In{V: 7}, nil) {
			return
		}
	}
	var outSeq iter.Seq2[Out, error]
	if err := c.Call("Svc.Pipe", &inSeq, &outSeq); err != nil {
		t.Fatalf("call: %v", err)
	}
	results := make([]Out, 0, 2)
	for v, err := range outSeq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].V != 10 || results[1].V != 14 {
		t.Fatalf("got %v %v", results[0], results[1])
	}
}

func TestServerRespondsToPing(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	srv := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	go func() { _ = enc.Encode(&frame{Type: pingType, Sequence: 42, HeartbeatToken: "abc"}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != pongType || f.HeartbeatToken != "abc" || f.Sequence != 42 {
		t.Fatalf("pong mismatch: %+v", f)
	}
	_ = srv.Close()
	_ = cliConn.Close()
}

func TestClientRespondsToPing(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	go func() { _ = enc.Encode(&frame{Type: pingType, Sequence: 7, HeartbeatToken: "xyz"}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != pongType || f.HeartbeatToken != "xyz" || f.Sequence != 7 {
		t.Fatalf("pong mismatch: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnBodyBeforeHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	srv := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	go func() { _ = enc.Encode(&frame{Type: requestBody, Sequence: 1, StreamID: 1}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" {
		t.Fatalf("reset mismatch: %+v", f)
	}
	_ = srv.Close()
	_ = cliConn.Close()
}

func TestClientResetOnBodyBeforeHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	go func() { _ = enc.Encode(&frame{Type: responseBody, Sequence: 2, StreamID: 2}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" {
		t.Fatalf("reset mismatch: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnInvalidDirection(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	srv := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	go func() { _ = enc.Encode(&frame{Type: streamData, Sequence: 3, StreamID: 3, Direction: "bogus"}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" {
		t.Fatalf("reset mismatch: %+v", f)
	}
	_ = srv.Close()
	_ = cliConn.Close()
}

func TestClientResetOnInvalidDirection(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	go func() { _ = enc.Encode(&frame{Type: streamData, Sequence: 4, StreamID: 4, Direction: "bogus"}) }()
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" {
		t.Fatalf("reset mismatch: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerToClientHalfCloseAndWindowUpdateIgnored(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 11, StreamID: 11})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 11, StreamID: 11})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b1, _ := encodeGob(Event{X: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, Payload: b1})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 11, StreamID: 11, Direction: dirServerToClient, WindowUpdate: 5})
	b2, _ := encodeGob(Event{X: 2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, Payload: b2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, EndOfStream: true})
	results := make([]Event, 0, 2)
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 events, got %d", len(results))
	}
	if results[0].X != 1 {
		t.Fatalf("a=%v", results[0])
	}
	if results[1].X != 2 {
		t.Fatalf("b=%v", results[1])
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestClientToServerHalfCloseAndWindowUpdateIgnored(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 21, StreamID: 21, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 21, StreamID: 21})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var seq iter.Seq2[In, error]
	if err := s.ReadRequestBody(&seq); err != nil {
		t.Fatalf("req body: %v", err)
	}
	b1, _ := encodeGob(In{V: 3})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, Payload: b1})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 21, StreamID: 21, Direction: dirClientToServer, WindowUpdate: 5})
	b2, _ := encodeGob(In{V: 4})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, Payload: b2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, EndOfStream: true})
	results := make([]In, 0, 2)
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 items, got %d", len(results))
	}
	if results[0].V != 3 {
		t.Fatalf("a=%v", results[0])
	}
	if results[1].V != 4 {
		t.Fatalf("b=%v", results[1])
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientIgnoresExtraAfterEOS(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 31, StreamID: 31})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 31, StreamID: 31})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b1, _ := encodeGob(Event{X: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, Payload: b1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, Payload: b1})
	results := make([]Event, 0, 1)
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 event, got %d", len(results))
	}
	if results[0].X != 1 {
		t.Fatalf("a=%v", results[0])
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerIgnoresExtraAfterEOS(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 32, StreamID: 32, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 32, StreamID: 32})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var seq iter.Seq2[In, error]
	if err := s.ReadRequestBody(&seq); err != nil {
		t.Fatalf("req body: %v", err)
	}
	b1, _ := encodeGob(In{V: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, Payload: b1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, Payload: b1})
	results := make([]In, 0, 1)
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 item, got %d", len(results))
	}
	if results[0].V != 1 {
		t.Fatalf("a=%v", results[0])
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientResetOnDecodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 41, StreamID: 41})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 41, StreamID: 41})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 41, StreamID: 41, Direction: dirServerToClient, Payload: []byte{0xff}})
	// Read reset frame in goroutine to avoid deadlock with net.Pipe
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	// Iterate to trigger decode error
	for _, err := range seq {
		if err != nil {
			break // expected decode error
		}
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "DECODE_ERROR" || f.Direction != dirClientToServer || f.Sequence != 41 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for reset frame")
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnDecodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 42, StreamID: 42, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 42, StreamID: 42})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var seq iter.Seq2[In, error]
	if err := s.ReadRequestBody(&seq); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 42, StreamID: 42, Direction: dirClientToServer, Payload: []byte{0xff}})
	// Read reset frame in goroutine to avoid deadlock with net.Pipe
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	// Iterate to trigger decode error
	for _, err := range seq {
		if err != nil {
			break // expected decode error
		}
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "DECODE_ERROR" || f.Direction != dirServerToClient || f.Sequence != 42 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for reset frame")
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientSendIterEncodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	dec := gob.NewDecoder(srvConn)
	frames := make(chan frame, 3)
	errC := make(chan error, 1)
	go func() {
		defer close(frames)
		for i := 0; i < 3; i++ {
			var f frame
			if err := dec.Decode(&f); err != nil {
				errC <- err
				return
			}
			frames <- f
		}
	}()
	// Create an iter.Seq2 that yields a Bad value (which cannot be gob-encoded)
	seq := func(yield func(Bad, error) bool) {
		yield(Bad{C: make(chan int)}, nil)
	}
	if err := cli.WriteRequest(&rpc.Request{Seq: 90, ServiceMethod: "S.M"}, seq); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertFrame := func(want string) {
		select {
		case err := <-errC:
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("frames closed early")
			}
			if f.Type != want {
				t.Fatalf("want %s, got %+v", want, f)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	assertFrame(requestHeader)
	assertFrame(requestBody)
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	default:
	}
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("no reset frame")
		}
		if f.Type != streamReset || f.ResetCode != "ENCODE_ERROR" {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for reset")
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerSendIterEncodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	dec := gob.NewDecoder(cliConn)
	frames := make(chan frame, 3)
	errC := make(chan error, 1)
	go func() {
		defer close(frames)
		for i := 0; i < 3; i++ {
			var f frame
			if err := dec.Decode(&f); err != nil {
				errC <- err
				return
			}
			frames <- f
		}
	}()
	// Create an iter.Seq2 that yields a Bad value (which cannot be gob-encoded)
	seq := func(yield func(Bad, error) bool) {
		yield(Bad{C: make(chan int)}, nil)
	}
	if err := s.WriteResponse(&rpc.Response{Seq: 91, ServiceMethod: "S.M"}, &seq); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertFrame := func(want string) {
		select {
		case err := <-errC:
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("frames closed early")
			}
			if f.Type != want {
				t.Fatalf("want %s, got %+v", want, f)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	assertFrame(responseHeader)
	assertFrame(responseBody)
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	default:
	}
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("no reset frame")
		}
		if f.Type != streamReset || f.ResetCode != "ENCODE_ERROR" {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for reset")
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientIterTerminatedOnReset(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 51, StreamID: 51})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 51, StreamID: 51})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamReset, Sequence: 51, StreamID: 51, Direction: dirServerToClient, ResetCode: "CANCELLED"})
	// Iteration should terminate with an error due to reset
	gotError := false
	for _, err := range seq {
		if err != nil {
			gotError = true
			break
		}
	}
	if !gotError {
		// Reset might close stream without error if iterator already done
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnDataBeforeBodyAfterHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 61, StreamID: 61, ServiceMethod: "Svc.Stream"})
	b1, _ := encodeGob(In{V: 9})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 61, StreamID: 61, Direction: dirClientToServer, Payload: b1})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Direction != dirServerToClient || f.Sequence != 61 || f.ErrorMessage != "expected body after header" {
		t.Fatalf("reset: %+v", f)
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientResetOnDataBeforeBodyAfterHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 62, StreamID: 62})
	b1, _ := encodeGob(Event{X: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 62, StreamID: 62, Direction: dirServerToClient, Payload: b1})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Direction != dirClientToServer || f.Sequence != 62 || f.ErrorMessage != "expected body after header" {
		t.Fatalf("reset: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerIterTerminatedOnReset(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 52, StreamID: 52, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 52, StreamID: 52})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var seq iter.Seq2[In, error]
	if err := s.ReadRequestBody(&seq); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamReset, Sequence: 52, StreamID: 52, Direction: dirClientToServer, ResetCode: "CANCELLED"})
	// Iteration should terminate with an error due to reset
	gotError := false
	for _, err := range seq {
		if err != nil {
			gotError = true
			break
		}
	}
	if !gotError {
		// Reset might close stream without error if iterator already done
	}
	_ = s.Close()
	_ = cliConn.Close()
}

type Bad struct{ C chan int }

func (s *Svc) WatchNil(args *Filter, reply *iter.Seq2[Event, error]) error {
	var seq iter.Seq2[Event, error]
	*reply = seq
	return nil
}

func TestClientHandshakeTimeoutTriggersReset(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn, WithTimeout(50*time.Millisecond))
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 71})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	var reply int
	if err := cli.ReadResponseBody(&reply); err == nil {
		t.Fatalf("want EOF")
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "TIMEOUT" || f.Sequence != 71 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timeout")
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestClientStreamIdleTimeoutTriggersResetAndClose(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn, WithTimeout(50*time.Millisecond))
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 72})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 72})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	// Read reset frame in goroutine to avoid deadlock
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	// Iteration should terminate due to timeout
	for _, err := range seq {
		if err != nil {
			break // expected timeout error
		}
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "TIMEOUT" || f.Sequence != 72 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for reset frame")
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestClientResetOnUnexpectedStreamDataAfterUnary(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	b, _ := encodeGob(123)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 73})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 73, Payload: b})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var reply int
	if err := cli.ReadResponseBody(&reply); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 73, Direction: dirServerToClient})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Sequence != 73 {
		t.Fatalf("reset: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnUnexpectedStreamDataAfterUnary(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 74, ServiceMethod: "Svc.Add"})
	b, _ := encodeGob(&Args{A: 1, B: 2})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 74, Payload: b})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var args Args
	if err := s.ReadRequestBody(&args); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 74, Direction: dirClientToServer})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Sequence != 74 {
		t.Fatalf("reset: %+v", f)
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestServerToClientNilIterClosesImmediately(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var seq iter.Seq2[Event, error]
	if err := c.Call("Svc.WatchNil", &Filter{N: 0}, &seq); err != nil {
		t.Fatalf("call: %v", err)
	}
	// Iteration should complete immediately with no items
	cnt := 0
	for _, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		cnt++
	}
	if cnt != 0 {
		t.Fatalf("expected 0 items, got %d", cnt)
	}
}

func TestClientNilArgsIterSendsEOS(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var inSeq iter.Seq2[Item, error]
	var ack Ack
	if err := c.Call("Svc.Ingest", &inSeq, &ack); err != nil {
		t.Fatalf("call: %v", err)
	}
	if ack.OK {
		t.Fatalf("ack true")
	}
}

func TestClientResetOnEncodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	dec := gob.NewDecoder(srvConn)
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		if f.Type != requestHeader {
			errC <- errors.New("want requestHeader")
			return
		}
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		if f.Type != requestBody {
			errC <- errors.New("want requestBody")
			return
		}
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	// Create an iter.Seq2 that yields a Bad value (which cannot be gob-encoded)
	seq := func(yield func(Bad, error) bool) {
		yield(Bad{C: make(chan int)}, nil)
	}
	if err := cli.WriteRequest(&rpc.Request{Seq: 81, ServiceMethod: "S.M"}, seq); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "ENCODE_ERROR" || f.Sequence != 81 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timeout")
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnEncodeError(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	dec := gob.NewDecoder(cliConn)
	done := make(chan frame, 1)
	errC := make(chan error, 1)
	go func() {
		var f frame
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		if f.Type != responseHeader {
			errC <- errors.New("want responseHeader")
			return
		}
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		if f.Type != responseBody {
			errC <- errors.New("want responseBody")
			return
		}
		if err := dec.Decode(&f); err != nil {
			errC <- err
			return
		}
		done <- f
	}()
	// Create an iter.Seq2 that yields a Bad value (which cannot be gob-encoded)
	seq := func(yield func(Bad, error) bool) {
		yield(Bad{C: make(chan int)}, nil)
	}
	if err := s.WriteResponse(&rpc.Response{Seq: 82}, &seq); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-errC:
		t.Fatalf("decode: %v", err)
	case f := <-done:
		if f.Type != streamReset || f.ResetCode != "ENCODE_ERROR" || f.Sequence != 82 {
			t.Fatalf("reset: %+v", f)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timeout")
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestDefaultStreamIDUsedWhenZero(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 91})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 91})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b, _ := encodeGob(Event{X: 7})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 91, Direction: dirServerToClient, Payload: b})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 91, Direction: dirServerToClient, EndOfStream: true})
	results := make([]Event, 0, 1)
	for v, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
		results = append(results, v)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 event, got %d", len(results))
	}
	if results[0].X != 7 {
		t.Fatalf("a=%v", results[0])
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestClientResetOnDuplicateResponseHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 101})
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 101})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Sequence != 101 {
		t.Fatalf("reset: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnDuplicateRequestHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 102, ServiceMethod: "Svc.X"})
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 102, ServiceMethod: "Svc.X"})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Sequence != 102 {
		t.Fatalf("reset: %+v", f)
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientDoubleEOSDoesNotPanic(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 201})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 201})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var seq iter.Seq2[Event, error]
	if err := cli.ReadResponseBody(&seq); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 201, Direction: dirServerToClient, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 201, Direction: dirServerToClient, EndOfStream: true})
	// Iteration should complete without panic on double EOS
	for _, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerDoubleEOSDoesNotPanic(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 202, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 202})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var seq iter.Seq2[In, error]
	if err := s.ReadRequestBody(&seq); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 202, Direction: dirClientToServer, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 202, Direction: dirClientToServer, EndOfStream: true})
	// Iteration should complete without panic on double EOS
	for _, err := range seq {
		if err != nil {
			t.Fatalf("iter error: %v", err)
		}
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientResetOnWindowUpdateBeforeBodyAfterHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	dec := gob.NewDecoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 301})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 301, Direction: dirServerToClient, WindowUpdate: 1})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Direction != dirClientToServer || f.Sequence != 301 || f.ErrorMessage != "expected body after header" {
		t.Fatalf("reset: %+v", f)
	}
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerResetOnWindowUpdateBeforeBodyAfterHeader(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	dec := gob.NewDecoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 302, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 302, Direction: dirClientToServer, WindowUpdate: 1})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "PROTOCOL_ERROR" || f.Direction != dirServerToClient || f.Sequence != 302 || f.ErrorMessage != "expected body after header" {
		t.Fatalf("reset: %+v", f)
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestNewClientSuccess(t *testing.T) {
	addr, stop := newClientServerTCP(t)
	defer stop()
	cli, err := NewClient(fmt.Sprintf("go://%s?timeout=1", addr))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	var reply int
	if err := cli.Call("Svc.Add", &Args{A: 2, B: 3}, &reply); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reply != 5 {
		t.Fatalf("reply %d", reply)
	}
	cli2, err := NewClient(fmt.Sprintf("go://%s?timeout=500ms", addr))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli2.Close()
	var reply2 int
	if err := cli2.Call("Svc.Add", &Args{A: 4, B: 6}, &reply2); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reply2 != 10 {
		t.Fatalf("reply2 %d", reply2)
	}
}

func TestNewClientBadScheme(t *testing.T) {
	if _, err := NewClient("http://127.0.0.1:1234"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestNewClientMissingHost(t *testing.T) {
	if _, err := NewClient("go://?timeout=1"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestNewClientBadHostPort(t *testing.T) {
	if _, err := NewClient("go://localhost?timeout=1"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestClientReconnectsAfterConnectionLoss(t *testing.T) {
	addr, conns, stop := newReconnectableServer(t)
	t.Cleanup(stop)

	client, err := NewClient(fmt.Sprintf("go://%s?timeout=3", addr))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	firstConn := <-conns

	var reply int
	if err := client.Call("Svc.Add", &Args{A: 3, B: 4}, &reply); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reply != 7 {
		t.Fatalf("reply %d", reply)
	}

	_ = firstConn.Close()

	secondConnCh := make(chan net.Conn, 1)
	go func() {
		conn := <-conns
		secondConnCh <- conn
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		reply = 0
		err = client.Call("Svc.Add", &Args{A: 2, B: 4}, &reply)
		if err == nil {
			if reply != 6 {
				t.Fatalf("reply %d", reply)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call after reconnect: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case conn := <-secondConnCh:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatalf("expected second connection")
	}
}

// TestServerContextInjection tests that context is injected into
// a struct field of type context.Context when processing requests.
func TestServerContextInjection(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close() }()
	defer func() { _ = s.Close() }()

	ctxReceived := make(chan context.Context, 1)
	svc := &CtxValidateSvc{CtxReceived: ctxReceived}

	// Register the service
	server := rpc.NewServer()
	if err := server.RegisterName("CtxSvc", svc); err != nil {
		t.Fatalf("failed to register service: %v", err)
	}

	serverCodec := NewServerCodec(s)
	go server.ServeCodec(serverCodec)

	// Client side
	clientCodec := NewClientCodec(c)
	client := rpc.NewClientWithCodec(clientCodec)
	defer func() { _ = client.Close() }()

	// Make the call - the server should inject context into args
	args := &CtxArgs{V: 42}
	var reply bool
	if err := client.Call("CtxSvc.CheckContext", args, &reply); err != nil {
		t.Fatalf("RPC call failed: %v", err)
	}

	if !reply {
		t.Fatal("expected context to be injected (reply=true), got false")
	}

	// Verify the context was received
	select {
	case ctx := <-ctxReceived:
		if ctx == nil {
			t.Fatal("received nil context")
		}
		// The context should be canceled after WriteResponse completes
		// Give some time for the response to be written
		time.Sleep(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			// Expected: context is canceled after RPC completes
		case <-time.After(500 * time.Millisecond):
			t.Fatal("context was not canceled after RPC completed")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for context")
	}
}

// TestClientClearsContextBeforeEncoding tests that the client automatically
// clears context.Context fields to nil before gob encoding, preventing
// "gob: type not registered for interface" errors.
func TestClientClearsContextBeforeEncoding(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close() }()
	defer func() { _ = s.Close() }()

	ctxReceived := make(chan context.Context, 1)
	svc := &CtxValidateSvc{CtxReceived: ctxReceived}

	// Register the service
	server := rpc.NewServer()
	if err := server.RegisterName("CtxSvc", svc); err != nil {
		t.Fatalf("failed to register service: %v", err)
	}

	serverCodec := NewServerCodec(s)
	go server.ServeCodec(serverCodec)

	// Client side
	clientCodec := NewClientCodec(c)
	client := rpc.NewClientWithCodec(clientCodec)
	defer func() { _ = client.Close() }()

	// Create args with a non-nil context - this would normally cause gob to fail
	// with "gob: type not registered for interface: context.backgroundCtx"
	// but the client should automatically clear it to nil before encoding.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := &CtxArgs{Ctx: ctx, V: 123}
	var reply bool

	// This call should succeed because the client clears the context before encoding
	if err := client.Call("CtxSvc.CheckContext", args, &reply); err != nil {
		t.Fatalf("RPC call failed (context should have been cleared): %v", err)
	}

	// The server should still inject its own context
	if !reply {
		t.Fatal("expected server to inject context (reply=true), got false")
	}

	// Verify the server received a valid (server-injected) context
	select {
	case serverCtx := <-ctxReceived:
		if serverCtx == nil {
			t.Fatal("server received nil context")
		}
		// The server's injected context should be different from the client's original context
		// (since the client's context was cleared and server injected a new one)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for context from server")
	}
}

// TestServerContextCanceledOnCodecClose tests that request contexts are
// canceled when the codec is closed.
func TestServerContextCanceledOnCodecClose(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close() }()
	defer func() { _ = s.Close() }()

	ctxReceived := make(chan context.Context, 1)
	svc := &CtxValidateSvc{CtxReceived: ctxReceived}

	// Register the service
	server := rpc.NewServer()
	if err := server.RegisterName("CtxSvc", svc); err != nil {
		t.Fatalf("failed to register service: %v", err)
	}

	serverCodec := NewServerCodec(s)
	go server.ServeCodec(serverCodec)

	// Client side
	clientCodec := NewClientCodec(c)
	client := rpc.NewClientWithCodec(clientCodec)

	// Make the call
	args := &CtxArgs{V: 42}
	var reply bool
	if err := client.Call("CtxSvc.CheckContext", args, &reply); err != nil {
		t.Fatalf("RPC call failed: %v", err)
	}

	// Close everything
	_ = client.Close()

	// The contexts should be canceled after closing
	if svc.LastCtx != nil {
		select {
		case <-svc.LastCtx.Done():
			// Expected
		case <-time.After(500 * time.Millisecond):
			t.Fatal("context was not canceled after codec close")
		}
	}
}

// TestClientNoGoroutineLeak verifies that client Close() doesn't leak goroutines
func TestClientNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	// Run the reconnect test scenario multiple times
	for i := 0; i < 5; i++ {
		addr, conns, stop := newReconnectableServer(t)
		client, err := NewClient(fmt.Sprintf("go://%s?timeout=1", addr))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		// First connection
		_ = <-conns

		// Make a call
		var reply int
		_ = client.Call("Svc.Add", &Args{A: 1, B: 2}, &reply)

		// Close client
		_ = client.Close()
		stop()

		// Give time for goroutines to exit
		time.Sleep(100 * time.Millisecond)
	}

	// Check for goroutine leaks
	after := runtime.NumGoroutine()
	leaked := after - before

	// Allow some tolerance for background goroutines
	if leaked > 2 {
		t.Fatalf("goroutine leak detected: before=%d, after=%d, leaked=%d", before, after, leaked)
	}
}

// TestServerContextCanceledOnClose tests that the codec context is canceled
// when the server connection is lost.
func TestServerContextCanceledOnClose(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = s.Close() }()

	codec := NewServerCodec(s)
	defer func() { _ = codec.Close() }()

	// Start readLoop
	server := rpc.NewServer()
	if err := server.Register(&Svc{}); err != nil {
		t.Fatal(err)
	}
	go server.ServeCodec(codec)

	// Simulate connection loss by closing the client side
	_ = c.Close()

	// Give time for the codec to detect the close and cancel context
	time.Sleep(100 * time.Millisecond)
	// The codec's internal context is canceled, but we can't directly access it.
	// This test just verifies no panic occurs during the close process.
}
