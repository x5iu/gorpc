// Package gorpc implements a net/rpc ClientCodec and ServerCodec over a
// simple gob-framed transport that supports unary and bidirectional streaming.
// It reuses net/rpc's concurrency/lifecycle and only replaces the wire format
// and per-call stream semantics, as described in DESIGN.md.
package gorpc

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// frame is the only on-the-wire unit. Each gob-decoded frame is self-delimiting
// (no length prefix) and carries the minimal information for multiplexing
// unary and stream messages across calls (sequence/stream) and directions.
// See §3 of DESIGN.md for detailed semantics.
type frame struct {
	Type           string // REQUEST_HEADER, REQUEST_BODY, RESPONSE_HEADER, RESPONSE_BODY, STREAM_DATA, STREAM_WINDOW_UPDATE, STREAM_RESET, PING, PONG
	Sequence       uint64 // Matches net/rpc Request/Response.Seq
	StreamID       uint64 // Optional; defaults to Sequence when 0
	Direction      string // client_to_server or server_to_client; required for STREAM_* and heartbeats
	EndOfStream    bool   // STREAM_DATA half-close marker for its Direction
	Payload        []byte // Gob-encoded user value for *_BODY and STREAM_DATA
	ErrorMessage   string // Carries RPC/protocol error text in *_HEADER or STREAM_RESET
	WindowUpdate   uint64 // Reserved for future flow control
	ResetCode      string // STREAM_RESET reason code
	ServiceMethod  string // Service.Method; present in *_HEADER
	HeartbeatToken string // Echo token for PING/PONG
}

const (
	requestHeader      = "REQUEST_HEADER"
	requestBody        = "REQUEST_BODY"
	responseHeader     = "RESPONSE_HEADER"
	responseBody       = "RESPONSE_BODY"
	streamData         = "STREAM_DATA"
	streamWindowUpdate = "STREAM_WINDOW_UPDATE" // Reserved (not enforced yet)
	streamReset        = "STREAM_RESET"
	streamError        = "STREAM_ERROR" // Transmit errors from iter.Seq2
	pingType           = "PING"
	pongType           = "PONG"

	dirClientToServer = "client_to_server"
	dirServerToClient = "server_to_client"
)

const (
	bufHdr      = 64
	bufBody     = 64
	bufStream   = 128
	userChanBuf = 16
)

var (
	errConnectionLost = errors.New("gorpc: connection lost")
	errCodecClosed    = errors.New("gorpc: codec closed")
)

type clientCodec struct {
	rwc io.ReadWriteCloser
	dec *gob.Decoder
	enc *gob.Encoder
	wmu sync.Mutex

	connMu         sync.Mutex
	dial           func() (io.ReadWriteCloser, error)
	backoff        Backoff
	backoffFactory func() Backoff
	shutdown       bool

	respHdrCh chan frame
	// mu protects all shared state accessed by readLoop and other goroutines:
	// respBody, streams, closed, respHdrSeen, ended, handshake, s2cWanted, pending
	mu          sync.Mutex
	respBody    map[uint64]chan frame
	streams     map[uint64]chan frame
	closed      chan struct{}
	respHdrSeen map[uint64]bool
	ended       map[uint64]bool
	handshake   map[uint64]bool
	s2cWanted   map[uint64]bool
	pending     map[uint64]*pendingState
	closeOnce   sync.Once
	closedOnce  sync.Once

	// Track the connection being used by readLoop
	readingMu  sync.Mutex
	readingRwc io.ReadWriteCloser

	wg sync.WaitGroup

	// Bumps each time a new connection is established.
	connGen uint64
	// curRespSeq is used to pass sequence number between ReadResponseHeader and ReadResponseBody.
	// Use atomic to avoid race when codec is misused with concurrent readers.
	curRespSeq    atomic.Uint64
	streamTimeout time.Duration

	// Context for connection lifecycle; canceled when codec closes.
	ctx       context.Context
	ctxCancel context.CancelFunc
}

type serverCodec struct {
	rwc io.ReadWriteCloser
	dec *gob.Decoder
	enc *gob.Encoder
	wmu sync.Mutex

	reqHdrCh   chan frame
	mu         sync.Mutex
	reqBody    map[uint64]chan frame
	streams    map[uint64]chan frame
	closed     chan struct{}
	reqHdrSeen map[uint64]bool
	ended      map[uint64]bool
	handshake  map[uint64]bool
	c2sWanted  map[uint64]bool
	closeOnce  sync.Once
	closedOnce sync.Once

	wg sync.WaitGroup

	lastReqHeader lastSeq
	streamTimeout time.Duration

	// Context for connection lifecycle; canceled when codec closes.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// Per-request context cancel functions, keyed by sequence number.
	reqCtxMu      sync.Mutex
	reqCtxCancels map[uint64]context.CancelFunc
}

type ClientOption func(*clientCodec)

func WithTimeout(d time.Duration) ClientOption { return func(c *clientCodec) { c.streamTimeout = d } }

func WithDialer(d func() (io.ReadWriteCloser, error)) ClientOption {
	return func(c *clientCodec) {
		c.connMu.Lock()
		c.dial = d
		if c.backoff == nil && c.backoffFactory != nil {
			c.backoff = c.backoffFactory()
		}
		c.connMu.Unlock()
	}
}

func WithReconnectBackoff(factory func() Backoff) ClientOption {
	return func(c *clientCodec) {
		c.connMu.Lock()
		c.backoffFactory = factory
		c.backoff = nil
		c.connMu.Unlock()
	}
}

type Backoff interface {
	Next() time.Duration
	Reset()
}

type exponentialBackoff struct {
	base time.Duration
	max  time.Duration
	cur  time.Duration
}

func (b *exponentialBackoff) Next() time.Duration {
	if b.base <= 0 {
		return 0
	}
	if b.cur == 0 {
		b.cur = b.base
		return b.cur
	}
	b.cur *= 2
	if b.max > 0 && b.cur > b.max {
		b.cur = b.max
	}
	return b.cur
}

func (b *exponentialBackoff) Reset() {
	b.cur = 0
}

type pendingState struct {
	headerDelivered bool
}

type ServerOption func(*serverCodec)

func WithServerTimeout(d time.Duration) ServerOption {
	return func(s *serverCodec) { s.streamTimeout = d }
}

