package gorpc

import (
	"encoding/gob"
	"io"
	"net"
	"net/rpc"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRace_RespHdrSeen_ConcurrentAccess tests for race condition between
// readLoop accessing respHdrSeen without lock and failBody accessing it with mu lock.
//
// The race exists because:
// - readLoop accesses c.respHdrSeen[seq] directly without any lock (codec.go:910-921)
// - failBody deletes from c.respHdrSeen while holding mu lock (codec.go:433)
// - These are different locks, so concurrent access is possible
func TestRace_RespHdrSeen_ConcurrentAccess(t *testing.T) {
	for i := 0; i < 100; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer srvConn.Close()

			// Create client with short timeout to trigger failPending quickly
			cli := NewClientCodec(cliConn, WithTimeout(10*time.Millisecond))
			defer cli.Close()

			enc := gob.NewEncoder(srvConn)

			var wg sync.WaitGroup

			// Goroutine 1: Send response headers rapidly
			// This triggers readLoop to access respHdrSeen
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 50; seq++ {
					_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
					// Don't send body - this leaves respHdrSeen[seq] = true
				}
			}()

			// Goroutine 2: Trigger connection failures to call failPending -> failBody
			// This triggers failBody to delete from respHdrSeen
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Millisecond)
				// Close server side to trigger connection loss
				srvConn.Close()
			}()

			wg.Wait()
			time.Sleep(50 * time.Millisecond)
		}()
	}
}

// TestRace_ReqHdrSeen_ConcurrentAccess tests similar race on server side
func TestRace_ReqHdrSeen_ConcurrentAccess(t *testing.T) {
	for i := 0; i < 100; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer cliConn.Close()

			srv := NewServerCodec(srvConn)
			defer srv.Close()

			enc := gob.NewEncoder(cliConn)

			var wg sync.WaitGroup

			// Send request headers rapidly without bodies
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 50; seq++ {
					_ = enc.Encode(&frame{
						Type:          requestHeader,
						Sequence:      seq,
						StreamID:      seq,
						ServiceMethod: "Svc.Test",
					})
				}
			}()

			// Trigger connection close
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Millisecond)
				cliConn.Close()
			}()

			wg.Wait()
			time.Sleep(50 * time.Millisecond)
		}()
	}
}

// TestRace_DeliverStream_TOCTOU tests Time-of-Check-Time-of-Use race in deliverStream.
//
// The race exists because deliverStream:
// 1. Checks c.ended[seq] and reads c.streams[seq] while holding mu
// 2. Releases mu
// 3. Later re-acquires mu to close the channel
// Between steps 2 and 3, another goroutine could modify the state.
func TestRace_DeliverStream_TOCTOU(t *testing.T) {
	for i := 0; i < 100; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer srvConn.Close()

			cli := NewClientCodec(cliConn, WithTimeout(50*time.Millisecond))
			defer cli.Close()

			enc := gob.NewEncoder(srvConn)

			// Setup: send header and body to establish stream
			seq := uint64(1)
			_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
			_ = enc.Encode(&frame{Type: responseBody, Sequence: seq, StreamID: seq})

			// Read header to process it
			var resp rpc.Response
			if err := cli.ReadResponseHeader(&resp); err != nil {
				return
			}

			// Start reading body with channel
			var ch chan Event
			go func() {
				_ = cli.ReadResponseBody(&ch)
			}()

			time.Sleep(5 * time.Millisecond)

			var wg sync.WaitGroup

			// Goroutine 1: Send stream data and EOS rapidly
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					b, _ := encodeGob(Event{X: j})
					_ = enc.Encode(&frame{
						Type:      streamData,
						Sequence:  seq,
						StreamID:  seq,
						Direction: dirServerToClient,
						Payload:   b,
					})
				}
				// Send EOS
				_ = enc.Encode(&frame{
					Type:        streamData,
					Sequence:    seq,
					StreamID:    seq,
					Direction:   dirServerToClient,
					EndOfStream: true,
				})
			}()

			// Goroutine 2: Trigger close simultaneously
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(2 * time.Millisecond)
				srvConn.Close()
			}()

			wg.Wait()
			time.Sleep(50 * time.Millisecond)
		}()
	}
}

