// Package awstest is a fake AWS Security Token Service and Secrets Manager
// for tests and certification, so neither needs an AWS account. It checks
// each request's Signature Version 4 against the key pairs it knows,
// answers sts:GetCallerIdentity and sts:AssumeRole in STS's XML and
// secretsmanager:GetSecretValue in its JSON, and records each AssumeRole it
// served.
package awstest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extensions/aws/sigv4"
)

// Assumed is one AssumeRole the fake served.
type Assumed struct {
	RoleARN, SessionName string
	Duration             time.Duration
	Tags                 map[string]string
	// AccessKeyID is the key ID it issued.
	AccessKeyID string
}

// Server is the fake STS.
type Server struct {
	*httptest.Server
	Region string
	Now    func() time.Time

	mu       sync.Mutex
	keys     map[string]key // by access key ID
	roles    map[string]bool
	untagged map[string]bool // roles whose trust policy denies sts:TagSession
	assumed  []Assumed
	secrets  map[string]string // SecretString by secret ID
	denied   map[string]bool   // secret IDs the keys may not read

	objects map[string][]byte   // S3 objects by bucket/key
	uploads map[string][][]byte // multipart uploads' parts by upload ID
	// FailS3 refuses a PUT of a bucket/key it returns true for.
	FailS3 func(key string) bool
	// S3PageSize bounds a listing's page; zero is 1000.
	S3PageSize int
}

type key struct {
	secret, account, arn string
}

// New starts a fake for region.
func New(region string) *Server {
	s := &Server{Region: region, Now: time.Now, keys: map[string]key{}, roles: map[string]bool{}, untagged: map[string]bool{}, secrets: map[string]string{}, denied: map[string]bool{}, objects: map[string][]byte{}, uploads: map[string][][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddKey lets a base key pair call, as the IAM principal arn in account.
func (s *Server) AddKey(id, secret, account, arn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[id] = key{secret, account, arn}
}

// AddRole lets the known keys assume roleARN; tags, false, makes its
// trust policy deny sts:TagSession.
func (s *Server) AddRole(roleARN string, tags bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[roleARN] = true
	s.untagged[roleARN] = !tags
}

// AddSecret stores a secret the known keys may read, by its ID (a name or
// an ARN); denied makes reading it an AccessDeniedException instead.
func (s *Server) AddSecret(id, value string, denied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[id] = value
	s.denied[id] = denied
}

// Assumed is every AssumeRole the fake has served, in order.
func (s *Server) Assumed() []Assumed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Assumed(nil), s.assumed...)
}

var credentialRE = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([^/]+)/(sts|secretsmanager|s3)/aws4_request, SignedHeaders=([^,]+), Signature=([0-9a-f]{64})$`)

func (s *Server) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>%s</Code><Message>refused</Message></Error><RequestId>fake</RequestId></ErrorResponse>`, code)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization"))
	secrets := r.Header.Get("X-Amz-Target") == "secretsmanager.GetSecretValue"
	fail := s.fail
	if secrets {
		fail = jsonFail
	}
	isS3 := m != nil && m[4] == "s3"
	if isS3 {
		fail = s3Fail
	}
	if (r.Method != http.MethodPost && !isS3) || m == nil || (m[4] == "secretsmanager") != secrets {
		fail(w, 403, "MissingAuthenticationToken")
		return
	}
	s.mu.Lock()
	k, ok := s.keys[m[1]]
	s.mu.Unlock()
	if !ok {
		fail(w, 403, map[bool]string{false: "InvalidClientTokenId", true: "UnrecognizedClientException"}[secrets])
		return
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || m[3] != s.Region || at.Format("20060102") != m[2] || s.Now().Sub(at).Abs() > 5*time.Minute {
		fail(w, 403, "SignatureDoesNotMatch")
		return
	}
	if !strings.Contains(";"+m[5]+";", ";host;") || !strings.Contains(";"+m[5]+";", ";x-amz-date;") {
		fail(w, 403, "SignatureDoesNotMatch")
		return
	}
	// Sign the same request again and compare.
	check, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	check.Host = r.Host
	for _, name := range strings.Split(m[5], ";") {
		if name != "host" && name != "authorization" {
			check.Header[http.CanonicalHeaderKey(name)] = r.Header.Values(name)
		}
	}
	sigv4.Sign(check, body, sigv4.Key{ID: m[1], Secret: k.secret, Token: r.Header.Get("X-Amz-Security-Token")}, s.Region, m[4], at)
	if check.Header.Get("Authorization") != r.Header.Get("Authorization") {
		fail(w, 403, "SignatureDoesNotMatch")
		return
	}
	if secrets {
		s.secret(w, body)
		return
	}
	if isS3 {
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			s3Fail(w, 400, "XAmzContentSHA256Mismatch")
			return
		}
		s.s3(w, r, body)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("Version") != "2011-06-15" {
		s.fail(w, 400, "InvalidAction")
		return
	}
	switch form.Get("Action") {
	case "GetCallerIdentity":
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>%s</Arn><UserId>AIDAFAKE</UserId><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, esc(k.arn), esc(k.account))
	case "AssumeRole":
		s.assume(w, form)
	default:
		s.fail(w, 400, "InvalidAction")
	}
}