func NewClientCodec(rwc io.ReadWriteCloser, opts ...ClientOption) rpc.ClientCodec {
	ctx, cancel := context.WithCancel(context.Background())
	c := &clientCodec{
		rwc:         rwc,
		respHdrCh:   make(chan frame, bufHdr),
		respBody:    make(map[uint64]chan frame),
		streams:     make(map[uint64]chan frame),
		closed:      make(chan struct{}),
		respHdrSeen: make(map[uint64]bool),
		ended:       make(map[uint64]bool),
		handshake:   make(map[uint64]bool),
		s2cWanted:   make(map[uint64]bool),
		pending:     make(map[uint64]*pendingState),
		backoffFactory: func() Backoff {
			return &exponentialBackoff{base: 100 * time.Millisecond, max: 2 * time.Second}
		},
		ctx:       ctx,
		ctxCancel: cancel,
	}
	if rwc != nil {
		c.dec = gob.NewDecoder(rwc)
		c.enc = gob.NewEncoder(rwc)
	}
	for _, opt := range opts {
		opt(c)
	}
	c.connMu.Lock()
	if c.backoff == nil && c.backoffFactory != nil {
		c.backoff = c.backoffFactory()
	}
	c.connMu.Unlock()
	c.wg.Add(1)
	go c.readLoop()
	return c
}

func NewServerCodec(rwc io.ReadWriteCloser, opts ...ServerOption) rpc.ServerCodec {
	ctx, cancel := context.WithCancel(context.Background())
	s := &serverCodec{
		rwc:           rwc,
		dec:           gob.NewDecoder(rwc),
		enc:           gob.NewEncoder(rwc),
		reqHdrCh:      make(chan frame, bufHdr),
		reqBody:       make(map[uint64]chan frame),
		streams:       make(map[uint64]chan frame),
		closed:        make(chan struct{}),
		reqHdrSeen:    make(map[uint64]bool),
		ended:         make(map[uint64]bool),
		handshake:     make(map[uint64]bool),
		c2sWanted:     make(map[uint64]bool),
		ctx:           ctx,
		ctxCancel:     cancel,
		reqCtxCancels: make(map[uint64]context.CancelFunc),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.wg.Add(1)
	go s.readLoop()
	return s
}

func (c *clientCodec) ensureConnected() error {
	if err := c.connect(); err != nil {
		return err
	}
	return nil
}

func (c *clientCodec) connect() error {
	for {
		c.connMu.Lock()
		if c.shutdown {
			c.connMu.Unlock()
			return errCodecClosed
		}
		if c.rwc != nil {
			c.connMu.Unlock()
			return nil
		}
		dial := c.dial
		c.connMu.Unlock()
		if dial == nil {
			return errCodecClosed
		}
		conn, err := dial()
		if err != nil {
			wait := c.nextBackoff()
			if wait <= 0 {
				wait = 100 * time.Millisecond
			}
			select {
			case <-time.After(wait):
				continue
			case <-c.closed:
				return errCodecClosed
			}
		}
		c.connMu.Lock()
		if c.shutdown {
			c.connMu.Unlock()
			_ = conn.Close()
			return errCodecClosed
		}
		// Check if another goroutine already established a connection
		if c.rwc != nil {
			c.connMu.Unlock()
			_ = conn.Close()
			return nil
		}
		c.rwc = conn
		c.dec = gob.NewDecoder(conn)
		c.enc = gob.NewEncoder(conn)
		// Increment connection generation to invalidate in-flight writers.
		c.connGen++
		if c.backoff != nil {
			c.backoff.Reset()
		}
		c.connMu.Unlock()
		return nil
	}
}

func (c *clientCodec) nextBackoff() time.Duration {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.backoff == nil {
		if c.backoffFactory != nil {
			c.backoff = c.backoffFactory()
		} else {
			return 0
		}
	}
	return c.backoff.Next()
}

func (c *clientCodec) markDisconnected() {
	c.connMu.Lock()
	if c.rwc != nil {
		_ = c.rwc.Close()
	}
	c.rwc = nil
	c.dec = nil
	c.enc = nil
	c.connMu.Unlock()
}

func (c *clientCodec) trackPending(seq uint64) {
	c.mu.Lock()
	if _, ok := c.pending[seq]; !ok {
		c.pending[seq] = &pendingState{}
	}
	delete(c.ended, seq)
	c.mu.Unlock()
	// Start a header timeout watchdog so a lost connection between write and
	// response header doesn't block net/rpc indefinitely.
	if c.streamTimeout > 0 {
		go func(seq uint64, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				c.mu.Lock()
				st, has := c.pending[seq]
				ended := c.ended[seq]
				c.mu.Unlock()
				if has && (st == nil || !st.headerDelivered) && !ended {
					// Synthesize an error header and fail the body to unblock waiters.
					h := frame{Type: responseHeader, Sequence: seq, StreamID: seq, ErrorMessage: "TIMEOUT"}
					select {
					case c.respHdrCh <- h:
					default:
						go func() {
							select {
							case c.respHdrCh <- h:
							case <-c.closed:
							}
						}()
					}
					c.failBody(seq)
				}
			case <-c.closed:
				return
			}
		}(seq, c.streamTimeout)
	}
}

func (c *clientCodec) untrackPending(seq uint64) {
	c.mu.Lock()
	delete(c.pending, seq)
	c.mu.Unlock()
}

func (c *clientCodec) markHeaderDelivered(seq uint64) {
	c.mu.Lock()
	if st, ok := c.pending[seq]; ok {
		st.headerDelivered = true
	}
	c.mu.Unlock()
}

func (c *clientCodec) clearPending(seq uint64) {
	c.mu.Lock()
	delete(c.pending, seq)
	c.mu.Unlock()
}

func (c *clientCodec) failPending(err error) {
	c.mu.Lock()
	seqs := make([]uint64, 0, len(c.pending))
	headerDelivered := make(map[uint64]bool, len(c.pending))
	for seq, st := range c.pending {
		seqs = append(seqs, seq)
		if st != nil && st.headerDelivered {
			headerDelivered[seq] = true
		}
	}
	c.mu.Unlock()
	if len(seqs) == 0 {
		return
	}
	for _, seq := range seqs {
		if !headerDelivered[seq] {
			h := frame{Type: responseHeader, Sequence: seq, StreamID: seq, ErrorMessage: err.Error()}
			select {
			case c.respHdrCh <- h:
			default:
				go func(f frame) {
					select {
					case c.respHdrCh <- f:
					case <-c.closed:
					}
				}(h)
			}
		}
		c.failBody(seq)
	}
}

func (c *clientCodec) failBody(seq uint64) {
	c.mu.Lock()
	if ch, ok := c.respBody[seq]; ok {
		close(ch)
		delete(c.respBody, seq)
	}
	if ch, ok := c.streams[seq]; ok && ch != nil {
		close(ch)
		delete(c.streams, seq)
	}
	delete(c.handshake, seq)
	delete(c.s2cWanted, seq)
	delete(c.respHdrSeen, seq)
	delete(c.pending, seq)
	c.ended[seq] = true
	c.mu.Unlock()
}