// TestRace_FailBody_DoubleClose tests potential double-close of channels.
//
// This can happen when:
// 1. deliverStream is processing an EOS and about to close the channel
// 2. failBody is called (due to connection loss) and also tries to close
func TestRace_FailBody_DoubleClose(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic from double close: %v", r)
		}
	}()

	for i := 0; i < 100; i++ {
		func() {
			srvConn, cliConn := net.Pipe()

			cli := NewClientCodec(cliConn, WithTimeout(20*time.Millisecond))

			enc := gob.NewEncoder(srvConn)

			seq := uint64(1)

			// Setup stream
			_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
			_ = enc.Encode(&frame{Type: responseBody, Sequence: seq, StreamID: seq})

			var resp rpc.Response
			if err := cli.ReadResponseHeader(&resp); err != nil {
				cli.Close()
				srvConn.Close()
				return
			}

			var ch chan Event
			go func() {
				_ = cli.ReadResponseBody(&ch)
			}()

			time.Sleep(5 * time.Millisecond)

			var wg sync.WaitGroup

			// Goroutine 1: Send EOS to trigger channel close in deliverStream
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = enc.Encode(&frame{
					Type:        streamData,
					Sequence:    seq,
					StreamID:    seq,
					Direction:   dirServerToClient,
					EndOfStream: true,
				})
			}()

			// Goroutine 2: Close connection to trigger failBody
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(1 * time.Millisecond)
				srvConn.Close()
			}()

			// Goroutine 3: Explicitly close codec
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(2 * time.Millisecond)
				cli.Close()
			}()

			wg.Wait()
			time.Sleep(30 * time.Millisecond)
		}()
	}
}

// TestRace_ConcurrentWriteAndReconnect tests race between writeFrameWithGen
// and reconnection logic.
func TestRace_ConcurrentWriteAndReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept connections
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Immediately close to trigger reconnect
			go func(c net.Conn) {
				time.Sleep(10 * time.Millisecond)
				c.Close()
			}(conn)
		}
	}()

	addr := ln.Addr().String()
	dial := func() (io.ReadWriteCloser, error) {
		return net.Dial("tcp", addr)
	}

	conn, _ := dial()
	cli := NewClientCodec(conn,
		WithDialer(dial),
		WithTimeout(50*time.Millisecond),
	)
	defer cli.Close()

	var wg sync.WaitGroup

	// Multiple goroutines trying to write
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				req := &rpc.Request{
					ServiceMethod: "Svc.Test",
					Seq:           uint64(id*100 + j),
				}
				_ = cli.WriteRequest(req, &Args{A: 1, B: 2})
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
}

// TestRace_StreamMapConcurrentAccess tests concurrent access to streams map
func TestRace_StreamMapConcurrentAccess(t *testing.T) {
	for i := 0; i < 50; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer srvConn.Close()

			cli := NewClientCodec(cliConn, WithTimeout(30*time.Millisecond))
			defer cli.Close()

			enc := gob.NewEncoder(srvConn)

			var wg sync.WaitGroup
			var activeStreams int32

			// Create multiple streams concurrently
			for seq := uint64(1); seq <= 10; seq++ {
				wg.Add(1)
				go func(s uint64) {
					defer wg.Done()
					atomic.AddInt32(&activeStreams, 1)
					defer atomic.AddInt32(&activeStreams, -1)

					_ = enc.Encode(&frame{Type: responseHeader, Sequence: s, StreamID: s})
					_ = enc.Encode(&frame{Type: responseBody, Sequence: s, StreamID: s})

					// Send some data
					for j := 0; j < 5; j++ {
						b, _ := encodeGob(Event{X: int(s)*10 + j})
						_ = enc.Encode(&frame{
							Type:      streamData,
							Sequence:  s,
							StreamID:  s,
							Direction: dirServerToClient,
							Payload:   b,
						})
					}

					// Random delay before EOS
					time.Sleep(time.Duration(s) * time.Millisecond)

					_ = enc.Encode(&frame{
						Type:        streamData,
						Sequence:    s,
						StreamID:    s,
						Direction:   dirServerToClient,
						EndOfStream: true,
					})
				}(seq)
			}

			// Concurrently close connection
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(15 * time.Millisecond)
				srvConn.Close()
			}()

			wg.Wait()
			time.Sleep(50 * time.Millisecond)
		}()
	}
}