// jsonFail is an error answer in Secrets Manager's JSON protocol, whose
// message, like the real one, names what was asked for.
func jsonFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": "refused for the request"})
}

// secret answers secretsmanager:GetSecretValue.
func (s *Server) secret(w http.ResponseWriter, body []byte) {
	var in struct {
		SecretID string `json:"SecretId"`
	}
	if json.Unmarshal(body, &in) != nil || in.SecretID == "" {
		jsonFail(w, 400, "InvalidParameterException")
		return
	}
	s.mu.Lock()
	v, ok := s.secrets[in.SecretID]
	denied := s.denied[in.SecretID]
	s.mu.Unlock()
	switch {
	case !ok:
		jsonFail(w, 400, "ResourceNotFoundException")
	case denied:
		jsonFail(w, 400, "AccessDeniedException")
	default:
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{"ARN": "arn:aws:secretsmanager:" + s.Region + ":111122223333:secret:" + in.SecretID, "Name": in.SecretID, "SecretString": v, "VersionId": "v1"})
	}
}

var sessionNameRE = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)

func (s *Server) assume(w http.ResponseWriter, form url.Values) {
	role, name := form.Get("RoleArn"), form.Get("RoleSessionName")
	secs, err := strconv.Atoi(form.Get("DurationSeconds"))
	if !sessionNameRE.MatchString(name) || err != nil || secs < 900 || secs > 43200 {
		s.fail(w, 400, "ValidationError")
		return
	}
	tags := map[string]string{}
	for i := 1; form.Has("Tags.member." + strconv.Itoa(i) + ".Key"); i++ {
		n := strconv.Itoa(i)
		tags[form.Get("Tags.member."+n+".Key")] = form.Get("Tags.member." + n + ".Value")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.roles[role] || (len(tags) > 0 && s.untagged[role]) {
		s.fail(w, 403, "AccessDenied")
		return
	}
	id := "ASIA" + strings.ToUpper(random(8))
	secret, token := random(20), "FwoG"+random(40)
	s.assumed = append(s.assumed, Assumed{RoleARN: role, SessionName: name, Duration: time.Duration(secs) * time.Second, Tags: tags, AccessKeyID: id})
	// A session's credentials sign as the role they were issued for.
	parts := strings.SplitN(role, ":", 6)
	account, roleName := "", strings.TrimPrefix(role, "arn:aws:iam::")
	if len(parts) == 6 {
		account, roleName = parts[4], strings.TrimPrefix(parts[5], "role/")
	}
	assumed := "arn:aws:sts::" + account + ":assumed-role/" + roleName + "/" + name
	s.keys[id] = key{secret, account, assumed}
	exp := s.Now().Add(time.Duration(secs) * time.Second).UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials><AssumedRoleUser><Arn>%s</Arn><AssumedRoleId>AROAFAKE:%s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult></AssumeRoleResponse>`,
		esc(id), esc(secret), esc(token), esc(exp), esc(assumed), esc(name))
}

// esc is a value as XML text: STS escapes its answers, and so must the
// fake, which echoes what the request named.
func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
