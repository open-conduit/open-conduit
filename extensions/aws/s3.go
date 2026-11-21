package aws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/aws/sigv4"
)

// S3Destination is the backup destination type this package provides: a
// bucket and key prefix, reached with the daemon's own AWS identity, as the
// aws-sm scheme is. Lifecycle rules, versioning, object lock and server-side
// encryption are the bucket's own configuration.
const S3Destination = Name + "-s3"

const (
	// s3PartSize is the smallest part of a multipart upload this package
	// sends, and how much of a file it holds in memory while uploading.
	s3PartSize = 16 << 20
	// s3MaxParts stays under S3's limit of 10,000 parts.
	s3MaxParts = 9000
	// s3MaxObject is S3's largest object.
	s3MaxObject = 5 << 40
	// s3ReadWindow is how much one ranged GET fetches: the host reads in
	// small chunks, and a request for each would be slow and costly.
	s3ReadWindow = 8 << 20
)

// S3Config is an s3 backup destination's config.
type S3Config struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	// Prefix is the key prefix bundles go under.
	Prefix string `json:"prefix,omitempty"`
	// Endpoint replaces S3's regional endpoint (an emulator, a VPC
	// endpoint, an S3-compatible store), addressed path-style.
	Endpoint string `json:"endpoint,omitempty"`
}

var (
	s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3Region = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	s3Prefix = regexp.MustCompile(`^[A-Za-z0-9!_.*'()/-]{0,512}$`)
)

// ParseS3Config reads and checks a destination's config.
func ParseS3Config(raw []byte) (S3Config, error) {
	var c S3Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("an s3 destination takes bucket, region, prefix and endpoint")
	}
	if !s3Bucket.MatchString(c.Bucket) {
		return c, errors.New("bucket is not a valid S3 bucket name")
	}
	if !s3Region.MatchString(c.Region) {
		return c, errors.New("region is required, such as eu-west-2")
	}
	if !s3Prefix.MatchString(c.Prefix) || strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, "//") || strings.Contains("/"+c.Prefix+"/", "/../") {
		return c, errors.New("prefix is a plain key prefix, with no leading slash or empty or .. segments")
	}
	c.Prefix = strings.TrimSuffix(c.Prefix, "/")
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("endpoint is an http(s) URL with a host and no credentials")
		}
	} else if strings.Contains(c.Bucket, ".") {
		return c, errors.New("a bucket with dots needs an endpoint: its name doesn't fit S3's TLS certificate")
	}
	return c, nil
}

// s3Store serves S3Destination.
type s3Store struct {
	o    Options
	keys func(context.Context) (Keys, error)

	mu     sync.Mutex
	window struct {
		key    string
		offset int64
		data   []byte
		total  int64
	}
}

func (s *s3Store) config(t extension.BackupTarget) (S3Config, error) {
	c, err := ParseS3Config(t.Config.Data)
	if err != nil {
		return c, extension.Invalid("", "invalid_s3_destination", err.Error())
	}
	return c, nil
}

func (c S3Config) root() string {
	if c.Prefix == "" {
		return ""
	}
	return c.Prefix + "/"
}

func (c S3Config) url(key string, query url.Values) string {
	escaped := (&url.URL{Path: "/" + key}).EscapedPath()
	var out string
	if c.Endpoint != "" {
		out = strings.TrimSuffix(c.Endpoint, "/") + "/" + c.Bucket + escaped
	} else {
		out = "https://" + c.Bucket + ".s3." + c.Region + ".amazonaws.com" + escaped
	}
	if len(query) > 0 {
		out += "?" + strings.ReplaceAll(query.Encode(), "+", "%20")
	}
	return out
}

func (c S3Config) object(bundle, path string) (string, error) {
	if !extension.ValidBackupBundle(bundle) || !extension.ValidBackupPath(path) {
		return "", extension.Invalid("", "invalid_backup_path", "not a bundle file's name")
	}
	return c.root() + bundle + "/" + path, nil
}

// s3Error is S3's refusal, by its code alone: its messages echo the request.
type s3Error struct {
	Status int
	Code   string
}

func (e *s3Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("s3 refused the call: %s (%d)", e.Code, e.Status)
	}
	return fmt.Sprintf("s3 refused the call (%d)", e.Status)
}

