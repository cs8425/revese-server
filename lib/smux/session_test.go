package smux

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

//	"github.com/juju/ratelimit"
)

func init() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	ln, err := net.Listen("tcp", "127.0.0.1:19999")
	if err != nil {
		// handle error
		panic(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// handle error
			}
			go handleConnection(conn)
		}
	}()
}

func handleConnection(conn net.Conn) {
	session, _ := Server(conn, nil)
	for {
		if stream, err := session.AcceptStream(); err == nil {
			go func(s io.ReadWriteCloser) {
				buf := make([]byte, 65536)
				for {
					n, err := s.Read(buf)
					if err != nil {
						return
					}
					s.Write(buf[:n])
				}
			}(stream)
		} else {
			return
		}
	}
}

// p1 >> p0 >> p2
func Cp3(p1 io.Reader, p0 net.Conn, p2 io.Writer) {
	p1die := make(chan struct{})
	go func() {
		io.Copy(p0, p1) // p0 << p1
		close(p1die)
	}()

	p2die := make(chan struct{})
	go func() {
		io.Copy(p2, p0) // p2 << p0
		close(p2die)
	}()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
}

func TestEcho(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	const N = 100
	buf := make([]byte, 10)
	var sent string
	var received string
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		stream.Write([]byte(msg))
		sent += msg
		if n, err := stream.Read(buf); err != nil {
			t.Fatal(err)
		} else {
			received += string(buf[:n])
		}
	}
	if sent != received {
		t.Fatal("data mimatch")
	}
	session.Close()
}

func TestSpeed(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	t.Log(stream.LocalAddr(), stream.RemoteAddr())

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		buf := make([]byte, 1024*1024)
		nrecv := 0
		for {
			n, err := stream.Read(buf)
			if err != nil {
				t.Fatal(err)
				break
			} else {
				nrecv += n
				if nrecv == 4096*4096 {
					break
				}
			}
		}
		stream.Close()
		t.Log("time for 16MB rtt", time.Since(start))
		wg.Done()
	}()
	msg := make([]byte, 8192)
	for i := 0; i < 2048; i++ {
		stream.Write(msg)
	}
	wg.Wait()
	session.Close()
}

func TestParallel(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)

	par := 1000
	messages := 100
	var wg sync.WaitGroup
	wg.Add(par)
	for i := 0; i < par; i++ {
		stream, _ := session.OpenStream()
		go func(s *Stream) {
			buf := make([]byte, 20)
			for j := 0; j < messages; j++ {
				msg := fmt.Sprintf("hello%v", j)
				s.Write([]byte(msg))
				if _, err := s.Read(buf); err != nil {
					break
				}
			}
			s.Close()
			wg.Done()
		}(stream)
	}
	t.Log("created", session.NumStreams(), "streams")
	wg.Wait()
	session.Close()
}

func TestParallel2(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	go handleConnection(srv)
	session, _ := Client(cli, nil)

	par := 1000
	messages := 100
	var wg sync.WaitGroup
	wg.Add(par)
	for i := 0; i < par; i++ {
		stream, _ := session.OpenStream()
		go func(s *Stream) {
			buf := make([]byte, 20)
			for j := 0; j < messages; j++ {
				msg := fmt.Sprintf("hello%v", j)
				s.Write([]byte(msg))
				if _, err := s.Read(buf); err != nil {
					break
				}
			}
			s.Close()
			wg.Done()
		}(stream)
	}
	t.Log("created", session.NumStreams(), "streams")
	wg.Wait()
	session.Close()
}

func TestCloseThenOpen(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	session.Close()
	if _, err := session.OpenStream(); err == nil {
		t.Fatal("opened after close")
	}
}

func TestStreamDoubleClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	stream.Close()
	if err := stream.Close(); err == nil {
		t.Log("double close doesn't return error")
	}
	session.Close()
}

func TestConcurrentClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	numStreams := 100
	streams := make([]*Stream, 0, numStreams)
	var wg sync.WaitGroup
	wg.Add(numStreams)
	for i := 0; i < 100; i++ {
		stream, _ := session.OpenStream()
		streams = append(streams, stream)
	}
	for _, s := range streams {
		stream := s
		go func() {
			stream.Close()
			wg.Done()
		}()
	}
	session.Close()
	wg.Wait()
}

func TestTinyReadBuffer(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	const N = 100
	tinybuf := make([]byte, 6)
	var sent string
	var received string
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		sent += msg
		nsent, err := stream.Write([]byte(msg))
		if err != nil {
			t.Fatal("cannot write")
		}
		nrecv := 0
		for nrecv < nsent {
			if n, err := stream.Read(tinybuf); err == nil {
				nrecv += n
				received += string(tinybuf[:n])
			} else {
				t.Fatal("cannot read with tiny buffer")
			}
		}
	}

	if sent != received {
		t.Fatal("data mimatch")
	}
	session.Close()
}

func TestTinyReadBuffer2(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	go handleConnection(srv)

	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	const N = 100
	tinybuf := make([]byte, 6)
	var sent string
	var received string
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		sent += msg
		nsent, err := stream.Write([]byte(msg))
		if err != nil {
			t.Fatal("cannot write")
		}
		nrecv := 0
		for nrecv < nsent {
			if n, err := stream.Read(tinybuf); err == nil {
				nrecv += n
				received += string(tinybuf[:n])
			} else {
				t.Fatal("cannot read with tiny buffer")
			}
		}
	}

	if sent != received {
		t.Fatal("data mimatch")
	}
	session.Close()
}

func TestIsClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	session.Close()
	if session.IsClosed() != true {
		t.Fatal("still open after close")
	}
}

func TestKeepAliveTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:29999")
	if err != nil {
		// handle error
		panic(err)
	}
	go func() {
		ln.Accept()
	}()
	defer ln.Close()

	cli, err := net.Dial("tcp", "127.0.0.1:29999")
	if err != nil {
		t.Fatal(err)
	}

	config := DefaultConfig()
	config.KeepAliveInterval = time.Second
	config.KeepAliveTimeout = 2 * time.Second
	session, _ := Client(cli, config)
	<-time.After(3 * time.Second)
	if session.IsClosed() != true {
		t.Fatal("keepalive-timeout failed")
	}
}

func TestServerEcho(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:39999")
	if err != nil {
		// handle error
		panic(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			session, _ := Server(conn, nil)
			if stream, err := session.OpenStream(); err == nil {
				const N = 100
				buf := make([]byte, 10)
				for i := 0; i < N; i++ {
					msg := fmt.Sprintf("hello%v", i)
					stream.Write([]byte(msg))
					if n, err := stream.Read(buf); err != nil {
						t.Fatal(err)
					} else if string(buf[:n]) != msg {
						t.Fatal(err)
					}
				}
				stream.Close()
			} else {
				t.Fatal(err)
			}
		} else {
			t.Fatal(err)
		}
	}()

	cli, err := net.Dial("tcp", "127.0.0.1:39999")
	if err != nil {
		t.Fatal(err)
	}
	if session, err := Client(cli, nil); err == nil {
		if stream, err := session.AcceptStream(); err == nil {
			buf := make([]byte, 65536)
			for {
				n, err := stream.Read(buf)
				if err != nil {
					break
				}
				stream.Write(buf[:n])
			}
		} else {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
}

func TestServerEcho2(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	go func() {
		session, _ := Server(srv, nil)
		if stream, err := session.OpenStream(); err == nil {
			const N = 100
			buf := make([]byte, 10)
			for i := 0; i < N; i++ {
				msg := fmt.Sprintf("hello%v", i)
				stream.Write([]byte(msg))
				if n, err := stream.Read(buf); err != nil {
					t.Fatal(err)
				} else if string(buf[:n]) != msg {
					t.Fatal(err)
				}
			}
			stream.Close()
		} else {
			t.Fatal(err)
		}
	}()

	if session, err := Client(cli, nil); err == nil {
		if stream, err := session.AcceptStream(); err == nil {
			buf := make([]byte, 65536)
			for {
				n, err := stream.Read(buf)
				if err != nil {
					break
				}
				stream.Write(buf[:n])
			}
		} else {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
}

func TestSendWithoutRecv(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	const N = 100
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		stream.Write([]byte(msg))
	}
	buf := make([]byte, 1)
	if _, err := stream.Read(buf); err != nil {
		t.Fatal(err)
	}
	stream.Close()
}

func TestWriteAfterClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	stream.Close()
	if _, err := stream.Write([]byte("write after close")); err == nil {
		t.Fatal("write after close failed")
	}
}

func TestReadStreamAfterSessionClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	session.Close()
	buf := make([]byte, 10)
	if _, err := stream.Read(buf); err != nil {
		t.Log(err)
	} else {
		t.Fatal("read stream after session close succeeded")
	}
}

func TestWriteStreamAfterConnectionClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	session.conn.Close()
	if _, err := stream.Write([]byte("write after connection close")); err == nil {
		t.Fatal("write after connection close failed")
	}
}

