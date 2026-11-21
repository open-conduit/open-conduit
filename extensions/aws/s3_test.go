package aws

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/aws/awstest"
)

func s3Harness(t *testing.T) (*s3Store, extension.BackupTarget, *awstest.Server) {
	t.Helper()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	fake := awstest.New(FixtureRegion)
	fake.Now = func() time.Time { return at }
	t.Cleanup(fake.Close)
	fake.AddKey(FixtureKeyID, FixtureSecret, FixtureAccount, "arn:aws:iam::"+FixtureAccount+":user/open-conduit")
	store := &s3Store{o: Options{Now: fake.Now}, keys: func(context.Context) (Keys, error) {
		return Keys{AccessKeyID: FixtureKeyID, SecretAccessKey: FixtureSecret}, nil
	}}
	raw, _ := json.Marshal(S3Config{Bucket: "backups", Region: FixtureRegion, Prefix: "oc/prod", Endpoint: fake.URL})
	return store, extension.BackupTarget{Name: "offsite", Config: domain.Payload{Schema: extension.BackupDestinationConfigSchema, Data: raw}}, fake
}

func readAll(t *testing.T, s *s3Store, target extension.BackupTarget, bundle, path string) []byte {
	t.Helper()
	var out []byte
	for {
		chunk, eof, err := s.Read(context.Background(), target, bundle, path, int64(len(out)), 256<<10)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
		if eof {
			return out
		}
		if len(chunk) == 0 {
			t.Fatal("no progress before the end")
		}
	}
}

func TestS3PutListReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, target, fake := s3Harness(t)
	large := make([]byte, 2*s3PartSize+1234)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"conduit.db": large, "artifacts/ab/cd": []byte("artifact"), "artifacts/empty": {}, extension.BackupManifest: []byte("{}")}
	// A bundle without its manifest is incomplete, so it isn't listed.
	if err := s.Put(ctx, target, "partial", extension.BackupFile{Path: "conduit.db", Size: 3}, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"conduit.db", "artifacts/ab/cd", "artifacts/empty", extension.BackupManifest} {
		if err := s.Put(ctx, target, "conduit-1", extension.BackupFile{Path: p, Size: int64(len(files[p]))}, bytes.NewReader(files[p])); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if fake.OpenUploads() != 0 {
		t.Fatalf("%d multipart uploads left open", fake.OpenUploads())
	}
	fake.S3PageSize = 2 // every listing has to follow its continuation
	names, err := s.List(ctx, target)
	if err != nil || len(names) != 1 || names[0] != "conduit-1" {
		t.Fatalf("list = %v, %v", names, err)
	}
	stored, err := s.Files(ctx, target, "conduit-1")
	if err != nil || len(stored) != len(files) {
		t.Fatalf("files = %+v, %v", stored, err)
	}
	for _, f := range stored {
		if int64(len(files[f.Path])) != f.Size {
			t.Errorf("%s: size %d", f.Path, f.Size)
		}
		if got := readAll(t, s, target, "conduit-1", f.Path); !bytes.Equal(got, files[f.Path]) {
			t.Errorf("%s: read %d bytes, want %d", f.Path, len(got), len(files[f.Path]))
		}
	}
}

// A multipart upload that fails is aborted, so nothing of it shows.
func TestS3FailedMultipartLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, target, fake := s3Harness(t)
	fake.FailS3 = func(key string) bool { return strings.HasSuffix(key, "conduit.db") }
	large := make([]byte, s3PartSize+1)
	if err := s.Put(ctx, target, "b", extension.BackupFile{Path: "conduit.db", Size: int64(len(large))}, bytes.NewReader(large)); err == nil {
		t.Fatal("the upload succeeded")
	}
	if fake.OpenUploads() != 0 {
		t.Fatal("the failed upload was not aborted")
	}
	if _, ok := fake.Object("backups/oc/prod/b/conduit.db"); ok {
		t.Fatal("a failed upload is visible")
	}
}

func TestS3RefusesUnsafeNames(t *testing.T) {
	ctx := context.Background()
	s, target, _ := s3Harness(t)
	for _, c := range [][2]string{{"..", "conduit.db"}, {"b", "../other"}, {"b", "/abs"}, {"b/c", "x"}} {
		if err := s.Put(ctx, target, c[0], extension.BackupFile{Path: c[1]}, strings.NewReader("")); err == nil {
			t.Errorf("%v was accepted", c)
		}
	}
}

func TestS3Config(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"bucket":"my-backups","region":"eu-west-2"}`:                        true,
		`{"bucket":"my-backups","region":"eu-west-2","prefix":"oc/prod/"}`:    true,
		`{"bucket":"my.backups","region":"eu-west-2"}`:                        false,
		`{"bucket":"my.backups","region":"r","endpoint":"http://minio:9000"}`: true,
		`{"bucket":"my-backups"}`:                                             false,
		`{"bucket":"my-backups","region":"r","prefix":"../x"}`:                false,
		`{"bucket":"my-backups","region":"r","endpoint":"http://u:p@host"}`:   false,
		`{"bucket":"my-backups","region":"r","secret":"x"}`:                   false,
	} {
		if _, err := ParseS3Config([]byte(body)); (err == nil) != ok {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}