func (s *s3Store) do(ctx context.Context, c S3Config, method, key string, query url.Values, headers map[string]string, body []byte) (*http.Response, error) {
	keys, err := s.keys(ctx)
	if err != nil || keys.AccessKeyID == "" || keys.SecretAccessKey == "" {
		return nil, errors.New("s3: the daemon has no AWS identity")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(key, query), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("s3: invalid endpoint")
	}
	if len(body) == 0 {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	now := time.Now
	if s.o.Now != nil {
		now = s.o.Now
	}
	sigv4.Sign(req, body, sigv4.Key{ID: keys.AccessKeyID, Secret: keys.SecretAccessKey, Token: keys.SessionToken}, c.Region, "s3", now())
	client := http.Client{Timeout: 10 * time.Minute}
	if s.o.HTTP != nil {
		client = *s.o.HTTP
	}
	// Never follow a redirect: a signed request goes only where it was
	// signed for.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("s3: unreachable")
	}
	return resp, nil
}

func s3Refusal(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal(raw, &e)
	return &s3Error{Status: resp.StatusCode, Code: e.Code}
}

// call makes a request and returns the body of a 2xx answer.
func (s *s3Store) call(ctx context.Context, c S3Config, method, key string, query url.Values, headers map[string]string, body []byte) (http.Header, []byte, error) {
	resp, err := s.do(ctx, c, method, key, query, headers, body)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, nil, s3Refusal(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, errors.New("s3: answer unreadable")
	}
	return resp.Header, raw, nil
}

// Put stores a file: in one PUT when it fits a part, otherwise as a
// multipart upload, which S3 shows only once it is completed. Each request
// carries its body's SHA-256, which S3 checks.
func (s *s3Store) Put(ctx context.Context, t extension.BackupTarget, bundle string, f extension.BackupFile, r io.Reader) error {
	c, err := s.config(t)
	if err != nil {
		return err
	}
	key, err := c.object(bundle, f.Path)
	if err != nil {
		return err
	}
	if f.Size > s3MaxObject {
		return errors.New("s3: the file is larger than S3's largest object")
	}
	part := max(int64(s3PartSize), f.Size/s3MaxParts+1)
	if f.Size <= part {
		body := make([]byte, f.Size)
		if _, err := io.ReadFull(r, body); err != nil {
			return err
		}
		_, _, err := s.call(ctx, c, http.MethodPut, key, nil, map[string]string{"Content-Type": "application/octet-stream"}, body)
		return err
	}
	_, raw, err := s.call(ctx, c, http.MethodPost, key, url.Values{"uploads": {""}}, map[string]string{"Content-Type": "application/octet-stream"}, nil)
	if err != nil {
		return err
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	if xml.Unmarshal(raw, &started) != nil || started.UploadID == "" {
		return errors.New("s3: answer unreadable")
	}
	abort := func() {
		a, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _, _ = s.call(a, c, http.MethodDelete, key, url.Values{"uploadId": {started.UploadID}}, nil, nil)
	}
	type completedPart struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	var parts []completedPart
	buf := make([]byte, part)
	for sent, n := int64(0), 1; sent < f.Size; n++ {
		size := min(part, f.Size-sent)
		if _, err := io.ReadFull(r, buf[:size]); err != nil {
			abort()
			return err
		}
		h, _, err := s.call(ctx, c, http.MethodPut, key, url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {started.UploadID}}, nil, buf[:size])
		if err != nil {
			abort()
			return err
		}
		parts = append(parts, completedPart{n, h.Get("ETag")})
		sent += size
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name        `xml:"CompleteMultipartUpload"`
		Parts   []completedPart `xml:"Part"`
	}{Parts: parts})
	if err != nil {
		abort()
		return err
	}
	_, raw, err = s.call(ctx, c, http.MethodPost, key, url.Values{"uploadId": {started.UploadID}}, map[string]string{"Content-Type": "application/xml"}, body)
	if err != nil {
		abort()
		return err
	}
	// S3 can answer 200 and report a failure in the body.
	var failed struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
	}
	if xml.Unmarshal(raw, &failed) == nil && failed.XMLName.Local == "Error" {
		abort()
		return &s3Error{Status: http.StatusOK, Code: failed.Code}
	}
	return nil
}