func (c *clientCodec) isShutdown() bool {
	c.connMu.Lock()
	shutdown := c.shutdown
	c.connMu.Unlock()
	return shutdown
}

func NewClient(rawURL string) (*rpc.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "go" {
		return nil, fmt.Errorf("gorpc: unsupported scheme %q", u.Scheme)
	}
	addr := u.Host
	if addr == "" {
		return nil, errors.New("gorpc: missing host:port")
	}
	if _, _, err = net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("gorpc: expected host:port: %w", err)
	}
	dial := func() (io.ReadWriteCloser, error) {
		return net.Dial("tcp", addr)
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	var opts []ClientOption
	opts = append(opts, WithDialer(dial))
	q := u.Query()
	if v := q.Get("timeout"); v != "" {
		d, err := parseClientTimeout(v)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("gorpc: bad timeout: %w", err)
		}
		opts = append(opts, WithTimeout(d))
	}
	return rpc.NewClientWithCodec(NewClientCodec(conn, opts...)), nil
}

func parseClientTimeout(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

func (c *clientCodec) callOnClose() {
	// Cancel the codec-level context, which cascades to all per-request contexts
	if c.ctxCancel != nil {
		c.ctxCancel()
	}
}

func (c *clientCodec) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.connMu.Lock()
		c.shutdown = true
		rwc := c.rwc
		c.rwc = nil
		c.dec = nil
		c.enc = nil
		c.connMu.Unlock()

		// Get the connection being used by readLoop
		c.readingMu.Lock()
		readingRwc := c.readingRwc
		c.readingRwc = nil
		c.readingMu.Unlock()

		// Close the main connection
		if rwc != nil {
			err = rwc.Close()
		}

		// Close the connection being used by readLoop (if different)
		// This ensures any blocking Decode operation will be interrupted
		if readingRwc != nil && readingRwc != rwc {
			_ = readingRwc.Close()
		}

		c.failPending(errCodecClosed)
		c.callOnClose()
	})
	c.closedOnce.Do(func() { close(c.closed) })

	// Wait for readLoop to exit with timeout
	// Closing the connection (above) should interrupt any blocking Decode,
	// so readLoop should exit quickly (typically within milliseconds)
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// readLoop exited cleanly
	case <-time.After(2 * time.Second):
		// This should rarely happen since we closed all connections
		// If timeout occurs, readLoop goroutine may leak, but Close() won't hang forever
	}

	c.mu.Lock()
	for seq, ch := range c.respBody {
		close(ch)
		delete(c.respBody, seq)
	}
	for seq, ch := range c.streams {
		if ch != nil {
			close(ch)
		}
		delete(c.streams, seq)
	}
	c.pending = make(map[uint64]*pendingState)
	c.handshake = make(map[uint64]bool)
	c.ended = make(map[uint64]bool)
	c.s2cWanted = make(map[uint64]bool)
	c.mu.Unlock()
	return err
}

func (s *serverCodec) callOnClose() {
	// Cancel the codec-level context, which cascades to all per-request contexts
	if s.ctxCancel != nil {
		s.ctxCancel()
	}
}

func (s *serverCodec) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.rwc.Close()
		s.callOnClose()
	})
	s.closedOnce.Do(func() { close(s.closed) })
	s.wg.Wait()
	s.mu.Lock()
	for seq, ch := range s.reqBody {
		close(ch)
		delete(s.reqBody, seq)
	}
	for seq, ch := range s.streams {
		if !s.ended[seq] {
			close(ch)
		}
		delete(s.streams, seq)
	}
	for seq := range s.handshake {
		delete(s.handshake, seq)
	}
	for seq := range s.ended {
		delete(s.ended, seq)
	}
	for seq := range s.c2sWanted {
		delete(s.c2sWanted, seq)
	}
	s.mu.Unlock()
	return err
}

func (c *clientCodec) WriteRequest(r *rpc.Request, body any) error {
	c.trackPending(r.Seq)
	// Ensure we have a connection and snapshot its generation to avoid
	// splitting one logical request across different connections.
	if err := c.ensureConnected(); err != nil {
		c.untrackPending(r.Seq)
		return err
	}
	c.connMu.Lock()
	expectGen := c.connGen
	c.connMu.Unlock()

	f1 := frame{Type: requestHeader, Sequence: r.Seq, StreamID: r.Seq, ServiceMethod: r.ServiceMethod}
	if err := c.writeFrameWithGen(f1, expectGen); err != nil {
		c.untrackPending(r.Seq)
		return err
	}
	if isIterSeq2WithError(body) || isPtrToIterSeq2WithError(body) {
		f2 := frame{Type: requestBody, Sequence: r.Seq, StreamID: r.Seq, Payload: nil}
		if err := c.writeFrameWithGen(f2, expectGen); err != nil {
			c.untrackPending(r.Seq)
			return err
		}
		// If body is a pointer to iter.Seq2, dereference it
		iterVal := body
		if isPtrToIterSeq2WithError(body) {
			iterVal = reflect.ValueOf(body).Elem().Interface()
		}
		go c.pumpSendIter(r.Seq, iterVal, dirClientToServer)
		return nil
	}
	// Clear context.Context fields to nil before gob encoding,
	// since gob cannot encode non-nil context.Context interface values.
	clearContext(reflect.ValueOf(body))
	b, err := encodeGob(body)
	if err != nil {
		c.untrackPending(r.Seq)
		return err
	}
	f2 := frame{Type: requestBody, Sequence: r.Seq, StreamID: r.Seq, Payload: b}
	if err := c.writeFrameWithGen(f2, expectGen); err != nil {
		c.untrackPending(r.Seq)
		return err
	}
	return nil
}
func (c *clientCodec) ReadResponseHeader(r *rpc.Response) error {
	f, ok := c.nextRespHeader()
	if !ok {
		return io.EOF
	}
	r.ServiceMethod = f.ServiceMethod
	r.Seq = f.Sequence
	r.Error = f.ErrorMessage
	c.curRespSeq.Store(f.Sequence)
	c.markHeaderDelivered(f.Sequence)
	return nil
}

