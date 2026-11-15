package chatcompletionsext

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves a fake Chat Completions endpoint and names it as the
// bindings' base_url, so certification never calls a real provider.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// barrierHeader marks a request the fake endpoint holds until the
// runner's finish file exists. The inject case sends it, so the attempt's
// first request is still in flight when the runner injects.
const barrierHeader = "X-Fake-Barrier"

// finishFile is the inject case's finish file, in the fixture command's
// working directory (the runner's {tmp} for a fixture command).
const finishFile = "inject-go"

// Fixture is the certification fixture for a fake endpoint at baseURL:
// the executor case answers at once, and the inject case's requests wait
// for the runner's finish file, so the Event injected meanwhile is folded
// into a second request.
func Fixture(baseURL string) protocol.Fixture {
	run, _ := json.Marshal(map[string]any{"base_url": baseURL, "model": "fake"})
	inject, _ := json.Marshal(map[string]any{"base_url": baseURL, "model": "fake", "headers": map[string]string{barrierHeader: "1"}})
	return protocol.Fixture{
		Executor: &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Data: run}},
		Inject:   &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Data: inject}, FinishFile: "{tmp}/" + finishFile},
	}
}

// ServeFixture is `open-conduit extension-run chat-completions
// certify-fixture`: it serves the fake endpoint on a loopback port,
// prints the fixture and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	barrier := filepath.Join(dir, finishFile)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fakeEndpoint(w, r, barrier) }), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	b, err := json.Marshal(Fixture("http://" + l.Addr().String() + "/v1"))
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}

// fakeEndpoint answers every request with a text reply, after the
// barrier file exists when the request is marked to wait for it.
func fakeEndpoint(w http.ResponseWriter, r *http.Request, barrier string) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	if r.Header.Get(barrierHeader) != "" {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(barrier); err == nil {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"fake","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"certified"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
}