func TestNumStreamAfterClose(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	if _, err := session.OpenStream(); err == nil {
		if session.NumStreams() != 1 {
			t.Fatal("wrong number of streams after opened")
		}
		session.Close()
		if session.NumStreams() != 0 {
			t.Fatal("wrong number of streams after session closed")
		}
	} else {
		t.Fatal(err)
	}
	cli.Close()
}

func TestRandomFrame(t *testing.T) {
	// pure random
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	for i := 0; i < 100; i++ {
		rnd := make([]byte, rand.Uint32()%1024)
		io.ReadFull(crand.Reader, rnd)
		session.conn.Write(rnd)
	}
	cli.Close()

	// double syn
	cli, err = net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ = Client(cli, nil)
	for i := 0; i < 100; i++ {
		f := newFrame(cmdSYN, 1000)
		session.writeFrame(f)
	}
	cli.Close()

	// random cmds
	cli, err = net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	allcmds := []byte{cmdSYN, cmdFIN, cmdPSH, cmdNOP, cmdACK, cmdFUL, cmdEMP}
	session, _ = Client(cli, nil)
	for i := 0; i < 100; i++ {
		f := newFrame(allcmds[rand.Int()%len(allcmds)], rand.Uint32())
		session.writeFrame(f)
	}
	cli.Close()

	// random cmds & sids
	cli, err = net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ = Client(cli, nil)
	for i := 0; i < 100; i++ {
		f := newFrame(byte(rand.Uint32()), rand.Uint32())
		session.writeFrame(f)
	}
	cli.Close()

	// random version
	cli, err = net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ = Client(cli, nil)
	for i := 0; i < 100; i++ {
		f := newFrame(byte(rand.Uint32()), rand.Uint32())
		f.ver = byte(rand.Uint32())
		session.writeFrame(f)
	}
	cli.Close()

	// incorrect size
	cli, err = net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ = Client(cli, nil)

	f := newFrame(byte(rand.Uint32()), rand.Uint32())
	rnd := make([]byte, rand.Uint32()%1024)
	io.ReadFull(crand.Reader, rnd)
	f.data = rnd

	buf := make([]byte, headerSize+len(f.data))
	buf[0] = f.ver
	buf[1] = f.cmd
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(rnd)+1)) /// incorrect size
	binary.LittleEndian.PutUint32(buf[4:], f.sid)
	copy(buf[headerSize:], f.data)

	session.conn.Write(buf)
	t.Log(rawHeader(buf))
	cli.Close()
}

func TestReadDeadline(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	const N = 100
	buf := make([]byte, 10)
	var readErr error
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		stream.Write([]byte(msg))
		stream.SetReadDeadline(time.Now().Add(-1 * time.Minute))
		if _, readErr = stream.Read(buf); readErr != nil {
			break
		}
	}
	if readErr != nil {
		if !strings.Contains(readErr.Error(), "i/o timeout") {
			t.Fatalf("Wrong error: %v", readErr)
		}
	} else {
		t.Fatal("No error when reading with past deadline")
	}
	session.Close()
}

