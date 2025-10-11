package gorpc

import (
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"net/rpc"
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

type Svc struct{}

func (s *Svc) Add(args *Args, reply *int) error {
	*reply = args.A + args.B
	return nil
}

func (s *Svc) Fail(args *Args, reply *int) error {
	return errors.New("fail")
}

func (s *Svc) Ingest(args chan Item, reply *Ack) error {
	c := 0
	for range args {
		c++
	}
	*reply = Ack{OK: c > 0}
	return nil
}

func (s *Svc) Watch(args *Filter, reply *chan Event) error {
	ch := make(chan Event, 16)
	*reply = ch
	n := args.N
	go func() {
		defer close(ch)
		for i := 0; i < n; i++ {
			ch <- Event{X: i}
		}
	}()
	return nil
}

func (s *Svc) Pipe(args chan In, reply *chan Out) error {
	out := make(chan Out, 16)
	*reply = out
	go func() {
		defer close(out)
		for v := range args {
			out <- Out{V: v.V * 2}
		}
	}()
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
	var ch chan Event
	if err := c.Call("Svc.Watch", &Filter{N: 3}, &ch); err != nil {
		t.Fatalf("call: %v", err)
	}
	cnt := 0
	for v := range ch {
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
	in := make(chan In, 4)
	var out chan Out
	if err := c.Call("Svc.Pipe", in, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	in <- In{V: 5}
	in <- In{V: 7}
	close(in)
	a := <-out
	b := <-out
	_, ok := <-out
	if ok {
		t.Fatalf("out not closed")
	}
	if a.V != 10 || b.V != 14 {
		t.Fatalf("got %v %v", a, b)
	}
}

func TestBidiStream(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	in := make(chan In, 4)
	var out chan Out
	if err := c.Call("Svc.Pipe", in, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	in <- In{V: 5}
	in <- In{V: 7}
	close(in)
	a := <-out
	b := <-out
	_, ok := <-out
	if ok {
		t.Fatalf("out not closed")
	}
	if a.V != 10 || b.V != 14 {
		t.Fatalf("got %v %v", a, b)
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b1, _ := encodeGob(Event{X: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, Payload: b1})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 11, StreamID: 11, Direction: dirServerToClient, WindowUpdate: 5})
	b2, _ := encodeGob(Event{X: 2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, Payload: b2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 11, StreamID: 11, Direction: dirServerToClient, EndOfStream: true})
	a := <-ch
	if a.X != 1 {
		t.Fatalf("a=%v", a)
	}
	b := <-ch
	if b.X != 2 {
		t.Fatalf("b=%v", b)
	}
	_, ok := <-ch
	if ok {
		t.Fatalf("ch not closed")
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
	var in chan In
	if err := s.ReadRequestBody(&in); err != nil {
		t.Fatalf("req body: %v", err)
	}
	b1, _ := encodeGob(In{V: 3})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, Payload: b1})
	_ = enc.Encode(&frame{Type: streamWindowUpdate, Sequence: 21, StreamID: 21, Direction: dirClientToServer, WindowUpdate: 5})
	b2, _ := encodeGob(In{V: 4})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, Payload: b2})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 21, StreamID: 21, Direction: dirClientToServer, EndOfStream: true})
	a := <-in
	if a.V != 3 {
		t.Fatalf("a=%v", a)
	}
	b := <-in
	if b.V != 4 {
		t.Fatalf("b=%v", b)
	}
	_, ok := <-in
	if ok {
		t.Fatalf("in not closed")
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b1, _ := encodeGob(Event{X: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, Payload: b1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 31, StreamID: 31, Direction: dirServerToClient, Payload: b1})
	a := <-ch
	if a.X != 1 {
		t.Fatalf("a=%v", a)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ch not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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
	var in chan In
	if err := s.ReadRequestBody(&in); err != nil {
		t.Fatalf("req body: %v", err)
	}
	b1, _ := encodeGob(In{V: 1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, Payload: b1})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 32, StreamID: 32, Direction: dirClientToServer, Payload: b1})
	a := <-in
	if a.V != 1 {
		t.Fatalf("a=%v", a)
	}
	select {
	case _, ok := <-in:
		if ok {
			t.Fatalf("in not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 41, StreamID: 41, Direction: dirServerToClient, Payload: []byte{0xff}})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "DECODE_ERROR" || f.Direction != dirClientToServer || f.Sequence != 41 {
		t.Fatalf("reset: %+v", f)
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
	var in chan In
	if err := s.ReadRequestBody(&in); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 42, StreamID: 42, Direction: dirClientToServer, Payload: []byte{0xff}})
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "DECODE_ERROR" || f.Direction != dirServerToClient || f.Sequence != 42 {
		t.Fatalf("reset: %+v", f)
	}
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientDrainsSendChanOnError(t *testing.T) {
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
	ch := make(chan Bad)
	go func() { ch <- Bad{C: make(chan int)} }()
	if err := cli.WriteRequest(&rpc.Request{Seq: 90, ServiceMethod: "S.M"}, ch); err != nil {
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
	sent := make(chan struct{})
	go func() {
		ch <- Bad{C: make(chan int)}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("send blocked")
	}
	close(ch)
	_ = cli.Close()
	_ = srvConn.Close()
}

func TestServerDrainsSendChanOnError(t *testing.T) {
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
	ch := make(chan Bad)
	go func() { ch <- Bad{C: make(chan int)} }()
	if err := s.WriteResponse(&rpc.Response{Seq: 91, ServiceMethod: "S.M"}, &ch); err != nil {
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
	sent := make(chan struct{})
	go func() {
		ch <- Bad{C: make(chan int)}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("send blocked")
	}
	close(ch)
	_ = s.Close()
	_ = cliConn.Close()
}

func TestClientChanClosedOnReset(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	cli := NewClientCodec(cliConn)
	enc := gob.NewEncoder(srvConn)
	_ = enc.Encode(&frame{Type: responseHeader, Sequence: 51, StreamID: 51})
	_ = enc.Encode(&frame{Type: responseBody, Sequence: 51, StreamID: 51})
	var resp rpc.Response
	if err := cli.ReadResponseHeader(&resp); err != nil {
		t.Fatalf("resp hdr: %v", err)
	}
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamReset, Sequence: 51, StreamID: 51, Direction: dirServerToClient, ResetCode: "CANCELLED"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ch not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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

func TestServerChanClosedOnReset(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	s := NewServerCodec(srvConn)
	enc := gob.NewEncoder(cliConn)
	_ = enc.Encode(&frame{Type: requestHeader, Sequence: 52, StreamID: 52, ServiceMethod: "Svc.Stream"})
	_ = enc.Encode(&frame{Type: requestBody, Sequence: 52, StreamID: 52})
	var req rpc.Request
	if err := s.ReadRequestHeader(&req); err != nil {
		t.Fatalf("req hdr: %v", err)
	}
	var in chan In
	if err := s.ReadRequestBody(&in); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamReset, Sequence: 52, StreamID: 52, Direction: dirClientToServer, ResetCode: "CANCELLED"})
	select {
	case _, ok := <-in:
		if ok {
			t.Fatalf("in not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
	}
	_ = s.Close()
	_ = cliConn.Close()
}

type Bad struct{ C chan int }

func (s *Svc) WatchNil(args *Filter, reply *chan Event) error {
	var ch chan Event
	*reply = ch
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	var f frame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Type != streamReset || f.ResetCode != "TIMEOUT" || f.Sequence != 72 {
		t.Fatalf("reset: %+v", f)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ch not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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

func TestServerToClientNilChannelClosesImmediately(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var ch chan Event
	if err := c.Call("Svc.WatchNil", &Filter{N: 0}, &ch); err != nil {
		t.Fatalf("call: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ch not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
	}
}

func TestClientNilArgsChanSendsEOS(t *testing.T) {
	c, closeFn := newClientServer(t)
	defer closeFn()
	var in chan Item
	var ack Ack
	if err := c.Call("Svc.Ingest", in, &ack); err != nil {
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
	ch := make(chan Bad, 1)
	ch <- Bad{C: make(chan int)}
	if err := cli.WriteRequest(&rpc.Request{Seq: 81, ServiceMethod: "S.M"}, ch); err != nil {
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
	ch := make(chan Bad, 1)
	ch <- Bad{C: make(chan int)}
	if err := s.WriteResponse(&rpc.Response{Seq: 82}, &ch); err != nil {
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	b, _ := encodeGob(Event{X: 7})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 91, Direction: dirServerToClient, Payload: b})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 91, Direction: dirServerToClient, EndOfStream: true})
	a := <-ch
	if a.X != 7 {
		t.Fatalf("a=%v", a)
	}
	_, ok := <-ch
	if ok {
		t.Fatalf("ch not closed")
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
	var ch chan Event
	if err := cli.ReadResponseBody(&ch); err != nil {
		t.Fatalf("resp body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 201, Direction: dirServerToClient, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 201, Direction: dirServerToClient, EndOfStream: true})
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ch not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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
	var in chan In
	if err := s.ReadRequestBody(&in); err != nil {
		t.Fatalf("req body: %v", err)
	}
	_ = enc.Encode(&frame{Type: streamData, Sequence: 202, Direction: dirClientToServer, EndOfStream: true})
	_ = enc.Encode(&frame{Type: streamData, Sequence: 202, Direction: dirClientToServer, EndOfStream: true})
	select {
	case _, ok := <-in:
		if ok {
			t.Fatalf("in not closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout")
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

	client, err := NewClient(fmt.Sprintf("go://%s?timeout=1", addr))
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

	deadline := time.Now().Add(2 * time.Second)
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
