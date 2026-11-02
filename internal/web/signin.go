package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SignIn is how a browser gets in: the token in the sign-in URL, and the
// key that signs the session cookie it trades it for. Kept in a file
// (LoadSignIn), it outlives the process, so a bookmarked sign-in URL and a
// signed-in browser keep working across restarts. Without one, a Server
// makes a fresh pair. Each sign-in gets a session of its own that expires
// (SessionIdle, SessionLifetime); a new Session key signs every browser out.
type SignIn struct {
	Login   string `json:"login"`
	Session string `json:"session"`
}

// LoadSignIn reads the sign-in kept at path, making it (0600, in a 0700
// directory) the first time. Delete the file, or sign out everywhere in the
// view, to sign every browser out.
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
//
// Prefer the first. Cookies are not kept apart by port, so a browser signed
// in by address sends its session to every other server on that address,
// such as an agent's dev server; AddressWarning says so wherever the second
// is printed.
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

// AddressWarning goes with the by-address sign-in URL wherever it is
// printed (see SignInURLs).
const AddressWarning = "its session cookie also reaches every other server on that address, so use it only if the first fails"

// SaveSignIn replaces the sign-in kept at path with s, as LoadSignIn reads
// it. Written aside and renamed into place, so a reader never sees half a
// file.
func SaveSignIn(path string, s SignIn) error {
	if len(s.Login) < 32 || len(s.Session) < 32 {
		return errors.New("web sign-in is not valid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(s)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".web-signin-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const (
	// SessionIdle is how long a signed-in browser may go without a request
	// before it has to sign in again.
	SessionIdle = 12 * time.Hour
	// SessionLifetime is how long a sign-in lasts however busy the browser
	// is.
	SessionLifetime = 7 * 24 * time.Hour
	// sessionRefresh is how stale a session's last-seen time may be before
	// a request renews the cookie.
	sessionRefresh = time.Minute
)

// webSession is one browser's sign-in: when it signed in, when it was last
// seen and a nonce of its own, signed with the sign-in's session key. Each
// sign-in mints a new one, so a cookie planted before sign-in (session
// fixation) is never the one that gets in; a new key (signing out
// everywhere) voids them all.
type webSession struct {
	issued, seen time.Time
	nonce        string
}

func (v webSession) encode(key string) string {
	body := strconv.FormatInt(v.issued.Unix(), 10) + "." + strconv.FormatInt(v.seen.Unix(), 10) + "." + v.nonce
	return body + "." + sessionMAC(key, body)
}

func sessionMAC(key, body string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("open-conduit web session\x00" + body))
	return hex.EncodeToString(m.Sum(nil))
}

// decodeSession reads a session cookie signed with key, refusing one that
// is forged, idle past SessionIdle or older than SessionLifetime at now.
func decodeSession(key, value string, now time.Time) (webSession, bool) {
	i := strings.LastIndexByte(value, '.')
	if i < 0 {
		return webSession{}, false
	}
	body, mac := value[:i], value[i+1:]
	if !hmac.Equal([]byte(mac), []byte(sessionMAC(key, body))) {
		return webSession{}, false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 || parts[2] == "" {
		return webSession{}, false
	}
	issued, err1 := strconv.ParseInt(parts[0], 10, 64)
	seen, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return webSession{}, false
	}
	v := webSession{issued: time.Unix(issued, 0), seen: time.Unix(seen, 0), nonce: parts[2]}
	if now.Sub(v.issued) > SessionLifetime || now.Sub(v.seen) > SessionIdle || v.seen.Before(v.issued) {
		return webSession{}, false
	}
	return v, true
}
