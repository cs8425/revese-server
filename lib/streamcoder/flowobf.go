package streamcoder

import (
	"encoding/binary"
	"math/rand"
	"sync"
	"time"
	"io"
	"net"

//	"log"
)

type FlowObf struct {
	In           net.Conn //io.ReadWriteCloser
	DataLen      int
	NoiseMinLen  int
	NoiseMaxLen  int
	rbuf         []byte
	rbufLock     sync.Mutex
	wbuf         sync.Pool
	rem          []byte
}

type Frame struct {
	d []byte
	l int
	rl int
}

func (c *FlowObf) Close() error {
	if err := c.In.Close(); err != nil {
		return err
	}
	return nil
}

func (c *FlowObf) Read(data []byte) (n int, err error)  {
	dlen := len(data)

	n = copy(data, c.rem)
	if n == len(c.rem) {
		c.rem = make([]byte, 0, c.DataLen)
	} else {
		c.rem = c.rem[n:]
	}
	if n == dlen {
		return n, nil
	}

	f, err := c.readFrame(c.rbuf)
	c.rem = append(c.rem, f.d[:f.l]...)
	n2 := copy(data[n:], c.rem)
	if n2 == len(c.rem) {
		c.rem = make([]byte, 0, c.DataLen)
	} else {
		c.rem = c.rem[n2:]
	}
	return n + n2, err
}

func (c *FlowObf) Write(data []byte) (n int, err error) {
	frames := c.split(data)
	sent := 0
	rawsnd := 0
	for k := range frames {
		f := frames[k]
		n, err := c.In.Write(f.d[:f.l])
		c.wbuf.Put(f)
		sent += n
		rawsnd += f.rl
		if err != nil {
			return rawsnd, err
		}
	}

	return rawsnd, nil
}

// LocalAddr satisfies net.Conn interface
func (c *FlowObf) LocalAddr() net.Addr {
	if ts, ok := c.In.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (c *FlowObf) RemoteAddr() net.Addr {
	if ts, ok := c.In.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

func (c *FlowObf) SetReadDeadline(t time.Time) error {
	return c.In.SetReadDeadline(t)
//	return nil
}

func (c *FlowObf) SetWriteDeadline(t time.Time) error {
	return c.In.SetWriteDeadline(t)
//	return nil
}

func (c *FlowObf) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// noise = 0 ~ 65535
// data = 0 ~ 65535
func NewFlowObf(con net.Conn, maxData int, maxNoise int) (c *FlowObf, err error) {
	c = &FlowObf{}
	c.In = con
	c.DataLen = maxData
	c.NoiseMaxLen = maxNoise
	c.rbuf = make([]byte, maxData + maxNoise + 4, maxData + maxNoise + 4)

	c.wbuf.New = func() interface{} {
		return Frame{
			d: make([]byte, maxData + maxNoise + 4, maxData + maxNoise + 4),
			l: 0,
		}
	}

	return c, nil
}

func init() () {
	rand.Seed(int64(time.Now().Nanosecond()))
}

func (c *FlowObf) readFrame(b []byte) (f Frame, err error) {
	if _, err := io.ReadFull(c.In, b[:4]); err != nil {
		return f, err
	}

	noiseLen := binary.LittleEndian.Uint16(b[0:2])
	dataLen := binary.LittleEndian.Uint16(b[2:4])

/*	if noiseLen > 0 || dataLen > 0 {
		log.Println("readFrame", noiseLen, dataLen)
	}*/

	if noiseLen > 0 {
		if n, err := io.ReadFull(c.In, b[4:4+noiseLen]); err != nil {
			f.d = b[4:4+n]
			f.l = n
			return f, err
		}
	}
	if dataLen > 0 {
		if n, err := io.ReadFull(c.In, b[4+noiseLen:4+noiseLen+dataLen]); err != nil {
			f.d = b[4+noiseLen:4+int(noiseLen)+n]
			f.l = n
			return f, err
		}
		f.d = b[4+noiseLen : 4+noiseLen+dataLen]
		f.l = int(dataLen)
		return f, nil
	}
	return f, nil
}

func (c *FlowObf) split(bts []byte) ([]Frame) {
	frames := make([]Frame, 0, len(bts)/c.DataLen+1)
	for len(bts) > c.DataLen {
		buf := c.mkFrame(bts[:c.DataLen], c.DataLen)
		frames = append(frames, buf)
		bts = bts[c.DataLen:]
	}
	if len(bts) > 0 {
		n := len(bts)
		buf := c.mkFrame(bts[:n], n)
		frames = append(frames, buf)
	}
	return frames
}

func (c *FlowObf) mkNoise(buf []byte, dn int) (n int) {
	if dn > c.NoiseMaxLen {
		n = rand.Intn(5)
	} else {
		n = rand.Intn(c.NoiseMaxLen)
	}
	binary.LittleEndian.PutUint16(buf[0:2], uint16(n))
	rand.Read(buf[4:4+n])
	return
}

func (c *FlowObf) mkFrame(bts []byte, lts int) (Frame) {
	buf := c.wbuf.Get().(Frame)
	n := c.mkNoise(buf.d, len(bts))
	binary.LittleEndian.PutUint16(buf.d[2:4], uint16(lts))
	copy(buf.d[4+n:], bts[:lts])
	buf.l = 4 + n + lts
	buf.rl = lts
	return buf
}


