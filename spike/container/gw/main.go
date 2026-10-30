// Command gw is the spike's stand-in gateway: a unix-socket toolbelt forward
// and an allowlisting HTTP CONNECT proxy. Evidence only, not for merging.
package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	listen := os.Getenv("GW_SOCKET")   // e.g. /shared/toolbelt.sock
	target := os.Getenv("GW_TARGET")   // tcp:host.docker.internal:7777 or unix:/run/oc/toolbelt.sock
	allow := strings.Split(os.Getenv("GW_ALLOW"), ",")
	if listen != "" {
		os.Remove(listen)
		l, err := net.Listen("unix", listen)
		if err != nil {
			log.Fatal(err)
		}
		os.Chmod(listen, 0o666)
		network, addr, _ := strings.Cut(target, ":")
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					log.Fatal(err)
				}
				go func() {
					u, err := net.Dial(network, addr)
					if err != nil {
						log.Printf("forward: %v", err)
						c.Close()
						return
					}
					pipe(c, u)
				}()
			}
		}()
	}
	l, err := net.Listen("tcp", ":3128")
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go proxy(c, allow)
	}
}

func proxy(c net.Conn, allow []string) {
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		c.Close()
		return
	}
	host := req.URL.Hostname()
	if req.Method == http.MethodConnect {
		host, _, _ = strings.Cut(req.Host, ":")
	}
	ok := false
	for _, a := range allow {
		if a != "" && host == a {
			ok = true
		}
	}
	if !ok || req.Method != http.MethodConnect {
		log.Printf("deny %s %s", req.Method, host)
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		c.Close()
		return
	}
	u, err := net.Dial("tcp", req.Host)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		c.Close()
		return
	}
	log.Printf("allow %s", host)
	io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	pipe(c, u)
}

func pipe(a, b net.Conn) {
	go func() { io.Copy(a, b); a.Close() }()
	io.Copy(b, a)
	b.Close()
}
