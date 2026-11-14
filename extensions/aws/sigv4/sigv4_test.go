package sigv4_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/aws/sigv4"
)

// AWS's published Signature Version 4 example (IAM ListUsers, from the
// "Create a signed AWS API request" guide): the signature must match it
// exactly. Changing the key is the negative control.
func TestSignMatchesAWSExample(t *testing.T) {
	sign := func(secret string) string {
		req, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		sigv4.Sign(req, nil, sigv4.Key{ID: "AKIDEXAMPLE", Secret: secret}, "us-east-1", "iam", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
		return req.Header.Get("Authorization")
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := sign("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := sign("another"); got == want {
		t.Fatal("control: another key signed the same")
	}
}

// A session token is sent and signed.
func TestSignSessionToken(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://sts.eu-west-2.amazonaws.com/", strings.NewReader("Action=GetCallerIdentity"))
	sigv4.Sign(req, []byte("Action=GetCallerIdentity"), sigv4.Key{ID: "ASIA", Secret: "s", Token: "tok"}, "eu-west-2", "sts", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "tok" || !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("headers %v", req.Header)
	}
}
