package main

import (
	crand "crypto/rand"
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"
	"runtime"

	"./lib/smux"
	"./lib/streamcoder"
)

var conf = flag.String("c", "hub.json", "config file")

var (
	// VERSION is injected by buildflags
	VERSION = "SELFBUILD"
)

var verbosity int = 2

// global recycle buffer
var copyBuf sync.Pool

// Config for server
type Config struct {
	LocalAddr    string `json:"localaddr"`
	AdminAddr    string `json:"adminaddr"`

	MuxBuf       int    `json:"muxbuf"`
	MuxStreamBuf int    `json:"streambuf"`
	StreamBufEn  bool   `json:"streambuf-en"`
	BoostTimeout int    `json:"boosttimeout"`
	PipeBuf      int    `json:"pipebuf"`
	RWBuf        int    `json:"rwbuf"`
	MaxFrameSize int    `json:"maxframe"`

	Key          string `json:"key"`
	Log          string `json:"log"`
	Verb         int    `json:"verb"`
}

func parseJSONConfig(config *Config, path string) error {
	file, err := os.Open(path) // For read access.
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(config)
}

func setRWBuffer(p2 net.Conn, RWBuf int) {
	if RWBuf > 0 {
		if err := p2.(*net.TCPConn).SetReadBuffer(RWBuf); err != nil {
			Vln(3, "TCP SetReadBuffer:", err)
		}
		if err := p2.(*net.TCPConn).SetWriteBuffer(RWBuf); err != nil {
			Vln(3, "TCP SetWriteBuffer:", err)
		}
	}
}

func handleClient(sess *smux.Session, p1 net.Conn, config *Config) {
//	Vln(3, "stream opened")
//	defer Vln(3, "stream closed")
	defer p1.Close()

//	Vln(3, "new client:", config.Mode, p1.RemoteAddr(), p2.LocalAddr(), p2.RemoteAddr())

	p2, err := sess.OpenStream()
	if err != nil {
		return
	}
	defer p2.Close()
	setRWBuffer(p2, config.RWBuf)
	cp(p1, p2)
}

func cp(p1, p2 io.ReadWriteCloser) {
	// start tunnel
	p1die := make(chan struct{})
	go func() {
		buf := copyBuf.Get().([]byte)
		io.CopyBuffer(p1, p2, buf)
		close(p1die)
		copyBuf.Put(buf)
	}()

	p2die := make(chan struct{})
	go func() {
		buf := copyBuf.Get().([]byte)
		io.CopyBuffer(p2, p1, buf)
		close(p2die)
		copyBuf.Put(buf)
	}()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
}

func checkError(err error) {
	if err != nil {
		log.Printf("%+v\n", err)
		os.Exit(-1)
	}
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU() + 2)
	rand.Seed(int64(time.Now().Nanosecond()))
	if VERSION == "SELFBUILD" {
		// add more log flags for debugging
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	flag.Parse()

	config := Config{}
	err := parseJSONConfig(&config, *conf)
	checkError(err)

	// log redirect
	if config.Log != "" {
		f, err := os.OpenFile(config.Log, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		checkError(err)
		defer f.Close()
		log.SetOutput(f)
	}

	Vln(2, "version:", VERSION)

	if config.MuxBuf <= 0 {
		config.MuxBuf = 64 * 1024 * 1024
	}
	if config.MuxStreamBuf <= 0 || config.MuxStreamBuf > config.MuxBuf {
		config.MuxStreamBuf = 16 * 1024
	}
	if config.MaxFrameSize <= 0 || config.MaxFrameSize > 65535 {
		config.MaxFrameSize = 1400
	}
	if config.PipeBuf <= 0 {
		config.PipeBuf = 4096
	}
	if config.BoostTimeout <= 0{
		config.BoostTimeout = 10 * 1000
	}
	verbosity = config.Verb


	Vln(2, "listening on:", config.LocalAddr)
	Vln(2, "Admin address:", config.AdminAddr)
	Vln(2, "muxbuf:", config.MuxBuf)
	Vln(2, "muxstreambuf:", config.MuxStreamBuf)
	Vln(2, "muxstreambuf-en:", config.StreamBufEn)
	Vln(2, "boosttimeout:", config.BoostTimeout)
	Vln(2, "pipebuf:", config.PipeBuf)
	Vln(2, "rwbuf:", config.RWBuf)
	Vln(2, "maxframe:", config.MaxFrameSize)
	Vln(2, "verbosity:", config.Verb)

	cryptoState := "disable"
	if config.Key != "" {
		cryptoState = "enable"
	}
	Vln(2, "crypto:", cryptoState)

	copyBuf.New = func() interface{} {
		return make([]byte, config.PipeBuf)
	}

	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = config.MuxBuf
	smuxConfig.MaxFrameSize = config.MaxFrameSize
	smuxConfig.MaxStreamBuffer = config.MuxStreamBuf
	smuxConfig.EnableStreamBuffer = config.StreamBufEn
	smuxConfig.BoostTimeout = time.Duration(config.BoostTimeout) * time.Millisecond

	listener, err := net.Listen("tcp", config.LocalAddr)
	checkError(err)

	chAdmin := make(chan *smux.Session, 1)
	go func(addr string) {
		ln, err := net.Listen("tcp", addr)
		checkError(err)

		for {
			conn, err := ln.Accept()
			if err != nil {
				continue
			}

			Vln(4, "admin init start:", conn.RemoteAddr())

			// stream multiplex
			var session *smux.Session
			if config.Key != "" {
				var cnonce [12]byte
				var snonce [12]byte
				crand.Read(snonce[:])
				conn.Write(snonce[:])
				n, err := conn.Read(cnonce[:])
				if n != 12 {
					Vln(2, "createConn(): get nonce size = ", n)
					conn.Close()
					continue
				}
				if err != nil {
					conn.Close()
					continue
				}
				key, nonce := streamcoder.NewKeyNonce([]byte(config.Key), cnonce[:], snonce[:])
				//Vln(4, "createConn():nonce:", len(key), len(nonce), key, nonce)
				enccon, err := streamcoder.NewCoder(conn, key, nonce, true)
				if err != nil {
					conn.Close()
					continue
				}

				session, err = smux.Client(enccon, smuxConfig)
			} else {
				session, err = smux.Client(conn, smuxConfig)
			}
			if err != nil {
				Vln(2, "createConn(): smux.Client err", err)
				conn.Close()
				continue
			}

			Vln(2, "admin connect in:", conn.RemoteAddr())

			chAdmin <- session
		}
	}(config.AdminAddr)

	sess := <-chAdmin
	for {
		p1, err := listener.Accept()
		if err != nil {

			continue
		}

		Vln(3, "client connect in:", p1.RemoteAddr())
		if sess.IsClosed() {
			Vln(3, "wait new session...")
			sess = <-chAdmin
		}

		go handleClient(sess, p1, &config)
	}

}

func Vf(level int, format string, v ...interface{}) {
	if level <= verbosity {
		log.Printf(format, v...)
	}
}
func V(level int, v ...interface{}) {
	if level <= verbosity {
		log.Print(v...)
	}
}
func Vln(level int, v ...interface{}) {
	if level <= verbosity {
		log.Println(v...)
	}
}

