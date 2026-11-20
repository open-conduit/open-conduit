package aws_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/aws"
	"github.com/open-conduit/open-conduit/extensions/aws/awstest"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// secretsHarness serves the package's secret scheme against the fake,
// with the daemon's own identity the fixture's key pair (or none) and the
// environment env.
func secretsHarness(t *testing.T, identity bool, env map[string]string) (*sdktest.Host, *awstest.Server) {
	t.Helper()
	fake := awstest.New(aws.FixtureRegion)
	fake.Now = now
	t.Cleanup(fake.Close)
	aws.SeedFixture(fake)
	fake.AddSecret("prod/github", "ghp_plain", false)
	fake.AddSecret("prod/app", `{"token":"tok-1","port":5432}`, false)
	fake.AddSecret("prod/locked", "nope", true)
	fake.AddSecret("arn:aws:secretsmanager:"+aws.FixtureRegion+":111122223333:secret:prod/arn-AbCdEf", "by-arn", false)
	if env == nil {
		env = map[string]string{"AWS_REGION": aws.FixtureRegion}
	}
	env["AWS_ENDPOINT_URL_SECRETS_MANAGER"] = fake.URL
	ext := aws.Build(aws.Options{Now: now, Getenv: func(k string) string { return env[k] }, Ambient: func(string) func(context.Context) (aws.Keys, error) {
		return func(context.Context) (aws.Keys, error) {
			if !identity {
				return aws.Keys{}, aws.ErrNoBaseIdentity
			}
			return aws.Keys{AccessKeyID: aws.FixtureKeyID, SecretAccessKey: aws.FixtureSecret}, nil
		}
	}})
	return sdktest.New(t, ext), fake
}

func TestSecretsManagerScheme(t *testing.T) {
	h, _ := secretsHarness(t, true, nil)
	ctx := context.Background()
	for ref, want := range map[string]string{
		"prod/github":    "ghp_plain",
		"prod/app#token": "tok-1",
		"arn:aws:secretsmanager:" + aws.FixtureRegion + ":111122223333:secret:prod/arn-AbCdEf": "by-arn",
	} {
		got, err := h.ServeSecret(ctx, aws.SecretScheme, ref)
		if err != nil || got.Value != want || got.Code != "" {
			t.Errorf("%s: %+v, %v; want %q", ref, got, err, want)
		}
	}
	for ref, code := range map[string]string{
		"prod/missing":  "not_found",
		"prod/locked":   "denied",
		"prod/app#port": "key_missing",
		"prod/app#nope": "key_missing",
		"prod/github#k": "not_json",
		"#token":        "invalid_reference",
		"prod/app#a#b":  "invalid_reference",
	} {
		got, err := h.ServeSecret(ctx, aws.SecretScheme, ref)
		if err != nil || got.Code != code || got.Value != "" {
			t.Errorf("%s: %+v, %v; want refusal %s", ref, got, err, code)
		}
	}
}

func TestSecretsManagerSchemeNeedsTheDaemonsIdentityAndRegion(t *testing.T) {
	h, _ := secretsHarness(t, false, nil)
	if got, err := h.ServeSecret(context.Background(), aws.SecretScheme, "prod/github"); err != nil || got.Code != "no_identity" {
		t.Fatalf("%+v, %v", got, err)
	}
	h, _ = secretsHarness(t, true, map[string]string{})
	if got, err := h.ServeSecret(context.Background(), aws.SecretScheme, "prod/github"); err != nil || got.Code != "no_region" {
		t.Fatalf("%+v, %v", got, err)
	}
	// An ARN names its own region.
	h, _ = secretsHarness(t, true, map[string]string{})
	arn := "arn:aws:secretsmanager:" + aws.FixtureRegion + ":111122223333:secret:prod/arn-AbCdEf"
	if got, err := h.ServeSecret(context.Background(), aws.SecretScheme, arn); err != nil || got.Value != "by-arn" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestSecretsManagerRefusalsNeverCarryAWSsText(t *testing.T) {
	h, _ := secretsHarness(t, true, map[string]string{"AWS_REGION": "us-east-1"})
	// Signed for the wrong region: the fake refuses the signature.
	got, err := h.ServeSecret(context.Background(), aws.SecretScheme, "prod/github")
	if err != nil || got.Code != "identity_refused" || strings.Contains(got.Code, "prod") {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestManifestDeclaresTheSecretScheme(t *testing.T) {
	m := aws.Manifest()
	if len(m.Provides.SecretSchemes) != 1 || m.Provides.SecretSchemes[0].Scheme != "aws-sm" {
		t.Fatalf("%+v", m.Provides.SecretSchemes)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}
