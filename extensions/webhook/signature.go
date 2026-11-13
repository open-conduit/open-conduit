package webhook

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signature is a hook's check of the sender's own signature, on top of
// the URL token: senders such as GitHub, Slack, Stripe or Jira sign each
// body with a secret of their own. A delivery that doesn't verify is
// refused with 401 before it is accepted.
type Signature struct {
	// Secret names the entry secret the sender signs with (one of the
	// entry's secret_names).
	Secret string `json:"secret"`
	// Header is the header the signature is in ("X-Hub-Signature-256").
	// Its value is split on commas and spaces, and each item that starts
	// with Prefix is a signature; one of them must verify, so a sender
	// that sends several (Stripe while it rolls its secret) is taken.
	Header string `json:"header"`
	// Algorithm is hmac-sha256 (the default), hmac-sha1 or hmac-sha512.
	Algorithm string `json:"algorithm,omitempty"`
	// Encoding is how the MAC is written: hex (the default) or base64.
	Encoding string `json:"encoding,omitempty"`
	// Prefix is what comes before each signature ("sha256=", "v0=",
	// "v1="), stripped before it is compared.
	Prefix string `json:"prefix,omitempty"`
	// Signed is what the sender signs, from {body}, {timestamp},
	// {header:<name>} and literal text ("v0:{timestamp}:{body}"). It is
	// {body} by default and must hold {body} once.
	Signed string `json:"signed,omitempty"`
	// Timestamp is where the signed time of sending is read, for a sender
	// that signs one: header:<name>, body:<JSON pointer>, or
	// signature:<prefix> for an item of the signature header ("t=" for
	// Stripe). It is Unix seconds, or milliseconds when it is over 10^11.
	// A delivery whose time is further than ToleranceSeconds from now is
	// refused, so a captured delivery can't be replayed later.
	Timestamp string `json:"timestamp,omitempty"`
	// ToleranceSeconds is how far the timestamp may be from now: 300 by
	// default, at most a day.
	ToleranceSeconds int `json:"tolerance_seconds,omitempty"`
}

// DefaultTolerance is a signed timestamp's tolerance when the hook sets
// none.
const DefaultTolerance = 300

// MaxTolerance bounds a signed timestamp's tolerance.
const MaxTolerance = 86400

// verifier is a parsed Signature.
type verifier struct {
	sig       Signature
	hash      func() hash.Hash
	parts     []part
	timestamp *Key
	inSig     string // the signature item prefix the timestamp is read from
	tolerance time.Duration
}

// part is one piece of the signed template: literal text, the body, the
// timestamp or a header.
type part struct {
	kind  byte // 'l' literal, 'b' body, 't' timestamp, 'h' header
	value string
}

// errSignature is a delivery whose signature doesn't verify; its text is
// what the sender sees with the 401.
type errSignature string

func (e errSignature) Error() string { return string(e) }

