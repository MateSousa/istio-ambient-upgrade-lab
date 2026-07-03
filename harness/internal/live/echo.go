package live

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"time"
)

// EchoConfig configures the minimal TCP echo server the probe dials.
type EchoConfig struct {
	Addr string // listen address, e.g. ":9000"
}

// EchoConfigFromEnv reads the echo listen address from ECHO_LISTEN (default
// ":9000").
func EchoConfigFromEnv() EchoConfig {
	addr := os.Getenv("ECHO_LISTEN")
	if addr == "" {
		addr = ":9000"
	}
	return EchoConfig{Addr: addr}
}

// RunEcho starts a minimal TCP echo server: it accepts connections and writes
// back whatever it reads. It is the same-node peer the probe holds a long-lived
// connection to, so exactly one ztunnel mediates the flow. It blocks until the
// listener errors (i.e. process shutdown).
func RunEcho(cfg EchoConfig) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	log.Printf("echo: listening on %s", cfg.Addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go echoConn(conn)
	}
}

func echoConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		// A generous read deadline so a silent-but-open keepalive connection is
		// not reaped by the echo side; the probe trickles well within this.
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}
