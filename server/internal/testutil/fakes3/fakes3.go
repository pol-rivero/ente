// Package fakes3 is an in-memory S3 server for server-side copy tests. Objects
// only have a size and an ETag; no data is stored.
package fakes3

import (
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const Bucket = "test-bucket"

type Op string

const (
	OpHead     Op = "HeadObject"
	OpCopy     Op = "CopyObject"
	OpCreate   Op = "CreateMultipartUpload"
	OpPartCopy Op = "UploadPartCopy"
	OpComplete Op = "CompleteMultipartUpload"
	OpAbort    Op = "AbortMultipartUpload"
)

const MinPartSize = int64(5) << 20

type Request struct {
	Op         Op
	Key        string
	UploadID   string
	PartNumber int64
	CopySource string
	Range      string
	Parts      []int64
	// Closed when the client gives up on the request.
	Done <-chan struct{}
}

type Failure struct {
	Status int
	Code   string
}

type Object struct {
	Size int64
	ETag string
}

type Part struct {
	Start, End int64
	ETag       string
	Source     string
	SourceSize int64
}

type Upload struct {
	Key   string
	Parts map[int64]Part
}

type Server struct {
	URL string

	t        *testing.T
	mu       sync.Mutex
	nextID   int
	objects  map[string]Object
	uploads  map[string]*Upload
	requests []Request
	// Runs before each request is handled, outside the lock; a non-nil
	// failure is returned to the client instead.
	hook func(Request) *Failure
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{t: t, objects: map[string]Object{}, uploads: map[string]*Upload{}}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	s.URL = server.URL
	return s
}

func (s *Server) SetHook(hook func(Request) *Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *Server) PutObject(key string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = Object{Size: size, ETag: fmt.Sprintf(`"etag-%s"`, key)}
}

func (s *Server) Object(key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	return object, ok
}

func (s *Server) Uploads() map[string]Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	uploads := make(map[string]Upload, len(s.uploads))
	for id, upload := range s.uploads {
		uploads[id] = Upload{Key: upload.Key, Parts: maps.Clone(upload.Parts)}
	}
	return uploads
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func (s *Server) RequestsOf(op Op) []Request {
	var matching []Request
	for _, r := range s.Requests() {
		if r.Op == op {
			matching = append(matching, r)
		}
	}
	return matching
}

var rangePattern = regexp.MustCompile(`^bytes=(\d+)-(\d+)$`)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/"+Bucket+"/"))
	if err != nil {
		s.fail(w, "bad path %s", r.URL)
		return
	}
	query := r.URL.Query()
	req := Request{
		Key:        key,
		UploadID:   query.Get("uploadId"),
		CopySource: r.Header.Get("X-Amz-Copy-Source"),
		Range:      r.Header.Get("X-Amz-Copy-Source-Range"),
		Done:       r.Context().Done(),
	}
	req.PartNumber, _ = strconv.ParseInt(query.Get("partNumber"), 10, 64)
	var completeBody completeMultipartUpload
	switch {
	case r.Method == http.MethodHead:
		req.Op = OpHead
	case r.Method == http.MethodPost && query.Has("uploads"):
		req.Op = OpCreate
	case r.Method == http.MethodPut && req.UploadID != "" && req.CopySource != "":
		req.Op = OpPartCopy
	case r.Method == http.MethodPut && req.CopySource != "":
		req.Op = OpCopy
	case r.Method == http.MethodPost && req.UploadID != "":
		req.Op = OpComplete
		body, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(body, &completeBody); err != nil {
			s.fail(w, "bad complete body: %v", err)
			return
		}
		for _, part := range completeBody.Parts {
			req.Parts = append(req.Parts, part.PartNumber)
		}
	case r.Method == http.MethodDelete && req.UploadID != "":
		req.Op = OpAbort
	default:
		s.fail(w, "unexpected request %s %s", r.Method, r.URL)
		return
	}
	if query.Has("uploadId") && req.UploadID == "" {
		s.fail(w, "empty upload ID: %s %s", r.Method, r.URL)
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		if failure := hook(req); failure != nil {
			writeError(w, failure.Status, failure.Code)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Op {
	case OpHead:
		object, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
		w.Header().Set("ETag", object.ETag)
		w.WriteHeader(http.StatusOK)
	case OpCopy:
		_, source, ok := s.source(w, req.CopySource)
		if !ok {
			return
		}
		s.objects[key] = Object{Size: source.Size, ETag: fmt.Sprintf(`"copy-%s"`, key)}
		_, _ = fmt.Fprintf(w, `<CopyObjectResult><ETag>%s</ETag></CopyObjectResult>`, s.objects[key].ETag)
	case OpCreate:
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.uploads[id] = &Upload{Key: key, Parts: map[int64]Part{}}
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			Bucket, key, id)
	case OpPartCopy:
		upload, ok := s.upload(w, req)
		if !ok {
			return
		}
		sourceKey, source, ok := s.source(w, req.CopySource)
		if !ok {
			return
		}
		match := rangePattern.FindStringSubmatch(req.Range)
		if match == nil {
			s.fail(w, "bad copy range %q", req.Range)
			return
		}
		start, _ := strconv.ParseInt(match[1], 10, 64)
		end, _ := strconv.ParseInt(match[2], 10, 64)
		if start > end || end >= source.Size || req.PartNumber < 1 || req.PartNumber > 10000 {
			writeError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		part := Part{Start: start, End: end, ETag: fmt.Sprintf(`"part-%d"`, req.PartNumber), Source: sourceKey, SourceSize: source.Size}
		upload.Parts[req.PartNumber] = part
		_, _ = fmt.Fprintf(w, `<CopyPartResult><ETag>%s</ETag></CopyPartResult>`, part.ETag)
	case OpComplete:
		upload, ok := s.upload(w, req)
		if !ok {
			return
		}
		if !slices.IsSorted(req.Parts) {
			writeError(w, http.StatusBadRequest, "InvalidPartOrder")
			return
		}
		var size int64
		var first Part
		for i, listed := range completeBody.Parts {
			part, ok := upload.Parts[listed.PartNumber]
			if !ok || part.ETag != listed.ETag {
				writeError(w, http.StatusBadRequest, "InvalidPart")
				return
			}
			if i < len(completeBody.Parts)-1 && part.End-part.Start+1 < MinPartSize {
				writeError(w, http.StatusBadRequest, "EntityTooSmall")
				return
			}
			if i == 0 {
				first = part
			}
			if part.Start != size || part.Source != first.Source || part.SourceSize != first.SourceSize {
				s.fail(w, "part %d copies %s %d-%d, not contiguous with the previous parts", listed.PartNumber, part.Source, part.Start, part.End)
				return
			}
			size += part.End - part.Start + 1
		}
		if size != first.SourceSize {
			s.fail(w, "parts cover %d bytes of a %d-byte source", size, first.SourceSize)
			return
		}
		s.objects[key] = Object{Size: size, ETag: fmt.Sprintf(`"multipart-%s"`, key)}
		delete(s.uploads, req.UploadID)
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`,
			Bucket, key, s.objects[key].ETag)
	case OpAbort:
		if _, ok := s.upload(w, req); !ok {
			return
		}
		delete(s.uploads, req.UploadID)
		w.WriteHeader(http.StatusNoContent)
	}
}

type completeMultipartUpload struct {
	Parts []struct {
		PartNumber int64
		ETag       string
	} `xml:"Part"`
}

func (s *Server) source(w http.ResponseWriter, copySource string) (string, Object, bool) {
	bucket, escapedKey, _ := strings.Cut(copySource, "/")
	key, err := url.PathUnescape(escapedKey)
	if bucket != Bucket || err != nil {
		s.fail(w, "bad copy source %q", copySource)
		return "", Object{}, false
	}
	object, ok := s.objects[key]
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchKey")
	}
	return key, object, ok
}

func (s *Server) upload(w http.ResponseWriter, req Request) (*Upload, bool) {
	upload, ok := s.uploads[req.UploadID]
	if !ok || upload.Key != req.Key {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return nil, false
	}
	return upload, true
}

func (s *Server) fail(w http.ResponseWriter, format string, args ...any) {
	s.t.Errorf(format, args...)
	writeError(w, http.StatusBadRequest, "UnexpectedRequest")
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}