func (c *clientCodec) ReadResponseBody(body any) error {
	seq := c.curRespSeq.Load()
	defer c.clearPending(seq)
	if body == nil {
		f := c.waitBody(seq, false)
		if f == nil {
			c.mu.Lock()
			delete(c.handshake, seq)
			c.mu.Unlock()
			return nil
		}
		c.mu.Lock()
		delete(c.handshake, seq)
		c.mu.Unlock()
		return nil
	}
	if isPtrToIterSeq2WithError(body) {
		ptrVal := reflect.ValueOf(body).Elem()
		elemType := getIterSeq2ElemType(ptrVal.Type())
		c.mu.Lock()
		c.s2cWanted[seq] = true
		c.mu.Unlock()
		sch := c.ensureStream(seq)
		iterVal := c.makeRecvIter(seq, elemType, sch)
		ptrVal.Set(reflect.ValueOf(iterVal))
		f := c.waitBody(seq, true)
		if f == nil {
			return io.EOF
		}
		return nil
	}
	f := c.waitBody(seq, false)
	if f == nil {
		return io.EOF
	}
	err := decodeGob(f.Payload, body)
	c.mu.Lock()
	delete(c.handshake, seq)
	c.mu.Unlock()
	return err
}
func (s *serverCodec) ReadRequestHeader(r *rpc.Request) error {
	f, ok := s.nextReqHeader()
	if !ok {
		return io.EOF
	}
	r.ServiceMethod = f.ServiceMethod
	r.Seq = f.Sequence
	s.lastReqHeader.set(f.Sequence)
	return nil
}

func (s *serverCodec) ReadRequestBody(body any) error {
	if body == nil {
		seq := s.peekLastHeaderSeq()
		f := s.waitReqBody(seq)
		if f == nil {
			return io.EOF
		}
		s.mu.Lock()
		delete(s.handshake, seq)
		s.mu.Unlock()
		return nil
	}
	seq := s.peekLastHeaderSeq()
	if isPtrToIterSeq2WithError(body) {
		ptrVal := reflect.ValueOf(body).Elem()
		elemType := getIterSeq2ElemType(ptrVal.Type())
		s.mu.Lock()
		s.c2sWanted[seq] = true
		s.mu.Unlock()
		sch := s.ensureStream(seq)
		iterVal := s.makeRecvIter(seq, elemType, sch)
		ptrVal.Set(reflect.ValueOf(iterVal))
		f := s.waitReqBody(seq)
		if f == nil {
			return io.EOF
		}
		// Inject context into the body (args) after handshake
		cancel := injectContext(reflect.ValueOf(body), s.ctx)
		s.registerReqCtxCancel(seq, cancel)
		return nil
	}
	f := s.waitReqBody(seq)
	if f == nil {
		return io.EOF
	}
	err := decodeGob(f.Payload, body)
	// Inject context into the body (args) after decoding
	cancel := injectContext(reflect.ValueOf(body), s.ctx)
	s.registerReqCtxCancel(seq, cancel)
	s.mu.Lock()
	delete(s.handshake, seq)
	s.mu.Unlock()
	return err
}

func (s *serverCodec) WriteResponse(r *rpc.Response, body any) error {
	defer s.cancelReqCtx(r.Seq)
	f1 := frame{Type: responseHeader, Sequence: r.Seq, StreamID: r.Seq, ServiceMethod: r.ServiceMethod, ErrorMessage: r.Error}
	if err := s.writeFrame(f1); err != nil {
		return err
	}
	if isPtrToIterSeq2WithError(body) {
		f2 := frame{Type: responseBody, Sequence: r.Seq, StreamID: r.Seq, Payload: nil}
		if err := s.writeFrame(f2); err != nil {
			return err
		}
		go s.pumpSendIter(r.Seq, reflect.ValueOf(body).Elem().Interface(), dirServerToClient)
		return nil
	}
	b, err := encodeGob(body)
	if err != nil {
		return err
	}
	f2 := frame{Type: responseBody, Sequence: r.Seq, StreamID: r.Seq, Payload: b}
	return s.writeFrame(f2)
}

func (c *clientCodec) writeFrame(f frame) error {
	// Do not establish new connections from writers; only the read loop
	// is responsible for dialing. This avoids read/write connection split.
	c.connMu.Lock()
	rwc := c.rwc
	enc := c.enc
	c.connMu.Unlock()
	if enc == nil {
		return errConnectionLost
	}
	// Apply a bounded write deadline to avoid indefinite stalls on write.
	if conn, ok := rwc.(net.Conn); ok {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	}
	c.wmu.Lock()
	err := enc.Encode(&f)
	c.wmu.Unlock()
	if err != nil {
		c.markDisconnected()
		c.failPending(errConnectionLost)
	}
	return err
}

func (s *serverCodec) writeFrame(f frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.enc.Encode(&f)
}

// writeFrameWithGen writes a frame only if the connection generation matches
// the expected value captured by the caller. This prevents a single logical
// request (header + body) from being split across different TCP connections
// during client reconnection, which can otherwise lead to hung calls.
func (c *clientCodec) writeFrameWithGen(f frame, expectGen uint64) error {
	// Writers never dial. Ensure we still have a connection and that the
	// generation matches to prevent split-writes across connections.
	c.connMu.Lock()
	if c.connGen != expectGen || c.enc == nil {
		c.connMu.Unlock()
		return errConnectionLost
	}
	rwc := c.rwc
	enc := c.enc
	// Apply a bounded write deadline for safety while holding connMu.
	if conn, ok := rwc.(net.Conn); ok {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	}
	c.wmu.Lock()
	err := enc.Encode(&f)
	c.wmu.Unlock()
	c.connMu.Unlock()
	if err != nil {
		c.markDisconnected()
		c.failPending(errConnectionLost)
	}
	return err
}

func (c *clientCodec) readLoop() {
	defer c.wg.Done()
	defer c.closedOnce.Do(func() { close(c.closed) })
	defer c.callOnClose()
	defer func() {
		// Clean up readingRwc reference on exit
		c.readingMu.Lock()
		c.readingRwc = nil
		c.readingMu.Unlock()
	}()
	for {
		if err := c.ensureConnected(); err != nil {
			if c.isShutdown() {
				return
			}
			if err != errCodecClosed {
				c.failPending(err)
			}
			if err == errCodecClosed || c.dial == nil {
				return
			}
			continue
		}
		c.connMu.Lock()
		dec := c.dec
		rwc := c.rwc
		c.connMu.Unlock()

		// Save the connection being used for reading
		c.readingMu.Lock()
		c.readingRwc = rwc
		c.readingMu.Unlock()

		if dec == nil {
			continue
		}

		// Check shutdown before blocking Decode
		if c.isShutdown() {
			return
		}

		var f frame
		if err := dec.Decode(&f); err != nil {
			if c.isShutdown() {
				return
			}
			c.markDisconnected()
			c.failPending(errConnectionLost)
			if c.dial == nil {
				return
			}
			continue
		}
		if f.StreamID == 0 {
			f.StreamID = f.Sequence
		}
		switch f.Type {
		case responseHeader:
			c.mu.Lock()
			seen := c.respHdrSeen[f.Sequence]
			if seen {
				c.mu.Unlock()
				_ = c.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirClientToServer, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "duplicate header"})
				break
			}
			c.respHdrSeen[f.Sequence] = true
			c.mu.Unlock()
			c.respHdrCh <- f
		case responseBody:
			c.mu.Lock()
			seen := c.respHdrSeen[f.Sequence]
			if !seen {
				c.mu.Unlock()
				_ = c.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirClientToServer, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "body before header"})
				break
			}
			delete(c.respHdrSeen, f.Sequence)
			c.handshake[f.Sequence] = true
			c.mu.Unlock()
			c.deliverBody(f.Sequence, f)
		case streamData, streamWindowUpdate, streamReset, streamError:
			if f.Direction != dirServerToClient {
				_ = c.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirClientToServer, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "invalid direction"})
				break
			}
			c.mu.Lock()
			seen := c.respHdrSeen[f.Sequence]
			c.mu.Unlock()
			if seen {
				_ = c.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirClientToServer, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "expected body after header"})
				break
			}
			c.deliverStream(f.Sequence, f)
		case pingType:
			_ = c.writeFrame(frame{Type: pongType, Sequence: f.Sequence, StreamID: f.StreamID, HeartbeatToken: f.HeartbeatToken})
		case pongType:
		default:
		}
	}
}