func TestWriteDeadline(t *testing.T) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := Client(cli, nil)
	stream, _ := session.OpenStream()
	buf := make([]byte, 10)
	var writeErr error
	for {
		stream.SetWriteDeadline(time.Now().Add(-1 * time.Minute))
		if _, writeErr = stream.Write(buf); writeErr != nil {
			if !strings.Contains(writeErr.Error(), "i/o timeout") {
				t.Fatalf("Wrong error: %v", writeErr)
			}
			break
		}
	}
	session.Close()
}

func TestSlowReadBlocking(t *testing.T) {

	runtime.GOMAXPROCS(runtime.NumCPU() + 2)
	config := &Config{
		KeepAliveInterval:  10 * time.Second,
		KeepAliveTimeout:   30 * time.Second,
		MaxFrameSize:       4096,
		MaxReceiveBuffer:   1 * 1024 * 1024,
		EnableStreamBuffer: true,
		MaxStreamBuffer:    16384,
		BoostTimeout:       100 * time.Millisecond,
		WriteRequestQueueSize: 1024,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:39999")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}
			go func (conn net.Conn) {
				session, _ := Server(conn, config)
				for {
					if stream, err := session.AcceptStream(); err == nil {
						go func(s io.ReadWriteCloser) {
							buf := make([]byte, 1024 * 1024, 1024 * 1024)
							for {
								n, err := s.Read(buf)
								if err != nil {
									return
								}
//								t.Log("s1", stream.id, "session.bucket", atomic.LoadInt32(&session.bucket), "stream.bucket", atomic.LoadInt32(&stream.bucket), n)
								s.Write(buf[:n])
//								t.Log("s2", stream.id, "session.bucket", atomic.LoadInt32(&session.bucket), "stream.bucket", atomic.LoadInt32(&stream.bucket), n)
							}
						}(stream)
					} else {
						return
					}
				}
			}(conn)
		}
	}()

	cli, err := net.Dial("tcp", "127.0.0.1:39999")
	if err != nil {
		// handle error
		panic(err)
	}
	defer cli.Close()


	session, _ := Client(cli, config)
	startNotify := make(chan bool, 1)
	flag := int32(1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // fast write
		defer wg.Done()

		stream, err := session.OpenStream()
		if err == nil {
			t.Log("fast write stream start...")
			defer func() {
				stream.Close()
				t.Log("fast write stream end...")
			}()

			const SIZE = 4 * 1024 // 4KB

			var fwg sync.WaitGroup
			fwg.Add(1)
			go func() { // read = 4 * 2000 KB/s
				defer fwg.Done()
				rbuf := make([]byte, SIZE, SIZE)
				for atomic.LoadInt32(&flag) > 0 {
					stream.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					if _, err := stream.Read(rbuf); err != nil {
						if strings.Contains(err.Error(), "i/o timeout") {
							//t.Logf("read block too long: %v", err)
							continue
						}
						break
					}
					<- time.After(500 * time.Microsecond) // slow down read
				}
			}()

			buf := make([]byte, SIZE, SIZE)
			for i := range buf {
				buf[i] = byte('-')
			}
			startNotify <- true
			for atomic.LoadInt32(&flag) > 0 { // write = 4 * 10000 KB/s
				stream.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
				_, err := stream.Write(buf)
				if err != nil {
					if strings.Contains(err.Error(), "i/o timeout") {
						//t.Logf("write block too long: %v", err)
						continue
					}
					break
				}
				<- time.After(100 * time.Microsecond) // slow down write
//				t.Log("f2", stream.id, "session.bucket", atomic.LoadInt32(&session.bucket), "stream.bucket", atomic.LoadInt32(&stream.bucket))
			}
			fwg.Wait()

		} else {
			t.Fatal(err)
		}
	}()

	wg.Add(1)
	go func() { // normal write
		defer func() {
			session.Close()
			wg.Done()
		}()

		stream, err := session.OpenStream()
		if err == nil {
			t.Log("normal stream start...")
			defer func() {
				atomic.StoreInt32(&flag, int32(0))
				stream.Close()
				t.Log("normal stream end...")
			}()

			const N = 25
			buf := make([]byte, 12)
			<- startNotify
			for i := 0; i < N; i++ {
				msg := fmt.Sprintf("hello%v", i)
				start := time.Now()

				stream.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
				_, err := stream.Write([]byte(msg))
				if err != nil && strings.Contains(err.Error(), "i/o timeout") {
					t.Log(stream.id, i, err, "session.bucket", atomic.LoadInt32(&session.bucket), "stream.bucket", atomic.LoadInt32(&stream.bucket))
					return
				}

				stream.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				if n, err := stream.Read(buf); err != nil {
					t.Fatal(stream.id, i, err, "since start", time.Since(start))
					return
				} else if string(buf[:n]) != msg {
					t.Fatal(err)
				} else {
//					t.Log(stream.id, "session.bucket", atomic.LoadInt32(&session.bucket), "stream.bucket", atomic.LoadInt32(&stream.bucket))
					t.Log(stream.id, i, "time for normal stream rtt", time.Since(start))
				}
				<- time.After(200 * time.Millisecond)
			}
		} else {
			t.Fatal(err)
		}
	}()
	wg.Wait()
}