type s3Listing struct {
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
	IsTruncated bool   `xml:"IsTruncated"`
	Next        string `xml:"NextContinuationToken"`
}

func (s *s3Store) list(ctx context.Context, c S3Config, prefix, delimiter string, each func(s3Listing)) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if delimiter != "" {
			q.Set("delimiter", delimiter)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		_, raw, err := s.call(ctx, c, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return err
		}
		var page s3Listing
		if xml.Unmarshal(raw, &page) != nil {
			return errors.New("s3: answer unreadable")
		}
		each(page)
		if !page.IsTruncated {
			return nil
		}
		if page.Next == "" || page.Next == token {
			return errors.New("s3: listing did not advance")
		}
		token = page.Next
	}
}

// List names the bundles that hold their manifest.
func (s *s3Store) List(ctx context.Context, t extension.BackupTarget) ([]string, error) {
	c, err := s.config(t)
	if err != nil {
		return nil, err
	}
	var names []string
	err = s.list(ctx, c, c.root(), "/", func(p s3Listing) {
		for _, x := range p.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(x.Prefix, c.root()), "/")
			if extension.ValidBackupBundle(name) {
				names = append(names, name)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		resp, err := s.do(ctx, c, http.MethodHead, c.root()+name+"/"+extension.BackupManifest, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			out = append(out, name)
		case resp.StatusCode == http.StatusNotFound:
		default:
			return nil, &s3Error{Status: resp.StatusCode}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Files lists a bundle's objects.
func (s *s3Store) Files(ctx context.Context, t extension.BackupTarget, bundle string) ([]extension.BackupFile, error) {
	c, err := s.config(t)
	if err != nil {
		return nil, err
	}
	if !extension.ValidBackupBundle(bundle) {
		return nil, extension.Invalid("", "invalid_backup_path", "not a bundle's name")
	}
	root := c.root() + bundle + "/"
	var out []extension.BackupFile
	err = s.list(ctx, c, root, "", func(p s3Listing) {
		for _, x := range p.Contents {
			out = append(out, extension.BackupFile{Path: strings.TrimPrefix(x.Key, root), Size: x.Size})
		}
	})
	return out, err
}

// Read serves part of an object from a window fetched with one ranged GET.
func (s *s3Store) Read(ctx context.Context, t extension.BackupTarget, bundle, path string, offset int64, length int) ([]byte, bool, error) {
	c, err := s.config(t)
	if err != nil {
		return nil, false, err
	}
	key, err := c.object(bundle, path)
	if err != nil {
		return nil, false, err
	}
	id := c.url(key, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &s.window
	if w.key != id || offset < w.offset || offset >= w.offset+int64(len(w.data)) {
		data, total, err := s.fetch(ctx, c, key, offset)
		if err != nil {
			return nil, false, err
		}
		w.key, w.offset, w.data, w.total = id, offset, data, total
	}
	start := offset - w.offset
	if start > int64(len(w.data)) {
		return nil, true, nil
	}
	end := min(start+int64(length), int64(len(w.data)))
	out := bytes.Clone(w.data[start:end])
	return out, w.offset+end >= w.total, nil
}

var s3ContentRange = regexp.MustCompile(`^bytes \d+-\d+/(\d+)$`)

// fetch gets one window from offset, and the object's size.
func (s *s3Store) fetch(ctx context.Context, c S3Config, key string, offset int64) ([]byte, int64, error) {
	resp, err := s.do(ctx, c, http.MethodGet, key, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, offset+s3ReadWindow-1)}, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		m := s3ContentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if m == nil {
			return nil, 0, errors.New("s3: answer unreadable")
		}
		total, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, 0, errors.New("s3: answer unreadable")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, s3ReadWindow))
		if err != nil {
			return nil, 0, errors.New("s3: answer unreadable")
		}
		return data, total, nil
	case http.StatusRequestedRangeNotSatisfiable:
		// Reading at the end of an object, which is all of an empty one.
		return nil, offset, nil
	}
	return nil, 0, s3Refusal(resp)
}