// TestRace_PendingMapConcurrentAccess tests concurrent access to pending map
// during trackPending/untrackPending/clearPending operations
func TestRace_PendingMapConcurrentAccess(t *testing.T) {
	for i := 0; i < 50; i++ {
		func() {
			srvConn, cliConn := net.Pipe()

			cli := NewClientCodec(cliConn, WithTimeout(20*time.Millisecond))

			enc := gob.NewEncoder(srvConn)

			var wg sync.WaitGroup

			// Multiple writers
			for j := 0; j < 5; j++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for k := 0; k < 5; k++ {
						seq := uint64(id*100 + k)
						req := &rpc.Request{
							ServiceMethod: "Svc.Test",
							Seq:           seq,
						}
						_ = cli.WriteRequest(req, &Args{A: 1, B: 2})
					}
				}(j)
			}

			// Response sender
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(0); seq < 25; seq++ {
					_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
					b, _ := encodeGob(123)
					_ = enc.Encode(&frame{Type: responseBody, Sequence: seq, StreamID: seq, Payload: b})
				}
			}()

			// Connection closer
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(10 * time.Millisecond)
				srvConn.Close()
			}()

			wg.Wait()

			cli.Close()
			time.Sleep(30 * time.Millisecond)
		}()
	}
}

// TestRace_HandshakeMapConcurrentAccess tests race on handshake map
func TestRace_HandshakeMapConcurrentAccess(t *testing.T) {
	for i := 0; i < 50; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer srvConn.Close()

			cli := NewClientCodec(cliConn, WithTimeout(20*time.Millisecond))
			defer cli.Close()

			enc := gob.NewEncoder(srvConn)

			var wg sync.WaitGroup

			// Send headers and bodies rapidly
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 20; seq++ {
					_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
					_ = enc.Encode(&frame{Type: responseBody, Sequence: seq, StreamID: seq})
				}
			}()

			// Read responses concurrently (this accesses handshake map)
			for j := 0; j < 3; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for k := 0; k < 5; k++ {
						var resp rpc.Response
						if err := cli.ReadResponseHeader(&resp); err != nil {
							return
						}
						var reply int
						_ = cli.ReadResponseBody(&reply)
					}
				}()
			}

			// Close to trigger cleanup
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(15 * time.Millisecond)
				srvConn.Close()
			}()

			wg.Wait()
			time.Sleep(30 * time.Millisecond)
		}()
	}
}

// TestRace_RespHdrSeen_DirectRace is a more aggressive test for respHdrSeen race.
// It tries to trigger the race between readLoop and failBody.
func TestRace_RespHdrSeen_DirectRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		func() {
			srvConn, cliConn := net.Pipe()

			cli := NewClientCodec(cliConn, WithTimeout(5*time.Millisecond))

			enc := gob.NewEncoder(srvConn)

			var wg sync.WaitGroup

			// Sender: rapidly send headers (sets respHdrSeen[seq] = true in readLoop)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(1); seq <= 100; seq++ {
					_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
				}
			}()

			// Writer: make requests to trigger trackPending/failPending
			wg.Add(1)
			go func() {
				defer wg.Done()
				for seq := uint64(200); seq <= 250; seq++ {
					req := &rpc.Request{ServiceMethod: "Svc.Test", Seq: seq}
					_ = cli.WriteRequest(req, &Args{A: 1, B: 2})
				}
			}()

			// Closer: close connection to trigger failPending -> failBody
			// which deletes from respHdrSeen while readLoop reads/writes it
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(2 * time.Millisecond)
				srvConn.Close()
			}()

			wg.Wait()
			cli.Close()
			time.Sleep(20 * time.Millisecond)
		}()
	}
}

// TestRace_CurRespSeq tests race on curRespSeq field
// curRespSeq is written in ReadResponseHeader and read in ReadResponseBody
// without synchronization
func TestRace_CurRespSeq(t *testing.T) {
	for i := 0; i < 50; i++ {
		func() {
			srvConn, cliConn := net.Pipe()
			defer srvConn.Close()

			cli := NewClientCodec(cliConn, WithTimeout(50*time.Millisecond))
			defer cli.Close()

			enc := gob.NewEncoder(srvConn)

			// Send multiple responses
			for seq := uint64(1); seq <= 10; seq++ {
				_ = enc.Encode(&frame{Type: responseHeader, Sequence: seq, StreamID: seq})
				b, _ := encodeGob(int(seq))
				_ = enc.Encode(&frame{Type: responseBody, Sequence: seq, StreamID: seq, Payload: b})
			}

			var wg sync.WaitGroup

			// Multiple readers racing on curRespSeq
			for j := 0; j < 3; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for k := 0; k < 3; k++ {
						var resp rpc.Response
						if err := cli.ReadResponseHeader(&resp); err != nil {
							return
						}
						var reply int
						_ = cli.ReadResponseBody(&reply)
					}
				}()
			}

			wg.Wait()
		}()
	}
}
