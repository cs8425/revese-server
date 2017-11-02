package streamcoder

import (
	"time"
	"net"

	"log"
)

type FlowDump struct {
	In           net.Conn //io.ReadWriteCloser
	DataLen      int
}

func (c *FlowDump) Close() error {
	if err := c.In.Close(); err != nil {
		return err
	}
	return nil
}

func (c *FlowDump) Read(data []byte) (n int, err error)  {
	n, err = c.In.Read(data)
	log.Println("[d]Read", n, err, data)
	return n, err
}

func (c *FlowDump) Write(data []byte) (n int, err error) {
	n, err = c.In.Write(data)
	log.Println("[d]Write", n, err, data)
	return n, err
}

// LocalAddr satisfies net.Conn interface
func (c *FlowDump) LocalAddr() net.Addr {
	if ts, ok := c.In.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (c *FlowDump) RemoteAddr() net.Addr {
	if ts, ok := c.In.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

func (c *FlowDump) SetReadDeadline(t time.Time) error {
	return c.In.SetReadDeadline(t)
}

func (c *FlowDump) SetWriteDeadline(t time.Time) error {
	return c.In.SetWriteDeadline(t)
}

func (c *FlowDump) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

func NewFlowDump(con net.Conn, maxData int) (c *FlowDump, err error) {
	c = &FlowDump{}
	c.In = con
	c.DataLen = maxData

	return c, nil
}