func (s *serverCodec) readLoop() {
	defer s.wg.Done()
	defer s.callOnClose()
	for {
		var f frame
		if err := s.dec.Decode(&f); err != nil {
			s.closedOnce.Do(func() { close(s.closed) })
			return
		}
		if f.StreamID == 0 {
			f.StreamID = f.Sequence
		}
		switch f.Type {
		case requestHeader:
			s.mu.Lock()
			seen := s.reqHdrSeen[f.Sequence]
			if seen {
				s.mu.Unlock()
				_ = s.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirServerToClient, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "duplicate header"})
				break
			}
			s.reqHdrSeen[f.Sequence] = true
			s.mu.Unlock()
			s.reqHdrCh <- f
		case requestBody:
			s.mu.Lock()
			seen := s.reqHdrSeen[f.Sequence]
			if !seen {
				s.mu.Unlock()
				_ = s.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirServerToClient, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "body before header"})
				break
			}
			delete(s.reqHdrSeen, f.Sequence)
			s.handshake[f.Sequence] = true
			s.mu.Unlock()
			s.deliverReqBody(f.Sequence, f)
		case streamData, streamWindowUpdate, streamReset, streamError:
			if f.Direction != dirClientToServer {
				_ = s.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirServerToClient, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "invalid direction"})
				break
			}
			s.mu.Lock()
			seen := s.reqHdrSeen[f.Sequence]
			s.mu.Unlock()
			if seen {
				_ = s.writeFrame(frame{Type: streamReset, Sequence: f.Sequence, StreamID: f.StreamID, Direction: dirServerToClient, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "expected body after header"})
				break
			}
			s.deliverStream(f.Sequence, f)
		case pingType:
			_ = s.writeFrame(frame{Type: pongType, Sequence: f.Sequence, StreamID: f.StreamID, HeartbeatToken: f.HeartbeatToken})
		case pongType:
		default:
		}
	}
}

func (c *clientCodec) deliverBody(seq uint64, f frame) {
	c.mu.Lock()
	if c.ended[seq] {
		c.mu.Unlock()
		return
	}
	ch := c.respBody[seq]
	if ch == nil {
		ch = make(chan frame, bufBody)
		c.respBody[seq] = ch
	}
	c.mu.Unlock()
	ch <- f
}

func (s *serverCodec) deliverReqBody(seq uint64, f frame) {
	s.mu.Lock()
	if s.ended[seq] {
		s.mu.Unlock()
		return
	}
	ch := s.reqBody[seq]
	if ch == nil {
		ch = make(chan frame, bufBody)
		s.reqBody[seq] = ch
	}
	s.mu.Unlock()
	ch <- f
}

func (c *clientCodec) ensureStream(seq uint64) chan frame {
	c.mu.Lock()
	ch, ok := c.streams[seq]
	if !ok {
		ch = make(chan frame, bufStream)
		if c.ended[seq] {
			close(ch)
		}
		c.streams[seq] = ch
	}
	c.mu.Unlock()
	return ch
}

func (s *serverCodec) ensureStream(seq uint64) chan frame {
	s.mu.Lock()
	ch, ok := s.streams[seq]
	if !ok {
		ch = make(chan frame, bufStream)
		if s.ended[seq] {
			close(ch)
		}
		s.streams[seq] = ch
	}
	s.mu.Unlock()
	return ch
}

func (c *clientCodec) deliverStream(seq uint64, f frame) {
	c.mu.Lock()
	if c.ended[seq] {
		c.mu.Unlock()
		return
	}
	wanted := c.s2cWanted[seq]
	ch := c.streams[seq]
	c.mu.Unlock()

	// streamError is a terminal frame that needs to be delivered to the receiver
	// so it can propagate the error to the iterator, then close
	if f.Type == streamError {
		if ch == nil {
			c.mu.Lock()
			if !c.ended[seq] {
				ch = make(chan frame, bufStream)
				c.streams[seq] = ch
			}
			c.mu.Unlock()
		}
		if ch != nil {
			ch <- f
		}
		c.mu.Lock()
		if !c.ended[seq] {
			c.ended[seq] = true
			if ch2, ok := c.streams[seq]; ok && ch2 != nil {
				close(ch2)
			}
			delete(c.streams, seq)
			delete(c.handshake, seq)
			delete(c.s2cWanted, seq)
		}
		c.mu.Unlock()
		return
	}

	if f.Type == streamReset || (f.Type == streamData && f.EndOfStream) {
		c.mu.Lock()
		if !c.ended[seq] {
			c.ended[seq] = true
			if ch2, ok := c.streams[seq]; ok && ch2 != nil {
				close(ch2)
			}
			delete(c.streams, seq)
			delete(c.handshake, seq)
			delete(c.s2cWanted, seq)
		}
		c.mu.Unlock()
		return
	}

	if !wanted {
		if f.Type == streamData || f.Type == streamWindowUpdate {
			_ = c.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: f.StreamID, Direction: dirClientToServer, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "unexpected stream data"})
		}
		return
	}

	if f.Type == streamWindowUpdate {
		return
	}

	if ch == nil {
		c.mu.Lock()
		if !c.ended[seq] {
			ch = make(chan frame, bufStream)
			c.streams[seq] = ch
		}
		c.mu.Unlock()
	}
	if ch != nil {
		ch <- f
	}
}

