package smux

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"errors"
)

// Stream implements net.Conn
type Stream struct {
	id            uint32
	rstflag       int32
	sess          *Session
	buffer        bytes.Buffer
	bufferLock    sync.Mutex
	frameSize     int
	chReadEvent   chan struct{} // notify a read event
	die           chan struct{} // flag the stream has closed
	dieLock       sync.Mutex
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	bucket         int32         // token bucket
	bucketNotify   chan struct{} // used for waiting for tokens
	fulflag        int32
	empflagLock    sync.Mutex
	empflag        int32
	countRead      int32         // for guess read speed
	boostTimeout   time.Time      // for initial boost
	guessBucket    int32         // for guess needed stream buffer size

	lastWrite      atomic.Value
	guessNeeded    int32
}

// newStream initiates a Stream struct
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.chReadEvent = make(chan struct{}, 1)
	s.frameSize = frameSize
	s.sess = sess
	s.die = make(chan struct{})

	s.bucket = int32(0)
	s.bucketNotify = make(chan struct{}, 1)
	s.empflag = int32(1)
	s.countRead = int32(0)
	s.boostTimeout = time.Now().Add(s.sess.BoostTimeout)
	s.guessBucket = int32(s.sess.MaxStreamBuffer)
	s.lastWrite.Store(time.Now())
	return s
}

// ID returns the unique stream ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(d.Sub(time.Now()))
		defer timer.Stop()
		deadline = timer.C
	}

READ:
	s.bufferLock.Lock()
	n, err = s.buffer.Read(b)
	rem := s.buffer.Len()
	s.bufferLock.Unlock()

	if n > 0 {
		s.sess.returnTokens(n)
		s.returnTokens(n)
		return n, nil
	} else if rem > 0 {
		return n, nil
	} else if atomic.LoadInt32(&s.rstflag) == 1 {
		_ = s.Close()
		return 0, io.EOF
	}

	select {
	case <-s.chReadEvent:
		goto READ
	case <-deadline:
		return n, errTimeout
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	}
}

// Write implements net.Conn
func (s *Stream) Write(b []byte) (n int, err error) {
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(d.Sub(time.Now()))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	default:
	}

	if atomic.LoadInt32(&s.fulflag) == 1 {
		select {
		case <-s.bucketNotify:
		case <-s.die:
			return 0, errors.New(errBrokenPipe)
		case <-deadline:
			return 0, errTimeout
		}
	}

	frames := s.split(b, cmdPSH, s.id)
	sent := 0
	for k := range frames {
		req := writeRequest{
			frame:  frames[k],
			result: make(chan writeResult, 1),
		}

		select {
		case s.sess.writes <- req:
		case <-s.die:
			return sent, errors.New(errBrokenPipe)
		case <-deadline:
			return sent, errTimeout
		}

		select {
		case result := <-req.result:
			sent += result.n
			if result.err != nil {
				return sent, result.err
			}
		case <-s.die:
			return sent, errors.New(errBrokenPipe)
		case <-deadline:
			return sent, errTimeout
		}

	}
	return sent, nil
}

// Close implements net.Conn
func (s *Stream) Close() error {
	s.dieLock.Lock()

	select {
	case <-s.die:
		s.dieLock.Unlock()
		if atomic.LoadInt32(&s.rstflag) == 1 {
			return nil
		}
		return errors.New(errBrokenPipe)
	default:
		close(s.die)
		s.dieLock.Unlock()
		s.sess.streamClosed(s.id)
		_, err := s.sess.writeFrame(newFrame(cmdFIN, s.id))
		return err
	}
}

// SetReadDeadline sets the read deadline as defined by
// net.Conn.SetReadDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline sets the write deadline as defined by
// net.Conn.SetWriteDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	return nil
}

// SetDeadline sets both read and write deadlines as defined by
// net.Conn.SetDeadline.
// A zero time value disables the deadlines.
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// session closes the stream
func (s *Stream) sessionClose() {
	s.dieLock.Lock()
	defer s.dieLock.Unlock()

	select {
	case <-s.die:
	default:
		close(s.die)
	}
}

