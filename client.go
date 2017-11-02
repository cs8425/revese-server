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
	"errors"
	"runtime"

	"./lib/smux"
	"./lib/streamcoder"
)

var conf = flag.String("c", "client.json", "config file")

var (
	// VERSION is injected by buildflags
	VERSION = "SELFBUILD"
)

var verbosity int = 2

// global recycle buffer
var copyBuf sync.Pool

// Config for server
type Config struct {
	TargetAddr   string `json:"targetaddr"`
	HubAddr      string `json:"hubaddr"`

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

func handleClient(p1 net.Conn, config *Config) {
	defer p1.Close()

	p2, err := net.DialTimeout("tcp", config.TargetAddr, 10*time.Second)
	if err != nil {
		Vln(2, "[connect err]", config.TargetAddr, err)
		return
	}
	defer p2.Close()
	Vln(3, "new client:", p1.RemoteAddr(), p2.LocalAddr(), p2.RemoteAddr())
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


	Vln(2, "Target address:", config.TargetAddr)
	Vln(2, "Hub address:", config.HubAddr)
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

	createConn := func() (*smux.Session, error) {

		var conn net.Conn
		var err error

		conn, err = net.Dial("tcp", config.HubAddr)
		if err != nil {
			return nil, errors.New("createConn():" + err.Error())
		}

		// stream multiplex
		var session *smux.Session
		if config.Key != "" {
			var cnonce [12]byte
			var snonce [12]byte
			crand.Read(cnonce[:])
			conn.Write(cnonce[:])
			n, err := conn.Read(snonce[:])
			if n != 12 {
				Vln(2, "createConn(): get nonce size = ", n)
				conn.Close()
				return nil, errors.New("createConn(): get nonce size != 12")
			}
			if err != nil {
				conn.Close()
				return nil, errors.New("createConn(): get nonce:" + err.Error())
			}
			key, nonce := streamcoder.NewKeyNonce([]byte(config.Key), cnonce[:], snonce[:])
			//Vln(4, "createConn():nonce:", len(key), len(nonce), key, nonce)
			enccon, err := streamcoder.NewCoder(conn, key, nonce, false)
			if err != nil {
				conn.Close()
				return nil, errors.New("createConn(): NewCoder:" + err.Error())
			}

			session, err = smux.Server(enccon, smuxConfig)
		} else {
			session, err = smux.Server(conn, smuxConfig)
		}
		if err != nil {
			conn.Close()
			return nil, errors.New("createConn():" + err.Error())
		}
		Vln(2, "connect to:", conn.RemoteAddr())

		return session, nil
	}

	// wait until a connection is ready
	waitConn := func() *smux.Session {
		for {
			if session, err := createConn(); err == nil {
				return session
			} else {
				time.Sleep(time.Second)
			}
		}
	}

	sess := waitConn()
	for {
		p1, err := sess.AcceptStream()
		if err != nil {
			Vln(3, "AcceptStream err:", err)
			if sess.IsClosed() {
				sess = waitConn()
			}
			continue
		}

		go handleClient(p1, &config)
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