func (s *serverCodec) deliverStream(seq uint64, f frame) {
	s.mu.Lock()
	if s.ended[seq] {
		s.mu.Unlock()
		return
	}
	wanted := s.c2sWanted[seq]
	ch := s.streams[seq]
	s.mu.Unlock()

	// streamError is a terminal frame that needs to be delivered to the receiver
	// so it can propagate the error to the iterator, then close
	if f.Type == streamError {
		if ch == nil {
			s.mu.Lock()
			if !s.ended[seq] {
				ch = make(chan frame, bufStream)
				s.streams[seq] = ch
			}
			s.mu.Unlock()
		}
		if ch != nil {
			ch <- f
		}
		s.mu.Lock()
		if !s.ended[seq] {
			s.ended[seq] = true
			if ch2, ok := s.streams[seq]; ok && ch2 != nil {
				close(ch2)
			}
			delete(s.streams, seq)
			delete(s.handshake, seq)
			delete(s.c2sWanted, seq)
		}
		s.mu.Unlock()
		return
	}

	if f.Type == streamReset || (f.Type == streamData && f.EndOfStream) {
		s.mu.Lock()
		if !s.ended[seq] {
			s.ended[seq] = true
			if ch2, ok := s.streams[seq]; ok && ch2 != nil {
				close(ch2)
			}
			delete(s.streams, seq)
			delete(s.handshake, seq)
			delete(s.c2sWanted, seq)
		}
		s.mu.Unlock()
		return
	}

	if !wanted {
		if f.Type == streamData || f.Type == streamWindowUpdate {
			_ = s.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: f.StreamID, Direction: dirServerToClient, ResetCode: "PROTOCOL_ERROR", ErrorMessage: "unexpected stream data"})
		}
		return
	}

	if f.Type == streamWindowUpdate {
		return
	}

	if ch == nil {
		s.mu.Lock()
		if !s.ended[seq] {
			ch = make(chan frame, bufStream)
			s.streams[seq] = ch
		}
		s.mu.Unlock()
	}
	if ch != nil {
		ch <- f
	}
}

func (c *clientCodec) nextRespHeader() (frame, bool) {
	select {
	case f := <-c.respHdrCh:
		return f, true
	case <-c.closed:
		return frame{}, false
	}
}

func (s *serverCodec) nextReqHeader() (frame, bool) {
	select {
	case f := <-s.reqHdrCh:
		return f, true
	case <-s.closed:
		return frame{}, false
	}
}

func (c *clientCodec) waitBody(seq uint64, expectStream bool) *frame {
	c.mu.Lock()
	// If the stream already ended before the handshake and no handshake was
	// observed, and the caller does NOT expect a server->client stream,
	// there will be no body. Avoid creating a fresh channel that would never
	// receive and return EOF to the caller. However, if the caller expects
	// a stream (s2cWanted[seq] == true), we still must deliver the responseBody
	// handshake so that net/rpc treats the call as successful and returns a
	// (already-closed) channel to the user.
	if c.ended[seq] && !c.handshake[seq] && !expectStream {
		c.mu.Unlock()
		return nil
	}
	ch := c.respBody[seq]
	if ch == nil {
		ch = make(chan frame, bufBody)
		c.respBody[seq] = ch
	}
	c.mu.Unlock()

	if c.streamTimeout <= 0 {
		select {
		case f, ok := <-ch:
			c.mu.Lock()
			delete(c.respBody, seq)
			c.mu.Unlock()
			if !ok {
				return nil
			}
			return &f
		case <-c.closed:
			return nil
		}
	}

	t := time.NewTimer(c.streamTimeout)
	defer t.Stop()
	select {
	case f, ok := <-ch:
		c.mu.Lock()
		delete(c.respBody, seq)
		c.mu.Unlock()
		if !ok {
			return nil
		}
		return &f
	case <-c.closed:
		return nil
	case <-t.C:
		// Send RESET asynchronously to avoid deadlock with net.Pipe
		go func() {
			_ = c.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq, Direction: dirClientToServer, ResetCode: "TIMEOUT", ErrorMessage: "handshake timeout"})
		}()
		c.mu.Lock()
		c.ended[seq] = true
		if ch2, ok := c.streams[seq]; ok && ch2 != nil {
			close(ch2)
		}
		delete(c.streams, seq)
		delete(c.respBody, seq)
		delete(c.s2cWanted, seq)
		delete(c.handshake, seq)
		c.mu.Unlock()
		return nil
	}
}

func (s *serverCodec) waitReqBody(seq uint64) *frame {
	s.mu.Lock()
	ch := s.reqBody[seq]
	if ch == nil {
		ch = make(chan frame, bufBody)
		s.reqBody[seq] = ch
	}
	s.mu.Unlock()

	if s.streamTimeout <= 0 {
		select {
		case f, ok := <-ch:
			s.mu.Lock()
			delete(s.reqBody, seq)
			s.mu.Unlock()
			if !ok {
				return nil
			}
			return &f
		case <-s.closed:
			return nil
		}
	}

	t := time.NewTimer(s.streamTimeout)
	defer t.Stop()
	select {
	case f, ok := <-ch:
		s.mu.Lock()
		delete(s.reqBody, seq)
		s.mu.Unlock()
		if !ok {
			return nil
		}
		return &f
	case <-s.closed:
		return nil
	case <-t.C:
		// Send RESET asynchronously to avoid deadlock with net.Pipe
		go func() {
			_ = s.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq, Direction: dirServerToClient, ResetCode: "TIMEOUT", ErrorMessage: "handshake timeout"})
		}()
		s.mu.Lock()
		s.ended[seq] = true
		if ch2, ok := s.streams[seq]; ok && ch2 != nil {
			close(ch2)
		}
		delete(s.streams, seq)
		delete(s.reqBody, seq)
		delete(s.c2sWanted, seq)
		delete(s.handshake, seq)
		s.mu.Unlock()
		return nil
	}
}

func (c *clientCodec) pumpSendIter(seq uint64, iterVal any, dir string) {
	v := reflect.ValueOf(iterVal)
	if !v.IsValid() || v.IsNil() {
		_ = c.writeFrame(frame{Type: streamData, Sequence: seq, StreamID: seq, Direction: dir, EndOfStream: true})
		return
	}

	yieldType := v.Type().In(0)
	yieldFn := reflect.MakeFunc(yieldType, func(args []reflect.Value) []reflect.Value {
		elem := args[0]
		errVal := args[1]

		select {
		case <-c.closed:
			return []reflect.Value{reflect.ValueOf(false)}
		default:
		}

		if !errVal.IsNil() {
			err := errVal.Interface().(error)
			_ = c.writeFrame(frame{
				Type: streamError, Sequence: seq, StreamID: seq,
				Direction: dir, ErrorMessage: err.Error(),
			})
			return []reflect.Value{reflect.ValueOf(false)}
		}

		b, err := encodeGob(elem.Interface())
		if err != nil {
			_ = c.writeFrame(frame{
				Type: streamReset, Sequence: seq, StreamID: seq,
				Direction: dir, ResetCode: "ENCODE_ERROR", ErrorMessage: err.Error(),
			})
			return []reflect.Value{reflect.ValueOf(false)}
		}

		if err := c.writeFrame(frame{
			Type: streamData, Sequence: seq, StreamID: seq,
			Direction: dir, Payload: b,
		}); err != nil {
			return []reflect.Value{reflect.ValueOf(false)}
		}
		return []reflect.Value{reflect.ValueOf(true)}
	})

	v.Call([]reflect.Value{yieldFn})
	_ = c.writeFrame(frame{Type: streamData, Sequence: seq, StreamID: seq, Direction: dir, EndOfStream: true})
}

