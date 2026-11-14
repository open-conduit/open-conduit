package aws

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extensions/aws/sigv4"
)

// Keys is an AWS access key pair, with the session token a temporary one
// carries.
type Keys struct {
	AccessKeyID, SecretAccessKey, SessionToken string
}

// ErrNoBaseIdentity is a connection with no usable base identity: no key
// pair in its secrets and none in the daemon's environment.
var ErrNoBaseIdentity = errors.New("aws: no base identity")

// STSError is an error answer from STS: its HTTP status and AWS's error
// code, never its message, which may echo the request.
type STSError struct {
	Status int
	Code   string
}

func (e *STSError) Error() string { return fmt.Sprintf("sts answered %d %s", e.Status, e.Code) }

// STS calls the AWS Security Token Service in one region with a base
// identity.
type STS struct {
	// Region signs every request and, without Endpoint, names the
	// regional endpoint.
	Region string
	// Endpoint replaces https://sts.<region>.amazonaws.com (an emulator,
	// a VPC endpoint).
	Endpoint string
	// Keys resolves the base identity for each request.
	Keys func(context.Context) (Keys, error)
	HTTP *http.Client
	Now  func() time.Time
}

// Credentials are temporary credentials STS issued.
type Credentials struct {
	Keys
	Expiration time.Time
	// AssumedRoleARN is the assumed-role ARN CloudTrail records the
	// calls under.
	AssumedRoleARN string
}

// Identity is what sts:GetCallerIdentity says the base identity is.
type Identity struct {
	Account, ARN, UserID string
}

// AssumeRequest is one sts:AssumeRole.
type AssumeRequest struct {
	RoleARN, SessionName string
	Duration             time.Duration
	// Tags are session tags, sent only when set: the role's trust policy
	// must allow sts:TagSession.
	Tags map[string]string
	// ExternalID is sent when the role's trust policy asks for one.
	ExternalID string
}

func (s *STS) endpoint() string {
	if s.Endpoint != "" {
		return strings.TrimSuffix(s.Endpoint, "/") + "/"
	}
	return "https://sts." + s.Region + ".amazonaws.com/"
}

// AssumeRole issues credentials for a role.
func (s *STS) AssumeRole(ctx context.Context, r AssumeRequest) (Credentials, error) {
	form := url.Values{"Action": {"AssumeRole"}, "Version": {"2011-06-15"}, "RoleArn": {r.RoleARN},
		"RoleSessionName": {r.SessionName}, "DurationSeconds": {strconv.Itoa(int(r.Duration / time.Second))}}
	if r.ExternalID != "" {
		form.Set("ExternalId", r.ExternalID)
	}
	keys := make([]string, 0, len(r.Tags))
	for k := range r.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		n := strconv.Itoa(i + 1)
		form.Set("Tags.member."+n+".Key", k)
		form.Set("Tags.member."+n+".Value", r.Tags[k])
	}
	var out struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
				Expiration      string `xml:"Expiration"`
			} `xml:"Credentials"`
			User struct {
				ARN string `xml:"Arn"`
			} `xml:"AssumedRoleUser"`
		} `xml:"AssumeRoleResult"`
	}
	if err := s.call(ctx, form, &out); err != nil {
		return Credentials{}, err
	}
	c := out.Result.Credentials
	exp, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return Credentials{}, errors.New("sts: malformed AssumeRole answer")
	}
	return Credentials{Keys: Keys{c.AccessKeyID, c.SecretAccessKey, c.SessionToken}, Expiration: exp.UTC(), AssumedRoleARN: out.Result.User.ARN}, nil
}

// CallerIdentity reads who the base identity is.
func (s *STS) CallerIdentity(ctx context.Context) (Identity, error) {
	var out struct {
		Result struct {
			Account string `xml:"Account"`
			ARN     string `xml:"Arn"`
			UserID  string `xml:"UserId"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := s.call(ctx, url.Values{"Action": {"GetCallerIdentity"}, "Version": {"2011-06-15"}}, &out); err != nil {
		return Identity{}, err
	}
	return Identity{Account: out.Result.Account, ARN: out.Result.ARN, UserID: out.Result.UserID}, nil
}

func (s *STS) call(ctx context.Context, form url.Values, out any) error {
	keys, err := s.Keys(ctx)
	if err != nil {
		return err
	}
	if keys.AccessKeyID == "" || keys.SecretAccessKey == "" {
		return ErrNoBaseIdentity
	}
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint(), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sigv4.Sign(req, []byte(body), sigv4.Key{ID: keys.AccessKeyID, Secret: keys.SecretAccessKey, Token: keys.SessionToken}, s.Region, "sts", now())
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow a redirect: a signed request goes only where it was
	// signed for.
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code string `xml:"Code"`
			} `xml:"Error"`
		}
		_ = xml.Unmarshal(raw, &e)
		return &STSError{Status: resp.StatusCode, Code: e.Error.Code}
	}
	return xml.Unmarshal(raw, out)
}