// LocalAddr satisfies net.Conn interface
func (s *Stream) LocalAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Stream) RemoteAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// pushBytes a slice into buffer
func (s *Stream) pushBytes(p []byte) {
	s.bufferLock.Lock()
	s.buffer.Write(p)
	s.bufferLock.Unlock()

	if !s.sess.EnableStreamBuffer {
		return
	}

	n := len(p)
	used := atomic.AddInt32(&s.bucket, int32(n))
	lastReadOut := atomic.SwapInt32(&s.countRead, int32(0))	// reset read

	// hard limit
	if used > int32(s.sess.MaxStreamBuffer) {
		s.sendPause()
		return
	}

	if lastReadOut != 0 {
		s.lastWrite.Store(time.Now())
		needed := atomic.LoadInt32(&s.guessNeeded)
		s.guessBucket = int32(float32(s.guessBucket) * 0.8 + float32(needed) * 0.2)
	}

	if used <= s.guessBucket {
		s.boostTimeout = time.Now().Add(s.sess.BoostTimeout)
		return
	}

	if time.Now().After(s.boostTimeout) {
		s.sendPause()
	}
}

// recycleTokens transform remaining bytes to tokens(will truncate buffer)
func (s *Stream) recycleTokens() (n int) {
	s.bufferLock.Lock()
	n = s.buffer.Len()
	s.buffer.Reset()
	s.bufferLock.Unlock()
	return
}

// split large byte buffer into smaller frames, reference only
func (s *Stream) split(bts []byte, cmd byte, sid uint32) []Frame {
	frames := make([]Frame, 0, len(bts)/s.frameSize+1)
	for len(bts) > s.frameSize {
		frame := newFrame(cmd, sid)
		frame.data = bts[:s.frameSize]
		bts = bts[s.frameSize:]
		frames = append(frames, frame)
	}
	if len(bts) > 0 {
		frame := newFrame(cmd, sid)
		frame.data = bts
		frames = append(frames, frame)
	}
	return frames
}

// notify read event
func (s *Stream) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

// mark this stream has been reset
func (s *Stream) markRST() {
	atomic.StoreInt32(&s.rstflag, 1)
}

// mark this stream has been pause write
func (s *Stream) pauseWrite() {
	atomic.StoreInt32(&s.fulflag, 1)
}

// mark this stream has been resume write
func (s *Stream) resumeWrite() {
	if atomic.LoadInt32(&s.fulflag) == 1 {
		select {
		case s.bucketNotify <- struct{}{}:
		default:
		}
	}
	atomic.StoreInt32(&s.fulflag, 0)
}

// returnTokens is called by stream to return token after read
func (s *Stream) returnTokens(n int) {
	if !s.sess.EnableStreamBuffer {
		return
	}

	used := atomic.AddInt32(&s.bucket, -int32(n))
	totalRead := atomic.AddInt32(&s.countRead, int32(n))
	lastWrite, _ := s.lastWrite.Load().(time.Time)
	dt := time.Now().Sub(lastWrite) + 1
	needed := totalRead * int32(s.sess.rtt / dt)
	atomic.StoreInt32(&s.guessNeeded, needed)
	if used <= 0 || (needed > 0 && needed >= used) {
		s.sendResume()
	}
}

// send cmdFUL to pause write
func (s *Stream) sendPause() {
	s.empflagLock.Lock()
	if s.empflag == 1 {
		s.empflag = 0
		s.empflagLock.Unlock()
		s.sess.writeFrame(newFrame(cmdFUL, s.id))
		return
	}
	s.empflagLock.Unlock()
}

// send cmdEMP to resume write
func (s *Stream) sendResume() {
	s.empflagLock.Lock()
	if s.empflag == 0 {
		s.empflag = 1
		s.empflagLock.Unlock()
		s.sess.writeFrame(newFrame(cmdEMP, s.id))
		return
	}
	s.empflagLock.Unlock()
}

var errTimeout error = &timeoutError{}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
