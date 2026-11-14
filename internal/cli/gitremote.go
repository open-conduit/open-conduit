package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// gitRemote is git's remote helper for the toolbelt's git transport
// (extension.GitTransport): git runs it, through the alias
// remote-open-conduit, for a remote URL open-conduit::https://host/path,
// as `os git-remote <remote> <url>`. It opens a session for the repository
// on the host, which asks the operations serving git.credential for a
// credential and keeps it, then carries git's own protocol over the
// toolbelt: protocol v2 to fetch (each request one exchange), and a push's
// one exchange. The host signs every request in, so git gets no credential
// and neither does anything else in the run.
//
// A repository no granted operation gives a credential for is fetched
// directly, by git's own https helper, as git would without the
// transport. A gate that holds the sign-in fails the command with what the
// gate said, which git prints for the agent.
func (t *toolbelt) gitRemote(args []string) int {
	if len(args) < 1 || len(args) > 2 {
		return t.usage("git-remote takes the remote and its URL, as git runs it")
	}
	repo := args[len(args)-1]
	c, err := t.client()
	if err != nil {
		fmt.Fprintf(t.errOut, "os: %s\n", err)
		return 1
	}
	octx, cancel := context.WithTimeout(context.Background(), callTimeout)
	var sess client.GitSession
	err = t.withRecovery(octx, func(c *client.Client) (e error) {
		sess, e = c.GitSessionOpen(octx, repo)
		return e
	})
	cancel()
	var api *client.Error
	switch {
	case errors.As(err, &api) && api.Code == "git_no_credential":
		return t.gitDirect(args)
	case errors.As(err, &api) && api.Code == "git_sign_in_held":
		fmt.Fprintf(t.errOut, "os: no git sign-in yet: %s\n", oneLine(api.Message))
		return 1
	case err != nil:
		fmt.Fprintf(t.errOut, "os: git can't reach %s: %s\n", oneLine(repo), oneLine(callError(err)))
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.GitSessionEnd(ctx, sess.Session)
	}()
	in := bufio.NewReader(t.stdin)
	out := bufio.NewWriter(t.out)
	for {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return 0
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return 0
		case line == "capabilities":
			fmt.Fprint(out, "connect\n\n")
			_ = out.Flush()
		case strings.HasPrefix(line, "connect "):
			service := strings.TrimPrefix(line, "connect ")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				fmt.Fprintf(t.errOut, "os: Open Conduit's git transport fetches and pushes; it doesn't carry %s\n", oneLine(service))
				return 1
			}
			fmt.Fprint(out, "\n")
			_ = out.Flush()
			g := &gitConn{c: c, session: sess.Session, service: service, in: in, out: out}
			if service == "git-upload-pack" {
				err = g.fetch()
			} else {
				err = g.push()
			}
			if err != nil {
				fmt.Fprintf(t.errOut, "os: git can't reach %s: %s\n", oneLine(repo), oneLine(callError(err)))
				return 1
			}
			return 0
		default:
			fmt.Fprintf(t.errOut, "os: git-remote doesn't know the command %q\n", oneLine(line))
			return 1
		}
	}
}

