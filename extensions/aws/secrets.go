package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/aws/sigv4"
)

// SecretScheme is the secret reference scheme the package serves from AWS
// Secrets Manager: "aws-sm:<secret>" is a secret's SecretString, and
// "aws-sm:<secret>#<key>" one string field of a SecretString that is a
// JSON object, where <secret> is the secret's name or its ARN. It reads
// with the daemon's own AWS identity (the package's environment, which
// settings.extensions.aws.pass_env extends: AWS_REGION, AWS_ROLE_ARN and
// AWS_WEB_IDENTITY_TOKEN_FILE for IRSA, AWS_PROFILE), never a connection's,
// so the credentials it serves can't depend on a secret it serves. The
// region is the ARN's, else AWS_REGION or AWS_DEFAULT_REGION, and
// AWS_ENDPOINT_URL_SECRETS_MANAGER or AWS_ENDPOINT_URL replaces the
// regional endpoint, as for an AWS SDK.
const SecretScheme = Name + "-sm"

var secretARN = regexp.MustCompile(`^arn:aws[a-z-]*:secretsmanager:([a-z0-9-]+):\d{12}:secret:.+$`)

// secrets serves SecretScheme.
type secrets struct {
	o    Options
	keys func(context.Context) (Keys, error)
}

func (s *secrets) getenv(k string) string {
	if s.o.Getenv != nil {
		return s.o.Getenv(k)
	}
	return os.Getenv(k)
}

// Serve reads one secret. Every answer it can't give is a refusal with a
// safe code (not_found, denied, no_identity, no_region, key_missing, …);
// AWS's own messages, which echo the request, are never passed on.
func (s *secrets) Serve(ctx context.Context, r extension.SecretRequest) (string, error) {
	id, field, _ := strings.Cut(r.Reference, "#")
	if id == "" || strings.Contains(field, "#") {
		return "", &extension.SecretRefusal{Code: "invalid_reference"}
	}
	region := ""
	if m := secretARN.FindStringSubmatch(id); m != nil {
		region = m[1]
	} else if region = s.getenv("AWS_REGION"); region == "" {
		region = s.getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		return "", &extension.SecretRefusal{Code: "no_region"}
	}
	keys, err := s.keys(ctx)
	if err != nil || keys.AccessKeyID == "" || keys.SecretAccessKey == "" {
		return "", &extension.SecretRefusal{Code: "no_identity"}
	}
	endpoint := s.getenv("AWS_ENDPOINT_URL_SECRETS_MANAGER")
	if endpoint == "" {
		endpoint = s.getenv("AWS_ENDPOINT_URL")
	}
	if endpoint == "" {
		endpoint = "https://secretsmanager." + region + ".amazonaws.com"
	}
	body, _ := json.Marshal(map[string]string{"SecretId": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return "", &extension.SecretRefusal{Code: "invalid_endpoint"}
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	now := time.Now
	if s.o.Now != nil {
		now = s.o.Now
	}
	sigv4.Sign(req, body, sigv4.Key{ID: keys.AccessKeyID, Secret: keys.SecretAccessKey, Token: keys.SessionToken}, region, "secretsmanager", now())
	hc := s.o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow a redirect: a signed request goes only where it was
	// signed for.
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("aws: secrets manager unreachable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New("aws: secrets manager answer unreadable")
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Type string `json:"__type"`
		}
		_ = json.Unmarshal(raw, &e)
		_, typ, _ := strings.Cut(e.Type, "#")
		if typ == "" {
			typ = e.Type
		}
		if code, ok := secretErrors[typ]; ok {
			return "", &extension.SecretRefusal{Code: code}
		}
		return "", errors.New("aws: secrets manager refused the call")
	}
	var out struct {
		SecretString *string `json:"SecretString"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", errors.New("aws: secrets manager answer unreadable")
	}
	if out.SecretString == nil {
		return "", &extension.SecretRefusal{Code: "binary_secret"}
	}
	if field == "" {
		return *out.SecretString, nil
	}
	var fields map[string]any
	if json.Unmarshal([]byte(*out.SecretString), &fields) != nil {
		return "", &extension.SecretRefusal{Code: "not_json"}
	}
	v, ok := fields[field].(string)
	if !ok {
		return "", &extension.SecretRefusal{Code: "key_missing"}
	}
	return v, nil
}

// secretErrors are Secrets Manager's error types the package names, as
// safe codes. Any other is provider_failed.
var secretErrors = map[string]string{
	"ResourceNotFoundException":   "not_found",
	"AccessDeniedException":       "denied",
	"DecryptionFailure":           "decryption_failed",
	"InvalidRequestException":     "unavailable",
	"InvalidParameterException":   "invalid_reference",
	"UnrecognizedClientException": "identity_refused",
	"InvalidSignatureException":   "identity_refused",
	"SignatureDoesNotMatch":       "identity_refused",
	"ExpiredTokenException":       "identity_refused",
	"MissingAuthenticationToken":  "identity_refused",
}