func TestHeadOfLineBlocking(t *testing.T) {

	kB := 1024
	mB := kB * 1024

	runtime.GOMAXPROCS(runtime.NumCPU() + 2)
	config := &Config{
		KeepAliveInterval:  1 * time.Second,
		KeepAliveTimeout:   30 * time.Second,
		MaxFrameSize:       1024,
		MaxReceiveBuffer:   16 * mB,
		EnableStreamBuffer: true,
		MaxStreamBuffer:    16 * kB,
		BoostTimeout:       100 * time.Millisecond,
		WriteRequestQueueSize: 10240,
		Test: true,
	}

	spdlim := 20 * kB
/*	small := spdlim
	mid := 10 * spdlim
	big := 50 * spdlim
*/
	small := spdlim / 10
	mid := spdlim
	big := 10 * spdlim

	allsize := []int{ big, small, small, small, mid, small, small, small, small, small}

	ln, err := net.Listen("tcp", "127.0.0.1:39999")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}

			srvlim := NewFlowLimit(conn)
			srvlim.SetRxSpd(spdlim)
			srvlim.SetTxSpd(spdlim)

			go func (conn net.Conn) {
				session, _ := Server(conn, config)
				i := 0
				for {
					if stream, err := session.AcceptStream(); err == nil {
						size := allsize[i%len(allsize)]
						i++
						go func(s io.ReadWriteCloser, dwsize int) {
							defer s.Close()
/*							buf := make([]byte, 1024, 1024)
							total := dwsize
							for {
								n := rand.Int()%1023 + 1
								if n > total {
									n = total
								}
								io.ReadFull(crand.Reader, buf[:n])
								s.Write(buf[:n])
								total -= n
								if n == 0 {
									break
								}
							}*/
							buf := make([]byte, dwsize, dwsize)
							io.ReadFull(crand.Reader, buf)
							s.Write(buf)
						}(stream, size)
					} else {
						return
					}
				}
			}(srvlim)
		}
	}()


	cli, err := net.Dial("tcp", "127.0.0.1:39999")
	if err != nil {
		// handle error
		panic(err)
	}
	defer cli.Close()

	session, _ := Client(cli, config)
	startNotify := make(chan bool)
	var wg sync.WaitGroup
	dwstatus := make([][]time.Time, len(allsize))

	allstart := time.Now()
	for wid := 0; wid<len(allsize); wid++ {
		wrec := make([]time.Time, 0, 4096)
		dwstatus[wid] = wrec

		wg.Add(1)
		go func(wid int, allstart time.Time) {
			defer wg.Done()

			stream, err := session.OpenStream()
			if err == nil {
				t.Log(wid, "stream start...", time.Now())
				defer func() {
					stream.Close()
//					t.Log(wid, "stream end...", time.Now())
				}()

				N := 100*mB
				buf := make([]byte, 1024)
				total := 0
				<- startNotify
				start := time.Now()
				ttfb := start
				dwstatus[wid] = append(dwstatus[wid], start)
				for i := 0; i < N; i++ {
//					stream.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
					n, err := stream.Read(buf)
					dwstatus[wid] = append(dwstatus[wid], time.Now())
					if i == 0 {
						ttfb = time.Now()
					}
					total += n
					if err == io.EOF {
						// total, init >> ttfb, init end >> ttfb, ttfb >> end
						t.Log(wid, stream.id, i, "stream end", time.Since(allstart), ttfb.Sub(allstart), ttfb.Sub(start), time.Since(ttfb), total)
						return
					} else if err != nil {
						t.Fatal(wid, stream.id, i, err, "stream err", time.Since(allstart), ttfb.Sub(allstart), ttfb.Sub(start), time.Since(ttfb), total)
						return
					}
//					runtime.Gosched()
				}
			} else {
				t.Fatal(err)
			}
		}(wid, allstart)
		<- time.After(5 * time.Millisecond)
	}
	close(startNotify)
	wg.Wait()
	session.Close()

	pktPerSec := float64(spdlim / config.MaxFrameSize)
	timeout := time.Duration(float64(time.Second) / pktPerSec)
	t.Log("[HoL test pktPerSec]", pktPerSec)
	t.Log("[HoL test Parallel]", len(allsize))
	t.Log("[HoL test timeout]", timeout)
	for wid, rec := range dwstatus {
		last := allstart
		count := 0
		out := ""
		for idx, t := range rec {
			delta := t.Sub(last)
			if delta >= timeout {
				out += fmt.Sprintf("%v:%v\t", idx, delta)
				count++
			}
			last = t
		}
		t.Log(wid, allsize[wid], count, len(rec), out)
	}

}