func (s Signature) parse() (*verifier, error) {
	v := &verifier{sig: s}
	if s.Secret == "" {
		return nil, errors.New("needs its secret")
	}
	if !headerPattern.MatchString(s.Header) {
		return nil, fmt.Errorf("header %q is not a header name", s.Header)
	}
	v.sig.Header = http.CanonicalHeaderKey(s.Header)
	switch s.Algorithm {
	case "", "hmac-sha256":
		v.hash = sha256.New
	case "hmac-sha1":
		v.hash = sha1.New
	case "hmac-sha512":
		v.hash = sha512.New
	default:
		return nil, fmt.Errorf("algorithm %q must be hmac-sha256, hmac-sha1 or hmac-sha512", s.Algorithm)
	}
	switch s.Encoding {
	case "", "hex", "base64":
	default:
		return nil, fmt.Errorf("encoding %q must be hex or base64", s.Encoding)
	}
	if strings.ContainsAny(s.Prefix, ", \t") || len(s.Prefix) > 32 {
		return nil, fmt.Errorf("prefix %q must be at most 32 bytes, without commas or spaces", s.Prefix)
	}
	signed := s.Signed
	if signed == "" {
		signed = "{body}"
	}
	if len(signed) > 256 {
		return nil, errors.New("signed must be at most 256 bytes")
	}
	bodies, stamps := 0, 0
	for rest := signed; rest != ""; {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			v.parts = append(v.parts, part{'l', rest})
			break
		}
		if rest[i] == '}' {
			return nil, fmt.Errorf("signed %q has a } without its {", signed)
		}
		if i > 0 {
			v.parts = append(v.parts, part{'l', rest[:i]})
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("signed %q has a { without its }", signed)
		}
		name := rest[i+1 : i+j]
		rest = rest[i+j+1:]
		switch {
		case name == "body":
			bodies++
			v.parts = append(v.parts, part{'b', ""})
		case name == "timestamp":
			stamps++
			v.parts = append(v.parts, part{'t', ""})
		case strings.HasPrefix(name, "header:") && headerPattern.MatchString(name[len("header:"):]):
			v.parts = append(v.parts, part{'h', http.CanonicalHeaderKey(name[len("header:"):])})
		default:
			return nil, fmt.Errorf("signed %q: {%s} must be {body}, {timestamp} or {header:<name>}", signed, name)
		}
	}
	if bodies != 1 {
		return nil, fmt.Errorf("signed %q must hold {body} once", signed)
	}
	if s.Timestamp == "" {
		if stamps > 0 {
			return nil, errors.New("signed holds {timestamp}, so the hook needs timestamp")
		}
		if s.ToleranceSeconds != 0 {
			return nil, errors.New("tolerance_seconds needs timestamp")
		}
		return v, nil
	}
	if p, ok := strings.CutPrefix(s.Timestamp, "signature:"); ok {
		if p == "" || strings.ContainsAny(p, ", \t") || p == s.Prefix {
			return nil, fmt.Errorf("timestamp %q must name a signature item's prefix other than the signature's own", s.Timestamp)
		}
		v.inSig = p
	} else {
		k, err := ParseKey(s.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("timestamp: %w", err)
		}
		v.timestamp = &k
	}
	// A timestamp the signature doesn't cover proves nothing: a replay
	// could set it to now.
	if stamps == 0 && (v.timestamp == nil || v.timestamp.Pointer == "") {
		return nil, fmt.Errorf("timestamp %q isn't signed: put {timestamp} in signed", s.Timestamp)
	}
	switch tol := s.ToleranceSeconds; {
	case tol == 0:
		v.tolerance = DefaultTolerance * time.Second
	case tol < 0 || tol > MaxTolerance:
		return nil, fmt.Errorf("tolerance_seconds must be 1 to %d", MaxTolerance)
	default:
		v.tolerance = time.Duration(tol) * time.Second
	}
	return v, nil
}

// verify checks a delivery's signature and, when the hook reads one, its
// signed timestamp against now.
func (v *verifier) verify(h http.Header, body []byte, secret string, now time.Time) error {
	var sigs []string
	stamp := ""
	for _, item := range strings.FieldsFunc(strings.Join(h.Values(v.sig.Header), ","), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if v.inSig != "" && strings.HasPrefix(item, v.inSig) {
			stamp = item[len(v.inSig):]
			continue
		}
		if s, ok := strings.CutPrefix(item, v.sig.Prefix); ok && s != "" {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return errSignature("no signature in " + v.sig.Header)
	}
	if v.sig.Timestamp != "" {
		if v.timestamp != nil {
			stamp, _ = v.timestamp.Value(h, body)
		}
		n, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil || n <= 0 {
			return errSignature("no signed timestamp in " + v.sig.Timestamp)
		}
		at := time.Unix(n, 0)
		if n > 1e11 {
			at = time.UnixMilli(n)
		}
		if d := now.Sub(at); d > v.tolerance || d < -v.tolerance {
			return errSignature("signed timestamp is outside the hook's tolerance")
		}
	}
	mac := hmac.New(v.hash, []byte(secret))
	for _, p := range v.parts {
		switch p.kind {
		case 'l':
			mac.Write([]byte(p.value))
		case 'b':
			mac.Write(body)
		case 't':
			mac.Write([]byte(stamp))
		case 'h':
			mac.Write([]byte(h.Get(p.value)))
		}
	}
	want := mac.Sum(nil)
	for _, s := range sigs {
		var got []byte
		var err error
		if v.sig.Encoding == "base64" {
			got, err = base64.StdEncoding.DecodeString(s)
		} else {
			got, err = hex.DecodeString(s)
		}
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errSignature("signature in " + v.sig.Header + " does not verify")
}
