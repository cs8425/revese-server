package streamcoder

import (
	"crypto/sha256"
	"crypto/sha512"
//	"io"
	"net"
	"time"
	chacha "../chacha20"
)

type Coder struct {
	In     net.Conn //io.ReadWriteCloser
	enc    *chacha.Cipher
	dec    *chacha.Cipher
	nonce  []byte
	dir    bool
}

func (c *Coder) Close() error {
	if err := c.In.Close(); err != nil {
		return err
	}
	return nil
}

func (c *Coder) Read(data []byte) (n int, err error)  {
    n, err = c.In.Read(data)
    if n > 0 {
		c.dec.XORKeyStream(data[0:n], data[0:n])
    }
    return n, err
}

func (c *Coder) Write(data []byte) (n int, err error) {
	c.enc.XORKeyStream(data[:], data[:])
	return c.In.Write(data)
}

// LocalAddr satisfies net.Conn interface
func (c *Coder) LocalAddr() net.Addr {
	if ts, ok := c.In.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (c *Coder) RemoteAddr() net.Addr {
	if ts, ok := c.In.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

func (c *Coder) SetReadDeadline(t time.Time) error {
	return c.In.SetReadDeadline(t)
}

func (c *Coder) SetWriteDeadline(t time.Time) error {
	return c.In.SetWriteDeadline(t)
}

func (c *Coder) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

func (c *Coder) ReKey(key []byte) (err error) {
	shah := sha512.New()
	shah.Write(key)
	key = shah.Sum([]byte(""))

	key = key[0:64]

	var encKey []byte
	var decKey []byte
	var encNonce []byte
	var decNonce []byte
	if c.dir {
		encKey = key[0:32]
		encNonce = c.nonce[0:12]
		decKey = key[32:64]
		decNonce = c.nonce[12:24]
	} else {
		encKey = key[32:64]
		encNonce = c.nonce[12:24]
		decKey = key[0:32]
		decNonce = c.nonce[0:12]
	}

	c.enc.ReKey(encKey, encNonce)
	if err != nil {
		return err
	}
	c.dec.ReKey(decKey, decNonce)
	if err != nil {
		return err
	}
	return nil
}

// key = 32 bytes x 2
// nonce = 12 bytes x 2
func NewCoder(con net.Conn, key []byte, nonce []byte, isClient bool) (c *Coder, err error) {
	c = &Coder{
			dir: isClient,
			nonce: nonce,
		}
	c.In = con

	key = key[0:64]
	nonce = nonce[0:24]

	var encKey []byte
	var decKey []byte
	var encNonce []byte
	var decNonce []byte
	if isClient {
		encKey = key[0:32]
		encNonce = nonce[0:12]
		decKey = key[32:64]
		decNonce = nonce[12:24]
	} else {
		encKey = key[32:64]
		encNonce = nonce[12:24]
		decKey = key[0:32]
		decNonce = nonce[0:12]
	}

	c.enc, err = chacha.NewCipher(encKey, encNonce)
	if err != nil {
		return nil, err
	}
	c.dec, err = chacha.NewCipher(decKey, decNonce)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func NewKeyNonce(key []byte, cnonce []byte, snonce []byte) (outKey, outNonce []byte) {
	shah := sha512.New()
	shah.Write(key)
	outKey = shah.Sum([]byte(""))

	shah = sha256.New()
	shah.Write(append(cnonce, snonce...))
	outNonce = shah.Sum([]byte(""))

	return
}