func TestHeadOfLineBlocking2(t *testing.T) {

	kB := 1024
	mB := kB * 1024

	runtime.GOMAXPROCS(runtime.NumCPU() + 2)
	config := &Config{
		KeepAliveInterval:  1 * time.Second,
		KeepAliveTimeout:   30 * time.Second,
		MaxFrameSize:       1024,
		MaxReceiveBuffer:   16 * mB,
		EnableStreamBuffer: false,
		MaxStreamBuffer:    16 * kB,
		BoostTimeout:       100 * time.Millisecond,
		WriteRequestQueueSize: 10240,
	}

	spdlim := 20 * kB
/*	small := spdlim
	mid := 10 * spdlim
	big := 50 * spdlim
*/
	small := spdlim / 10
	mid := spdlim
	big := 10 * spdlim

	allsize := []int{ big, small, small, small, mid, small, small, small, small, small}

	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	srvlim := NewFlowLimit(srv)
	srvlim.SetRxSpd(spdlim)
	srvlim.SetTxSpd(spdlim)

	go func (conn net.Conn) {
		session, _ := Server(conn, config)
		i := 0
		for {
			if stream, err := session.AcceptStream(); err == nil {
				size := allsize[i%len(allsize)]
				i++
				go func(s io.ReadWriteCloser, dwsize int) {
					defer s.Close()
/*					buf := make([]byte, 1024, 1024)
					total := dwsize
					for {
						n := rand.Int()%1023 + 1
						if n > total {
							n = total
						}
						io.ReadFull(crand.Reader, buf[:n])
						s.Write(buf[:n])
						total -= n
						if n == 0 {
							break
						}
					}*/
					buf := make([]byte, dwsize, dwsize)
					io.ReadFull(crand.Reader, buf)
					s.Write(buf)
				}(stream, size)
			} else {
				return
			}
		}
	}(srvlim)


	session, _ := Client(cli, config)
	startNotify := make(chan bool)
	var wg sync.WaitGroup
	dwstatus := make([][]time.Time, len(allsize))

	allstart := time.Now()
	for wid := 0; wid<len(allsize); wid++ {
		wrec := make([]time.Time, 0, 4096)
		dwstatus[wid] = wrec

		wg.Add(1)
		go func(wid int, allstart time.Time) {
			defer wg.Done()

			stream, err := session.OpenStream()
			if err == nil {
				t.Log(wid, "stream start...", time.Now())
				defer func() {
					stream.Close()
//					t.Log(wid, "stream end...", time.Now())
				}()

				N := 100*mB
				buf := make([]byte, 1024)
				total := 0
				<- startNotify
				start := time.Now()
				ttfb := start
				dwstatus[wid] = append(dwstatus[wid], start)
				for i := 0; i < N; i++ {
//					stream.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
					n, err := stream.Read(buf)
					dwstatus[wid] = append(dwstatus[wid], time.Now())
					if i == 0 {
						ttfb = time.Now()
					}
					total += n
					if err == io.EOF {
						// total, init >> ttfb, init end >> ttfb, ttfb >> end
						t.Log(wid, stream.id, i, "stream end", time.Since(allstart), ttfb.Sub(allstart), ttfb.Sub(start), time.Since(ttfb), total)
						return
					} else if err != nil {
						t.Fatal(wid, stream.id, i, err, "stream err", time.Since(allstart), ttfb.Sub(allstart), ttfb.Sub(start), time.Since(ttfb), total)
						return
					}
//					runtime.Gosched()
				}
			} else {
				t.Fatal(err)
			}
		}(wid, allstart)
		<- time.After(5 * time.Millisecond)
	}
	close(startNotify)
	wg.Wait()
	session.Close()

	pktPerSec := float64(spdlim / config.MaxFrameSize)
	timeout := time.Duration(float64(time.Second) / pktPerSec)
	t.Log("[HoL test pktPerSec]", pktPerSec)
	t.Log("[HoL test Parallel]", len(allsize))
	t.Log("[HoL test timeout]", timeout)
	for wid, rec := range dwstatus {
		last := allstart
		count := 0
		out := ""
		for idx, t := range rec {
			delta := t.Sub(last)
			if delta >= timeout {
				out += fmt.Sprintf("%v:%v\t", idx, delta)
				count++
			}
			last = t
		}
		t.Log(wid, allsize[wid], count, len(rec), out)
	}

}