func (s *serverCodec) pumpSendIter(seq uint64, iterVal any, dir string) {
	v := reflect.ValueOf(iterVal)
	if !v.IsValid() || v.IsNil() {
		_ = s.writeFrame(frame{Type: streamData, Sequence: seq, StreamID: seq, Direction: dir, EndOfStream: true})
		return
	}

	yieldType := v.Type().In(0)
	yieldFn := reflect.MakeFunc(yieldType, func(args []reflect.Value) []reflect.Value {
		elem := args[0]
		errVal := args[1]

		select {
		case <-s.closed:
			return []reflect.Value{reflect.ValueOf(false)}
		default:
		}

		if !errVal.IsNil() {
			err := errVal.Interface().(error)
			_ = s.writeFrame(frame{
				Type: streamError, Sequence: seq, StreamID: seq,
				Direction: dir, ErrorMessage: err.Error(),
			})
			return []reflect.Value{reflect.ValueOf(false)}
		}

		b, err := encodeGob(elem.Interface())
		if err != nil {
			_ = s.writeFrame(frame{
				Type: streamReset, Sequence: seq, StreamID: seq,
				Direction: dir, ResetCode: "ENCODE_ERROR", ErrorMessage: err.Error(),
			})
			return []reflect.Value{reflect.ValueOf(false)}
		}

		if err := s.writeFrame(frame{
			Type: streamData, Sequence: seq, StreamID: seq,
			Direction: dir, Payload: b,
		}); err != nil {
			return []reflect.Value{reflect.ValueOf(false)}
		}
		return []reflect.Value{reflect.ValueOf(true)}
	})

	v.Call([]reflect.Value{yieldFn})
	_ = s.writeFrame(frame{Type: streamData, Sequence: seq, StreamID: seq, Direction: dir, EndOfStream: true})
}

func (c *clientCodec) cleanupStream(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.streams, seq)
	c.ended[seq] = true
	delete(c.handshake, seq)
	delete(c.s2cWanted, seq)
}

func (c *clientCodec) makeRecvIter(seq uint64, elemType reflect.Type, sch <-chan frame) any {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	yieldType := reflect.FuncOf([]reflect.Type{elemType, errorType}, []reflect.Type{reflect.TypeOf(true)}, false)
	iterType := reflect.FuncOf([]reflect.Type{yieldType}, nil, false)

	iterFn := reflect.MakeFunc(iterType, func(args []reflect.Value) []reflect.Value {
		yield := args[0]

		var t *time.Timer
		if c.streamTimeout > 0 {
			t = time.NewTimer(c.streamTimeout)
			defer t.Stop()
		}

		for {
			var f frame
			var ok bool

			if t != nil {
				select {
				case f, ok = <-sch:
					if !t.Stop() {
						select {
						case <-t.C:
						default:
						}
					}
					t.Reset(c.streamTimeout)
				case <-t.C:
					// Send RESET asynchronously to avoid deadlock with net.Pipe
					go func() {
						_ = c.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq,
							Direction: dirClientToServer, ResetCode: "TIMEOUT", ErrorMessage: "stream inactivity"})
					}()
					c.cleanupStream(seq)
					zero := reflect.Zero(elemType)
					errVal := reflect.ValueOf(errors.New("stream timeout"))
					yield.Call([]reflect.Value{zero, errVal})
					return nil
				}
			} else {
				f, ok = <-sch
			}

			if !ok {
				c.cleanupStream(seq)
				return nil
			}

			switch f.Type {
			case streamData:
				if f.EndOfStream {
					c.cleanupStream(seq)
					return nil
				}
				v := reflect.New(elemType)
				if err := decodeGob(f.Payload, v.Interface()); err != nil {
					// Send RESET asynchronously to avoid deadlock with net.Pipe
					go func(errMsg string) {
						_ = c.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq,
							Direction: dirClientToServer, ResetCode: "DECODE_ERROR", ErrorMessage: errMsg})
					}(err.Error())
					c.cleanupStream(seq)
					errVal := reflect.ValueOf(err)
					yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
					return nil
				}
				result := yield.Call([]reflect.Value{v.Elem(), reflect.Zero(errorType)})
				if !result[0].Bool() {
					c.cleanupStream(seq)
					return nil
				}
			case streamError:
				c.cleanupStream(seq)
				errVal := reflect.ValueOf(errors.New(f.ErrorMessage))
				yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
				return nil
			case streamReset:
				c.cleanupStream(seq)
				errVal := reflect.ValueOf(fmt.Errorf("stream reset: %s", f.ResetCode))
				yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
				return nil
			}
		}
	})

	return iterFn.Interface()
}

func (s *serverCodec) cleanupStream(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, seq)
	s.ended[seq] = true
	delete(s.handshake, seq)
	delete(s.c2sWanted, seq)
}

