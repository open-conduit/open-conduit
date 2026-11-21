package awstest

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func s3Fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>refused for the request</Message></Error>`, code)
}

// Object returns a stored S3 object.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

// PutObject stores an S3 object directly, as someone with write access to
// the bucket could.
func (s *Server) PutObject(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = bytes.Clone(data)
}

// OpenUploads counts multipart uploads neither completed nor aborted.
func (s *Server) OpenUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// s3 serves the path-style calls a backup destination makes.
func (s *Server) s3(w http.ResponseWriter, r *http.Request, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	upload := q.Get("uploadId")
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.s3List(w, key, q.Get("prefix"), q.Get("delimiter"), q.Get("continuation-token"))
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("upload-%d", len(s.uploads)+len(s.objects)+1)
		s.uploads[id] = nil
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && upload != "":
		parts, ok := s.uploads[upload]
		n, err := strconv.Atoi(q.Get("partNumber"))
		if !ok || err != nil || n != len(parts)+1 {
			s3Fail(w, 404, "NoSuchUpload")
			return
		}
		if s.FailS3 != nil && s.FailS3(key) {
			s3Fail(w, 503, "SlowDown")
			return
		}
		s.uploads[upload] = append(parts, bytes.Clone(body))
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
	case r.Method == http.MethodPost && upload != "":
		parts, ok := s.uploads[upload]
		if !ok || strings.Count(string(body), "<Part>") != len(parts) {
			s3Fail(w, 400, "InvalidPart")
			return
		}
		s.objects[key] = bytes.Join(parts, nil)
		delete(s.uploads, upload)
		fmt.Fprint(w, `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && upload != "":
		delete(s.uploads, upload)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if s.FailS3 != nil && s.FailS3(key) {
			s3Fail(w, 503, "SlowDown")
			return
		}
		s.objects[key] = bytes.Clone(body)
	case r.Method == http.MethodHead:
		if _, ok := s.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodGet:
		s.s3Get(w, r, key)
	default:
		s3Fail(w, 405, "MethodNotAllowed")
	}
}

func (s *Server) s3Get(w http.ResponseWriter, r *http.Request, key string) {
	data, ok := s.objects[key]
	if !ok {
		s3Fail(w, 404, "NoSuchKey")
		return
	}
	var from, to int64
	if n, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil || n != 2 {
		s3Fail(w, 400, "InvalidRange")
		return
	}
	size := int64(len(data))
	if from >= size {
		s3Fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
		return
	}
	to = min(to, size-1)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[from : to+1])
}

func (s *Server) s3List(w http.ResponseWriter, bucket, prefix, delimiter, token string) {
	root := strings.TrimSuffix(bucket, "/") + "/"
	sizes := map[string]int{}
	common := map[string]bool{}
	for k, v := range s.objects {
		name, ok := strings.CutPrefix(k, root)
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		if delimiter != "" {
			if head, _, cut := strings.Cut(name[len(prefix):], delimiter); cut {
				common[prefix+head+delimiter] = true
				continue
			}
		}
		sizes[name] = len(v)
	}
	entries := make([]string, 0, len(sizes)+len(common))
	for k := range sizes {
		entries = append(entries, k)
	}
	for k := range common {
		entries = append(entries, k)
	}
	sort.Strings(entries)
	page := s.S3PageSize
	if page <= 0 {
		page = 1000
	}
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	end := min(start+page, len(entries))
	var b strings.Builder
	b.WriteString("<ListBucketResult>")
	for _, e := range entries[start:end] {
		if common[e] {
			fmt.Fprintf(&b, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", esc(e))
		} else {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", esc(e), sizes[e])
		}
	}
	if end < len(entries) {
		fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(b.String()))
}