func BenchmarkAcceptClose(b *testing.B) {
	cli, err := net.Dial("tcp", "127.0.0.1:19999")
	if err != nil {
		b.Fatal(err)
	}
	session, _ := Client(cli, nil)
	for i := 0; i < b.N; i++ {
		if stream, err := session.OpenStream(); err == nil {
			stream.Close()
		} else {
			b.Fatal(err)
		}
	}
}
func BenchmarkConnSmux(b *testing.B) {
	cs, ss, err := getSmuxStreamPair()
	if err != nil {
		b.Fatal(err)
	}
	defer cs.Close()
	defer ss.Close()
	bench(b, cs, ss)
}

func BenchmarkConnTCP(b *testing.B) {
	cs, ss, err := getTCPConnectionPair()
	if err != nil {
		b.Fatal(err)
	}
	defer cs.Close()
	defer ss.Close()
	bench(b, cs, ss)
}

func getSmuxStreamPair() (*Stream, *Stream, error) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		return nil, nil, err
	}

	s, err := Server(c2, nil)
	if err != nil {
		return nil, nil, err
	}
	c, err := Client(c1, nil)
	if err != nil {
		return nil, nil, err
	}
	var ss *Stream
	done := make(chan error)
	go func() {
		var rerr error
		ss, rerr = s.AcceptStream()
		done <- rerr
		close(done)
	}()
	cs, err := c.OpenStream()
	if err != nil {
		return nil, nil, err
	}
	err = <-done
	if err != nil {
		return nil, nil, err
	}

	return cs, ss, nil
}