// gitDirect hands git's command to git's own https helper, for a
// repository the toolbelt has no credential for: git reaches it directly,
// as it would without the transport.
func (t *toolbelt) gitDirect(args []string) int {
	cmd := exec.Command("git", append([]string{"remote-https"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = t.stdin, t.out, t.errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintf(t.errOut, "os: git's https helper didn't run: %s\n", err)
		return 1
	}
	return 0
}

// gitConn is one service's connection, as git's connect gives it: git
// writes its side of the protocol to in and reads the repository's from
// out.
type gitConn struct {
	c                *client.Client
	session, service string
	in               *bufio.Reader
	out              *bufio.Writer
}

// fetch carries git-upload-pack in protocol v2, whose requests each stand
// alone: the advertisement, then each request git sends (up to its flush)
// as one exchange, until git sends a bare flush or closes.
func (g *gitConn) fetch() error {
	const v2 = "version=2"
	if err := g.advertise(v2, true); err != nil {
		return err
	}
	for {
		req, err := readRequest(g.in)
		if errors.Is(err, io.EOF) && len(req) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if string(req) == "0000" {
			return nil
		}
		if err := g.exchange(v2, bytes.NewReader(req)); err != nil {
			return err
		}
	}
}

// push carries git-receive-pack: the advertisement, then git's one
// request: its commands up to their flush, its push options when it
// negotiated them, and the pack git sends when a command isn't a delete,
// which ends when git closes its side.
func (g *gitConn) push() error {
	if err := g.advertise("", false); err != nil {
		return err
	}
	var head bytes.Buffer
	commands, pack, options := 0, false, false
	for first := true; ; first = false {
		raw, payload, err := readPkt(g.in)
		if errors.Is(err, io.EOF) && head.Len() == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		head.Write(raw)
		if payload == nil {
			break
		}
		line := payload
		if i := bytes.IndexByte(line, 0); i >= 0 {
			if first {
				for _, c := range strings.Fields(string(line[i+1:])) {
					options = options || c == "push-options"
				}
			}
			line = line[:i]
		}
		f := strings.Fields(string(line))
		if len(f) == 3 && objectID(f[0]) && objectID(f[1]) {
			commands++
			pack = pack || strings.Trim(f[1], "0") != ""
		}
	}
	if commands == 0 {
		// Nothing to update: git sent only its flush.
		return nil
	}
	body := []io.Reader{&head}
	if options {
		var opts bytes.Buffer
		for {
			raw, payload, err := readPkt(g.in)
			if err != nil {
				return err
			}
			opts.Write(raw)
			if payload == nil {
				break
			}
		}
		body = append(body, &opts)
	}
	if pack {
		body = append(body, g.in)
	}
	return g.exchange("", io.MultiReader(body...))
}

// advertise relays the service's advertisement, without the "# service="
// header smart HTTP puts before it. With v2, the repository must answer in
// protocol v2: the transport carries no other fetch.
func (g *gitConn) advertise(protocol string, v2 bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	body, err := g.c.GitAdvertise(ctx, g.session, g.service, protocol)
	if err != nil {
		return err
	}
	defer body.Close()
	r := bufio.NewReader(body)
	raw, payload, err := readPkt(r)
	if err != nil {
		return fmt.Errorf("the repository's advertisement is unreadable: %w", err)
	}
	if bytes.HasPrefix(payload, []byte("# service=")) {
		if _, p, err := readPkt(r); err != nil || p != nil {
			return errors.New("the repository's advertisement is unreadable")
		}
		if raw, payload, err = readPkt(r); err != nil {
			return fmt.Errorf("the repository's advertisement is unreadable: %w", err)
		}
	}
	if v2 && strings.TrimSuffix(string(payload), "\n") != "version 2" {
		return errors.New("the repository doesn't speak git protocol v2, the only one Open Conduit's git transport fetches with")
	}
	g.out.Write(raw)
	if _, err := io.Copy(g.out, r); err != nil {
		return err
	}
	return g.out.Flush()
}

// exchange sends one request and relays the repository's answer.
func (g *gitConn) exchange(protocol string, req io.Reader) error {
	body, err := g.c.GitExchange(context.Background(), g.session, g.service, protocol, req)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(g.out, body); err != nil {
		return err
	}
	return g.out.Flush()
}

// readRequest reads one protocol v2 request: pkt-lines up to and including
// its flush.
func readRequest(r *bufio.Reader) ([]byte, error) {
	var req bytes.Buffer
	for {
		raw, payload, err := readPkt(r)
		req.Write(raw)
		if err != nil {
			if errors.Is(err, io.EOF) && req.Len() > 0 {
				err = io.ErrUnexpectedEOF
			}
			return req.Bytes(), err
		}
		if payload == nil && string(raw) == "0000" {
			return req.Bytes(), nil
		}
	}
}

// readPkt reads one pkt-line: raw is it as sent, payload its data (nil for
// a flush, delim or response-end packet).
func readPkt(r *bufio.Reader) (raw, payload []byte, err error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, nil, err
	}
	n, err := strconv.ParseUint(string(head), 16, 16)
	if err != nil {
		return nil, nil, errors.New("malformed pkt-line")
	}
	if n < 4 {
		if n == 3 {
			return nil, nil, errors.New("malformed pkt-line")
		}
		return head, nil, nil
	}
	raw = make([]byte, n)
	copy(raw, head)
	if _, err := io.ReadFull(r, raw[4:]); err != nil {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return raw, raw[4:], nil
}

// objectID reports whether s is a hex object name (SHA-1 or SHA-256).
func objectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// oneLine keeps a message on one line.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
