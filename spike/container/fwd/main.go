// Command fwd stands in for the daemon's per-execution 127.0.0.1 toolbelt
// port on macOS: it forwards a loopback TCP port to a unix socket.
package main

import (
	"io"
	"log"
	"net"
	"os"
)

func main() {
	l, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			u, err := net.Dial("unix", os.Args[2])
			if err != nil {
				c.Close()
				return
			}
			go func() { io.Copy(c, u); c.Close() }()
			io.Copy(u, c)
			u.Close()
		}()
	}
}
