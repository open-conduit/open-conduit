package slacktest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Socket Mode: apps.connections.open (app tokens only) returns a ws:// URL
// on this server; connected clients receive hello, then whatever the test
// sends. Acknowledgements are recorded by envelope ID.

type socket struct {
	conn net.Conn
	mu   sync.Mutex
}

func (k *socket) write(op byte, payload []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	header := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	_, err := k.conn.Write(append(header, payload...))
	return err
}

// AddAppToken accepts an app-level token for apps.connections.open.
func (s *Server) AddAppToken(t string) { s.mu.Lock(); s.appTokens[t] = true; s.mu.Unlock() }

// SocketConnects reports how many Socket Mode connections were established.
func (s *Server) SocketConnects() int { s.mu.Lock(); defer s.mu.Unlock(); return s.connects }

// SocketsOpen reports how many Socket Mode connections are open.
func (s *Server) SocketsOpen() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sockets) }

// Acked reports whether the client acknowledged an envelope.
func (s *Server) Acked(envelope string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks[envelope]
}

func (s *Server) openConnection(r *http.Request) apiAnswer {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.appTokens[auth] {
		return apiAnswer{"ok": false, "error": "invalid_auth"}
	}
	return apiAnswer{"ok": true, "url": "ws://" + strings.TrimPrefix(s.srv.URL, "http://") + "/socket?ticket=t"}
}

func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		http.Error(w, "upgrade required", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
	_ = rw.Flush()
	k := &socket{conn: conn}
	s.mu.Lock()
	s.sockets = append(s.sockets, k)
	s.connects++
	s.mu.Unlock()
	_ = k.write(0x1, []byte(`{"type":"hello","num_connections":1,"connection_info":{"app_id":"AAPP"}}`))
	go s.readSocket(k, rw.Reader)
}

func (s *Server) dropSocket(k *socket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sockets[:0]
	for _, x := range s.sockets {
		if x != k {
			out = append(out, x)
		}
	}
	s.sockets = out
}

func (s *Server) readSocket(k *socket, r *bufio.Reader) {
	defer func() { s.dropSocket(k); k.conn.Close() }()
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		op := h[0] & 0x0F
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		mask := make([]byte, 4)
		if h[1]&0x80 == 0 {
			return // clients must mask
		}
		if _, err := io.ReadFull(r, mask); err != nil {
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch op {
		case 0x8:
			return
		case 0x1:
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(payload, &ack) == nil && ack.EnvelopeID != "" {
				s.mu.Lock()
				s.acks[ack.EnvelopeID] = true
				s.mu.Unlock()
			}
		}
	}
}

func (s *Server) broadcast(msg []byte) int {
	s.mu.Lock()
	sockets := append([]*socket(nil), s.sockets...)
	s.mu.Unlock()
	sent := 0
	for _, k := range sockets {
		if k.write(0x1, msg) == nil {
			sent++
		}
	}
	return sent
}

// SocketSend delivers an Events API callback body as a Socket Mode envelope
// to the open connections and returns the envelope ID ("" if none is open).
func (s *Server) SocketSend(callback []byte) string {
	s.mu.Lock()
	s.envelopes++
	id := fmt.Sprintf("env-%06d", s.envelopes)
	s.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"envelope_id": id, "type": "events_api", "accepts_response_payload": false, "retry_attempt": 0, "payload": json.RawMessage(callback)})
	if s.broadcast(msg) == 0 {
		return ""
	}
	return id
}

// SocketPing sends a ping frame to every open connection.
func (s *Server) SocketPing() {
	s.mu.Lock()
	sockets := append([]*socket(nil), s.sockets...)
	s.mu.Unlock()
	for _, k := range sockets {
		_ = k.write(0x9, []byte("p"))
	}
}

// SocketDisconnect asks every client to reconnect (Slack's refresh).
func (s *Server) SocketDisconnect() {
	s.broadcast([]byte(`{"type":"disconnect","reason":"refresh_requested"}`))
}

// SocketDrop closes every connection abruptly.
func (s *Server) SocketDrop() {
	s.mu.Lock()
	sockets := append([]*socket(nil), s.sockets...)
	s.mu.Unlock()
	for _, k := range sockets {
		k.conn.Close()
	}
}
