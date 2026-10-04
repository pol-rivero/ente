// Package fakes3 is an in-memory S3 server. Objects created with PutObject
// only have a size and an ETag; objects with data can also be read, and are
// written by PUTs and uploaded parts.
package fakes3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
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
	"time"
)

const Bucket = "test-bucket"

type Op string

const (
	OpHead     Op = "HeadObject"
	OpCopy     Op = "CopyObject"
	OpDelete   Op = "DeleteObject"
	OpCreate   Op = "CreateMultipartUpload"
	OpPartCopy Op = "UploadPartCopy"
	OpComplete Op = "CompleteMultipartUpload"
	OpAbort    Op = "AbortMultipartUpload"
	OpList     Op = "ListMultipartUploads"
	OpListPart Op = "ListParts"
	OpGet      Op = "GetObject"
	OpPut      Op = "PutObject"
	OpUpload   Op = "UploadPart"
)

const MinPartSize = int64(5) << 20

type Request struct {
	Op         Op
	Key        string
	UploadID   string
	PartNumber int64
	CopySource string
	// The copy source range, or the Range header of a GET.
	Range        string
	StorageClass string
	ContentMD5   string
	Parts        []int64
	// Closed when the client gives up on the request.
	Done <-chan struct{}
}

type Failure struct {
	Status int
	Code   string
}

type Object struct {
	Size         int64
	ETag         string
	Data         []byte
	StorageClass string
}

type Part struct {
	Start, End int64
	ETag       string
	Source     string
	SourceSize int64
	// Set for uploaded (not copied) parts.
	Data []byte
}

type Upload struct {
	Key          string
	Parts        map[int64]Part
	Initiated    time.Time
	StorageClass string
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
	// Stalled GETs send their headers and half the body, then hang.
	stall           func(Request) bool
	pageSize        int
	omitNextMarkers bool
	quirks          ListQuirks
}

