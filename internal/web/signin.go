package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// SignIn is how a browser gets in: the token in the sign-in URL, and the
// session cookie it trades it for. Kept in a file (LoadSignIn), it outlives
// the process, so a bookmarked sign-in URL and a signed-in browser keep
// working across restarts. Without one, a Server makes a fresh pair.
type SignIn struct {
	Login   string `json:"login"`
	Session string `json:"session"`
}

// LoadSignIn reads the sign-in kept at path, making it (0600, in a 0700
// directory) the first time. Delete the file to sign every browser out.
func LoadSignIn(path string) (SignIn, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return makeSignIn(path)
	}
	if err != nil {
		return SignIn{}, err
	}
	var s SignIn
	if err := json.Unmarshal(data, &s); err != nil || len(s.Login) < 32 || len(s.Session) < 32 {
		return SignIn{}, fmt.Errorf("web sign-in file %s is not valid; delete it to make a new one", path)
	}
	return s, nil
}

func makeSignIn(path string) (SignIn, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SignIn{}, err
	}
	s := SignIn{Login: secret(), Session: secret()}
	data, _ := json.Marshal(s)
	// Written aside and linked into place, so a reader never sees half a
	// file and a second process racing to make it uses the first one's.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".web-signin-*")
	if err != nil {
		return SignIn{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err != nil {
		return SignIn{}, err
	}
	if err = os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return LoadSignIn(path)
	}
	if err != nil {
		return SignIn{}, err
	}
	return s, nil
}

// SignInURLs are the URLs that sign a browser in to a view served on addr
// under host: by name, then by address for a browser that does not resolve
// *.localhost. Empty addr and host mean DefaultAddr and DefaultHost.
func SignInURLs(s SignIn, addr, host string) []string {
	if addr == "" {
		addr = DefaultAddr
	}
	if host == "" {
		host = DefaultHost
	}
	_, port, _ := net.SplitHostPort(addr)
	q := "/login?token=" + s.Login
	return []string{"http://" + net.JoinHostPort(host, port) + q, "http://" + addr + q}
}
