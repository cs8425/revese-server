package streamcoder

import (
	"sync"
	"sync/atomic"
	"time"
	"net"

//	"log"
)

type FlowStat struct {
	In           net.Conn
	Tx           int64
	Rx           int64
	TxSPD        int
	RxSPD        int

	start        time.Time
	end          time.Time
	die          chan struct{}
	dieLock      sync.Mutex


	rxLim        float64
	rx0          int64
	rxt          time.Time

	txLim        float64
	tx0          int64
	txt          time.Time
}

func (c *FlowStat) Close() error {
	c.dieLock.Lock()

	select {
	case <-c.die:
		c.dieLock.Unlock()
		return nil
	default:
	}

	close(c.die)
	c.end = time.Now()
	return c.In.Close()
}

func (c *FlowStat) Read(data []byte) (n int, err error)  {
	n, err = c.In.Read(data)
//	atomic.AddInt64(&c.Rx, int64(n))
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

func (c *FlowStat) Write(data []byte) (n int, err error) {
	n, err = c.In.Write(data)
//	atomic.AddInt64(&c.Tx, int64(n))
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
func (c *FlowStat) LocalAddr() net.Addr {
	if ts, ok := c.In.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (c *FlowStat) RemoteAddr() net.Addr {
	if ts, ok := c.In.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

func (c *FlowStat) SetReadDeadline(t time.Time) error {
	return c.In.SetReadDeadline(t)
}

func (c *FlowStat) SetWriteDeadline(t time.Time) error {
	return c.In.SetWriteDeadline(t)
}

func (c *FlowStat) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

func NewFlowStat(con net.Conn) (c *FlowStat, err error) {
	c = &FlowStat{}
	c.die = make(chan struct{})
	c.In = con


	now := time.Now()
	c.start = now
	c.rxt = now
	c.txt = now

//	c.rxLim = 1 * 1024 * 1024
//	c.txLim = 2.5 * 1024 * 1024

//	go c.calcSpd()

	return c, nil
}

func (c *FlowStat) calcSpd() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	var t0 = time.Now()
	var Tx0 int64 = c.Tx
	var Rx0 int64 = c.Rx

	for {
		select {
		case <-tick.C:
			dt := time.Now().Sub(t0).Seconds()
			c.TxSPD = int(float64(c.Tx - Tx0) / dt)
			c.RxSPD = int(float64(c.Rx - Rx0) / dt)

			Tx0 = c.Tx
			Rx0 = c.Rx
			t0 = time.Now()

//log.Println("[SPD]", c.TxSPD, c.RxSPD)
		case <-c.die:
			return
		}
	}
}

// kBytes / sec
func (c *FlowStat) SetRxSpd(spd int) {
	now := time.Now()
	c.rxt = now
	c.rx0 = c.Rx
	c.rxLim = float64(spd) * 1024
}

func (c *FlowStat) SetTxSpd(spd int) {
	now := time.Now()
	c.txt = now
	c.tx0 = c.Tx
	c.txLim = float64(spd) * 1024
}

func (c *FlowStat) Dt() (dt time.Duration) {
	select {
	case <-c.die:
		dt = c.end.Sub(c.start)
	default:
		dt = time.Now().Sub(c.start)
	}
	return dt
}

func (c *FlowStat) AvgRx() (n int64) {
	n = atomic.LoadInt64(&c.Rx)
	dt := c.Dt().Seconds()
	n = int64(float64(n) / dt)
	return
}

func (c *FlowStat) AvgTx() (n int64) {
	n = atomic.LoadInt64(&c.Tx)
	dt := c.Dt().Seconds()
	n = int64(float64(n) / dt)
	return
}