func getTCPConnectionPair() (net.Conn, net.Conn, error) {
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}

	var conn0 net.Conn
	var err0 error
	done := make(chan struct{})
	go func() {
		conn0, err0 = lst.Accept()
		close(done)
	}()

	conn1, err := net.Dial("tcp", lst.Addr().String())
	if err != nil {
		return nil, nil, err
	}

	<-done
	if err0 != nil {
		return nil, nil, err0
	}
	return conn0, conn1, nil
}

func bench(b *testing.B, rd io.Reader, wr io.Writer) {
	buf := make([]byte, 128*1024)
	buf2 := make([]byte, 128*1024)
	b.SetBytes(128 * 1024)
	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for {
			n, _ := rd.Read(buf2)
			count += n
			if count == 128*1024*b.N {
				return
			}
		}
	}()
	for i := 0; i < b.N; i++ {
		wr.Write(buf)
	}
	wg.Wait()
}


type FlowLimit struct {
	In           net.Conn
	Tx           int64
	Rx           int64

	die          chan struct{}
	dieLock      sync.Mutex


	rxLim        float64
	rx0          int64
	rxt          time.Time

	txLim        float64
	tx0          int64
	txt          time.Time
}

func (c *FlowLimit) Close() error {
	c.dieLock.Lock()

	select {
	case <-c.die:
		c.dieLock.Unlock()
		return nil
	default:
	}

	close(c.die)
	return c.In.Close()
}

func (c *FlowLimit) Read(data []byte) (n int, err error)  {
	n, err = c.In.Read(data)
	curr := atomic.AddInt64(&c.Rx, int64(n))

	if c.rxLim <= 0 {
		return
	}

	now := time.Now()
	emsRx := int64(c.rxLim * now.Sub(c.rxt).Seconds()) + c.rx0
	if curr > emsRx {
		over := curr - emsRx
		sleep := float64(over) / c.rxLim
		sleepT := time.Duration(sleep * 1000000000) * time.Nanosecond
//log.Println("[Rx over]", curr, emsRx, over, sleepT)
		select {
		case <-c.die:
			return n, err
		case <-time.After(sleepT):
		}
	} else {
		c.rxt = now
		c.rx0 = curr
	}

	return n, err
}

func (c *FlowLimit) Write(data []byte) (n int, err error) {
	n, err = c.In.Write(data)
	curr := atomic.AddInt64(&c.Tx, int64(n))

	if c.txLim <= 0 {
		return
	}

	now := time.Now()
	emsTx := int64(c.txLim * now.Sub(c.txt).Seconds()) + c.tx0
	if curr > emsTx {
		over := curr - emsTx
		sleep := float64(over) / c.txLim
		sleepT := time.Duration(sleep * 1000000000) * time.Nanosecond
//log.Println("[Tx over]", curr, emsTx, over, sleepT)
		select {
		case <-c.die:
			return n, err
		case <-time.After(sleepT):
		}
	} else {
		c.txt = now
		c.tx0 = curr
	}

	return n, err
}

// LocalAddr satisfies net.Conn interface
func (c *FlowLimit) LocalAddr() net.Addr {
	if ts, ok := c.In.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (c *FlowLimit) RemoteAddr() net.Addr {
	if ts, ok := c.In.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

func (c *FlowLimit) SetReadDeadline(t time.Time) error {
	return c.In.SetReadDeadline(t)
}

func (c *FlowLimit) SetWriteDeadline(t time.Time) error {
	return c.In.SetWriteDeadline(t)
}

func (c *FlowLimit) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

func NewFlowLimit(con net.Conn) (c *FlowLimit) {
	c = &FlowLimit{}
	c.die = make(chan struct{})
	c.In = con

	return c
}

// Bytes / sec
func (c *FlowLimit) SetRxSpd(spd int) {
	now := time.Now()
	c.rxt = now
	c.rx0 = c.Rx
	c.rxLim = float64(spd)
}

func (c *FlowLimit) SetTxSpd(spd int) {
	now := time.Now()
	c.txt = now
	c.tx0 = c.Tx
	c.txLim = float64(spd)
}