// Quirks of S3-compatible stores' truncated listings.
type ListQuirks struct {
	// ListParts reports every page as truncated.
	AlwaysTruncatedParts bool
	// Upload listings leave out NextUploadIdMarker.
	OmitNextUploadIDMarker bool
	// Upload listings repeat the request's markers as the next ones.
	EchoUploadMarkers bool
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

func (s *Server) SetStall(stall func(Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = stall
}

// Limits ListParts and ListMultipartUploads pages; omitNextMarkers mimics
// stores that leave out the next markers of truncated listings.
func (s *Server) SetPageSize(pageSize int, omitNextMarkers bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize, s.omitNextMarkers = pageSize, omitNextMarkers
}

func (s *Server) SetListQuirks(quirks ListQuirks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quirks = quirks
}

func (s *Server) PutObject(key string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = Object{Size: size, ETag: fmt.Sprintf(`"etag-%s"`, key)}
}

func (s *Server) PutObjectData(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = Object{Size: int64(len(data)), ETag: md5ETag(data), Data: data}
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
		uploads[id] = Upload{Key: upload.Key, Parts: maps.Clone(upload.Parts), Initiated: upload.Initiated, StorageClass: upload.StorageClass}
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

func md5ETag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/"+Bucket+"/"))
	if err != nil {
		s.fail(w, "bad path %s", r.URL)
		return
	}
	query := r.URL.Query()
	req := Request{
		Key:          key,
		UploadID:     query.Get("uploadId"),
		CopySource:   r.Header.Get("X-Amz-Copy-Source"),
		Range:        r.Header.Get("X-Amz-Copy-Source-Range"),
		StorageClass: r.Header.Get("X-Amz-Storage-Class"),
		ContentMD5:   r.Header.Get("Content-Md5"),
		Done:         r.Context().Done(),
	}
	req.PartNumber, _ = strconv.ParseInt(query.Get("partNumber"), 10, 64)
	var completeBody completeMultipartUpload
	var body []byte
	switch {
	case r.Method == http.MethodHead:
		req.Op = OpHead
	case r.Method == http.MethodPost && query.Has("uploads"):
		req.Op = OpCreate
	case r.Method == http.MethodPut && req.UploadID != "" && req.CopySource != "":
		req.Op = OpPartCopy
	case r.Method == http.MethodPut && req.UploadID != "":
		req.Op = OpUpload
		body, _ = io.ReadAll(r.Body)
	case r.Method == http.MethodPut && req.CopySource != "":
		req.Op = OpCopy
	case r.Method == http.MethodPut:
		req.Op = OpPut
		body, _ = io.ReadAll(r.Body)
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
	case r.Method == http.MethodDelete:
		req.Op = OpDelete
	case r.Method == http.MethodGet && query.Has("uploads"):
		req.Op = OpList
		req.Key = query.Get("prefix")
	case r.Method == http.MethodGet && req.UploadID != "":
		req.Op = OpListPart
	case r.Method == http.MethodGet:
		req.Op = OpGet
		req.Range = r.Header.Get("Range")
	default:
		s.fail(w, "unexpected request %s %s", r.Method, r.URL)
		return
	}
	if query.Has("uploadId") && req.UploadID == "" {
		s.fail(w, "empty upload ID: %s %s", r.Method, r.URL)
		return
	}

	if (req.Op == OpUpload || req.Op == OpPut) && req.ContentMD5 != "" {
		sum := md5.Sum(body)
		if req.ContentMD5 != base64.StdEncoding.EncodeToString(sum[:]) {
			writeError(w, http.StatusBadRequest, "BadDigest")
			return
		}
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	hook, stall := s.hook, s.stall
	s.mu.Unlock()
	if hook != nil {
		if failure := hook(req); failure != nil {
			writeError(w, failure.Status, failure.Code)
			return
		}
	}
	if req.Op == OpGet && stall != nil && stall(req) {
		s.serveStalled(w, r, req)
		return
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
	case OpGet:
		object, ok := s.objects[key]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		data, status, ok := s.objectRange(w, object, req.Range)
		if !ok {
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", object.ETag)
		w.WriteHeader(status)
		_, _ = w.Write(data)
	case OpPut:
		s.objects[key] = Object{Size: int64(len(body)), ETag: md5ETag(body), Data: body, StorageClass: req.StorageClass}
		w.Header().Set("ETag", s.objects[key].ETag)
		w.WriteHeader(http.StatusOK)
	case OpUpload:
		upload, ok := s.upload(w, req)
		if !ok {
			return
		}
		if req.PartNumber < 1 || req.PartNumber > 10000 {
			writeError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		part := Part{End: int64(len(body)) - 1, ETag: md5ETag(body), Data: body}
		upload.Parts[req.PartNumber] = part
		w.Header().Set("ETag", part.ETag)
		w.WriteHeader(http.StatusOK)
	case OpCreate:
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.uploads[id] = &Upload{Key: key, Parts: map[int64]Part{}, Initiated: time.Now(), StorageClass: req.StorageClass}
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
		var data []byte
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
			if (part.Data == nil) != (first.Data == nil) {
				s.fail(w, "upload %s mixes copied and uploaded parts", req.UploadID)
				return
			}
			if part.Data != nil {
				data = append(data, part.Data...)
				size += int64(len(part.Data))
				continue
			}
			if part.Start != size || part.Source != first.Source || part.SourceSize != first.SourceSize {
				s.fail(w, "part %d copies %s %d-%d, not contiguous with the previous parts", listed.PartNumber, part.Source, part.Start, part.End)
				return
			}
			size += part.End - part.Start + 1
		}
		if first.Data == nil && size != first.SourceSize {
			s.fail(w, "parts cover %d bytes of a %d-byte source", size, first.SourceSize)
			return
		}
		s.objects[key] = Object{Size: size, ETag: fmt.Sprintf(`"multipart-%s"`, key), Data: data, StorageClass: upload.StorageClass}
		delete(s.uploads, req.UploadID)
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`,
			Bucket, key, s.objects[key].ETag)
	case OpDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case OpAbort:
		if _, ok := s.upload(w, req); !ok {
			return
		}
		delete(s.uploads, req.UploadID)
		w.WriteHeader(http.StatusNoContent)
	case OpList:
		var listed []string
		for _, id := range slices.SortedFunc(maps.Keys(s.uploads), s.compareUploads) {
			upload := s.uploads[id]
			if !strings.HasPrefix(upload.Key, req.Key) || !afterUploadMarker(upload.Key, id, query.Get("key-marker"), query.Get("upload-id-marker")) {
				continue
			}
			listed = append(listed, id)
		}
		shown, truncated := page(listed, query.Get("max-uploads"), s.pageSize)
		var body strings.Builder
		fmt.Fprintf(&body, `<ListMultipartUploadsResult><Bucket>%s</Bucket><IsTruncated>%t</IsTruncated>`, Bucket, truncated)
		if truncated && !s.omitNextMarkers {
			last := shown[len(shown)-1]
			nextKey, nextID := s.uploads[last].Key, last
			if s.quirks.EchoUploadMarkers {
				nextKey, nextID = query.Get("key-marker"), query.Get("upload-id-marker")
			}
			if s.quirks.OmitNextUploadIDMarker {
				nextID = ""
			}
			fmt.Fprintf(&body, `<NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>%s</NextUploadIdMarker>`, nextKey, nextID)
		}
		for _, id := range shown {
			fmt.Fprintf(&body, `<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>`,
				s.uploads[id].Key, id, s.uploads[id].Initiated.UTC().Format(time.RFC3339Nano))
		}
		body.WriteString(`</ListMultipartUploadsResult>`)
		_, _ = w.Write([]byte(body.String()))
	case OpListPart:
		upload, ok := s.upload(w, req)
		if !ok {
			return
		}
		marker, _ := strconv.ParseInt(query.Get("part-number-marker"), 10, 64)
		var numbers []int64
		for _, number := range slices.Sorted(maps.Keys(upload.Parts)) {
			if number > marker {
				numbers = append(numbers, number)
			}
		}
		shown, truncated := page(numbers, query.Get("max-parts"), s.pageSize)
		truncated = truncated || s.quirks.AlwaysTruncatedParts
		var body strings.Builder
		fmt.Fprintf(&body, `<ListPartsResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId><IsTruncated>%t</IsTruncated>`,
			Bucket, upload.Key, req.UploadID, truncated)
		if truncated && !s.omitNextMarkers && len(shown) > 0 {
			fmt.Fprintf(&body, `<NextPartNumberMarker>%d</NextPartNumberMarker>`, shown[len(shown)-1])
		}
		for _, number := range shown {
			part := upload.Parts[number]
			fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>%s</ETag><Size>%d</Size></Part>`, number, part.ETag, part.End-part.Start+1)
		}
		body.WriteString(`</ListPartsResult>`)
		_, _ = w.Write([]byte(body.String()))
	}
}

func (s *Server) compareUploads(a, b string) int {
	if c := strings.Compare(s.uploads[a].Key, s.uploads[b].Key); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func afterUploadMarker(key, id, keyMarker, uploadIDMarker string) bool {
	if keyMarker == "" {
		return true
	}
	return key > keyMarker || (key == keyMarker && uploadIDMarker != "" && id > uploadIDMarker)
}

func page[T any](items []T, maxParam string, pageSize int) ([]T, bool) {
	limit, _ := strconv.Atoi(maxParam)
	if pageSize > 0 && (limit <= 0 || pageSize < limit) {
		limit = pageSize
	}
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

func (s *Server) objectRange(w http.ResponseWriter, object Object, rangeHeader string) ([]byte, int, bool) {
	if object.Data == nil {
		s.fail(w, "GET of an object without data")
		return nil, 0, false
	}
	if rangeHeader == "" {
		return object.Data, http.StatusOK, true
	}
	match := rangePattern.FindStringSubmatch(rangeHeader)
	if match == nil {
		s.fail(w, "bad range %q", rangeHeader)
		return nil, 0, false
	}
	start, _ := strconv.ParseInt(match[1], 10, 64)
	end, _ := strconv.ParseInt(match[2], 10, 64)
	if start > end || start >= object.Size {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
		return nil, 0, false
	}
	end = min(end, object.Size-1)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, object.Size))
	return object.Data[start : end+1], http.StatusPartialContent, true
}

func (s *Server) serveStalled(w http.ResponseWriter, r *http.Request, req Request) {
	s.mu.Lock()
	object, ok := s.objects[req.Key]
	var data []byte
	var status int
	if ok {
		data, status, ok = s.objectRange(w, object, req.Range)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data[:len(data)/2])
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// Starts an upload as a process that died before recording it would have.
func (s *Server) StartUpload(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("upload-%d", s.nextID)
	s.uploads[id] = &Upload{Key: key, Parts: map[int64]Part{}, Initiated: time.Now()}
	return id
}

func (s *Server) StartUploadAt(key string, initiated time.Time) string {
	id := s.StartUpload(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads[id].Initiated = initiated
	return id
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

func (s *Server) PutPart(uploadID string, number int64, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads[uploadID].Parts[number] = Part{End: int64(len(data)) - 1, ETag: md5ETag(data), Data: data}
}

// A part with only a size, like the objects PutObject creates.
func (s *Server) PutPartSize(uploadID string, number int64, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads[uploadID].Parts[number] = Part{End: size - 1, ETag: fmt.Sprintf(`"etag-%d"`, number)}
}

// Assembles all of an upload's parts, as a client's CompleteMultipartUpload
// listing every part would.
func (s *Server) CompleteUpload(uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload := s.uploads[uploadID]
	object := Object{ETag: fmt.Sprintf(`"multipart-%s"`, upload.Key), StorageClass: upload.StorageClass}
	withData := true
	for _, number := range slices.Sorted(maps.Keys(upload.Parts)) {
		part := upload.Parts[number]
		object.Size += part.End - part.Start + 1
		object.Data = append(object.Data, part.Data...)
		withData = withData && part.Data != nil
	}
	if !withData {
		object.Data = nil
	}
	s.objects[upload.Key] = object
	delete(s.uploads, uploadID)
}

// Removes an upload the way a provider's expiry would.
func (s *Server) DropUpload(uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, uploadID)
}