func (s *serverCodec) makeRecvIter(seq uint64, elemType reflect.Type, sch <-chan frame) any {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	yieldType := reflect.FuncOf([]reflect.Type{elemType, errorType}, []reflect.Type{reflect.TypeOf(true)}, false)
	iterType := reflect.FuncOf([]reflect.Type{yieldType}, nil, false)

	iterFn := reflect.MakeFunc(iterType, func(args []reflect.Value) []reflect.Value {
		yield := args[0]

		var t *time.Timer
		if s.streamTimeout > 0 {
			t = time.NewTimer(s.streamTimeout)
			defer t.Stop()
		}

		for {
			var f frame
			var ok bool

			if t != nil {
				select {
				case f, ok = <-sch:
					if !t.Stop() {
						select {
						case <-t.C:
						default:
						}
					}
					t.Reset(s.streamTimeout)
				case <-t.C:
					// Send RESET asynchronously to avoid deadlock with net.Pipe
					go func() {
						_ = s.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq,
							Direction: dirServerToClient, ResetCode: "TIMEOUT", ErrorMessage: "stream inactivity"})
					}()
					s.cleanupStream(seq)
					zero := reflect.Zero(elemType)
					errVal := reflect.ValueOf(errors.New("stream timeout"))
					yield.Call([]reflect.Value{zero, errVal})
					return nil
				}
			} else {
				f, ok = <-sch
			}

			if !ok {
				s.cleanupStream(seq)
				return nil
			}

			switch f.Type {
			case streamData:
				if f.EndOfStream {
					s.cleanupStream(seq)
					return nil
				}
				v := reflect.New(elemType)
				if err := decodeGob(f.Payload, v.Interface()); err != nil {
					// Send RESET asynchronously to avoid deadlock with net.Pipe
					go func(errMsg string) {
						_ = s.writeFrame(frame{Type: streamReset, Sequence: seq, StreamID: seq,
							Direction: dirServerToClient, ResetCode: "DECODE_ERROR", ErrorMessage: errMsg})
					}(err.Error())
					s.cleanupStream(seq)
					errVal := reflect.ValueOf(err)
					yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
					return nil
				}
				result := yield.Call([]reflect.Value{v.Elem(), reflect.Zero(errorType)})
				if !result[0].Bool() {
					s.cleanupStream(seq)
					return nil
				}
			case streamError:
				s.cleanupStream(seq)
				errVal := reflect.ValueOf(errors.New(f.ErrorMessage))
				yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
				return nil
			case streamReset:
				s.cleanupStream(seq)
				errVal := reflect.ValueOf(fmt.Errorf("stream reset: %s", f.ResetCode))
				yield.Call([]reflect.Value{reflect.Zero(elemType), errVal})
				return nil
			}
		}
	})

	return iterFn.Interface()
}

// isIterSeq2WithError checks if v is of type iter.Seq2[T, error]
// iter.Seq2[T, error] signature: func(yield func(T, error) bool)
func isIterSeq2WithError(v any) bool {
	if v == nil {
		return false
	}
	return isIterSeq2WithErrorType(reflect.TypeOf(v))
}

func isIterSeq2WithErrorType(t reflect.Type) bool {
	if t.Kind() != reflect.Func {
		return false
	}
	if t.NumIn() != 1 || t.NumOut() != 0 {
		return false
	}
	yieldType := t.In(0)
	if yieldType.Kind() != reflect.Func {
		return false
	}
	if yieldType.NumIn() != 2 || yieldType.NumOut() != 1 {
		return false
	}
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if !yieldType.In(1).Implements(errorType) {
		return false
	}
	return yieldType.Out(0).Kind() == reflect.Bool
}

func isPtrToIterSeq2WithError(v any) bool {
	if v == nil {
		return false
	}
	t := reflect.TypeOf(v)
	return t.Kind() == reflect.Pointer && isIterSeq2WithErrorType(t.Elem())
}

func getIterSeq2ElemType(t reflect.Type) reflect.Type {
	return t.In(0).In(0) // func(func(T, error) bool) -> T
}

func (s *serverCodec) peekLastHeaderSeq() uint64 { return s.lastReqHeader.get() }

type lastSeq struct {
	mu sync.Mutex
	v  uint64
}

func (l *lastSeq) set(x uint64) { l.mu.Lock(); l.v = x; l.mu.Unlock() }
func (l *lastSeq) get() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.v }

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
var readerPool = sync.Pool{New: func() any { return new(bytes.Reader) }}

func encodeGob(v any) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := gob.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		bufPool.Put(buf)
		return nil, err
	}
	b := make([]byte, buf.Len())
	copy(b, buf.Bytes())
	buf.Reset()
	bufPool.Put(buf)
	return b, nil
}

func decodeGob(b []byte, out any) error {
	r := readerPool.Get().(*bytes.Reader)
	r.Reset(b)
	dec := gob.NewDecoder(r)
	err := dec.Decode(out)
	readerPool.Put(r)
	return err
}

var contextType = reflect.TypeOf((*context.Context)(nil)).Elem()

// registerReqCtxCancel stores the cancel function for a request.
func (s *serverCodec) registerReqCtxCancel(seq uint64, cancel context.CancelFunc) {
	if cancel == nil {
		return
	}
	s.reqCtxMu.Lock()
	s.reqCtxCancels[seq] = cancel
	s.reqCtxMu.Unlock()
}

// cancelReqCtx cancels and removes the context for a request.
func (s *serverCodec) cancelReqCtx(seq uint64) {
	s.reqCtxMu.Lock()
	cancel := s.reqCtxCancels[seq]
	delete(s.reqCtxCancels, seq)
	s.reqCtxMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// injectContext recursively sets context.Context fields in the given value
// using a child context derived from parent. Returns the cancel function
// that should be called when the request completes.
// The value v must be settable (typically obtained via reflect.ValueOf(&x).Elem()).
func injectContext(v reflect.Value, parent context.Context) context.CancelFunc {
	if !v.IsValid() {
		return nil
	}

	// Handle pointer: dereference if non-nil
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return injectContext(v.Elem(), parent)
	}

	// Handle interface: if it's context.Context, replace it
	if v.Kind() == reflect.Interface {
		if v.Type().Implements(contextType) && v.CanSet() {
			child, cancel := context.WithCancel(parent)
			v.Set(reflect.ValueOf(child))
			return cancel
		}
		// If the interface contains a struct, try to inject into it
		if !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			return injectContext(v.Elem(), parent)
		}
		return nil
	}

	// Handle struct: recursively check all fields
	if v.Kind() == reflect.Struct {
		var cancels []context.CancelFunc
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanSet() {
				continue
			}
			if cancel := injectContext(field, parent); cancel != nil {
				cancels = append(cancels, cancel)
			}
		}
		if len(cancels) == 0 {
			return nil
		}
		return func() {
			for _, c := range cancels {
				c()
			}
		}
	}

	return nil
}

// clearContext recursively sets context.Context fields in the given value to nil.
// This is used on the client side before gob encoding to avoid gob errors,
// since gob cannot encode non-nil context.Context interface values.
func clearContext(v reflect.Value) {
	if !v.IsValid() {
		return
	}

	// Handle pointer: dereference if non-nil
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		clearContext(v.Elem())
		return
	}

	// Handle interface: if it's context.Context, set to nil
	if v.Kind() == reflect.Interface {
		if v.Type().Implements(contextType) && v.CanSet() {
			v.Set(reflect.Zero(v.Type()))
			return
		}
		// If the interface contains a struct, try to clear its context fields
		if !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			clearContext(v.Elem())
		}
		return
	}

	// Handle struct: recursively check all fields
	if v.Kind() == reflect.Struct {
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanSet() {
				continue
			}
			clearContext(field)
		}
	}
}
