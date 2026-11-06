// Package fakegcp is an in-process stand-in for the parts of the Cloud
// Storage JSON API and Cloud Run Admin v2 API the provider and runner call.
// It exists for tests only.
package fakegcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// JobFunc runs a fake job execution with the overridden environment. A nil
// error marks the execution succeeded.
type JobFunc func(ctx context.Context, env map[string]string) error

// Server is a fake GCS + Cloud Run endpoint.
type Server struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string][]byte // "bucket/object"
	jobs    map[string]JobFunc
	ops     map[string]*operation
	execs   map[string]*execution
	seq     int
	// Runs records the environment of every job run, in order.
	Runs []map[string]string
}

type operation struct {
	name string
	exec string
	done bool
	err  string
}

type execution struct {
	name      string
	succeeded bool
	failed    bool
	message   string
	done      time.Time
}

// New starts a Server on a random local port.
func New() *Server {
	s := newServer()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewAt starts a Server on addr, for the local demo.
func NewAt(addr string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer()
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Server.Listener.Close()
	s.Server.Listener = l
	s.Server.Start()
	return s, nil
}

func newServer() *Server {
	return &Server{
		objects: map[string][]byte{},
		jobs:    map[string]JobFunc{},
		ops:     map[string]*operation{},
		execs:   map[string]*execution{},
	}
}

// StorageEndpoint is the value for the provider's storage_endpoint.
func (s *Server) StorageEndpoint() string { return s.URL + "/storage/v1/" }

// RunEndpoint is the value for the provider's run_endpoint.
func (s *Server) RunEndpoint() string { return s.URL + "/" }

// AddJob registers a job by its full name.
func (s *Server) AddJob(name string, fn JobFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = fn
}

// Object returns a stored object.
func (s *Server) Object(bucket, name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[bucket+"/"+name]
	return b, ok
}

// PutObject stores an object.
func (s *Server) PutObject(bucket, name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+name] = append([]byte(nil), data...)
}

// RunCount is the number of job runs started so far.
func (s *Server) RunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Runs)
}

// DeleteObject removes an object.
func (s *Server) DeleteObject(bucket, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, bucket+"/"+name)
}

// ObjectNames lists stored object names with a prefix.
func (s *Server) ObjectNames(bucket, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if name, ok := strings.CutPrefix(k, bucket+"/"); ok && strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(p, "/storage/v1/b/") && r.Method == http.MethodGet:
		s.getObject(w, r, strings.TrimPrefix(p, "/storage/v1/b/"))
	case strings.HasPrefix(p, "/upload/storage/v1/b/") && r.Method == http.MethodPost:
		s.upload(w, r, strings.TrimPrefix(p, "/upload/storage/v1/b/"))
	case strings.HasPrefix(p, "/v2/") && strings.HasSuffix(p, ":run") && r.Method == http.MethodPost:
		s.runJob(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/v2/"), ":run"))
	case strings.HasPrefix(p, "/v2/") && strings.Contains(p, "/operations/") && r.Method == http.MethodGet:
		s.getOperation(w, strings.TrimPrefix(p, "/v2/"))
	case strings.HasPrefix(p, "/v2/") && strings.Contains(p, "/executions/") && r.Method == http.MethodGet:
		s.getExecution(w, strings.TrimPrefix(p, "/v2/"))
	default:
		apiError(w, http.StatusNotFound, "fakegcp: no handler for "+r.Method+" "+p)
	}
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// getObject handles "<bucket>/o/<escaped object>".
func (s *Server) getObject(w http.ResponseWriter, r *http.Request, rest string) {
	bucket, escaped, ok := strings.Cut(rest, "/o/")
	if !ok {
		apiError(w, http.StatusNotFound, "bad object path")
		return
	}
	name, err := url.PathUnescape(escaped)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, found := s.Object(bucket, name)
	if !found {
		apiError(w, http.StatusNotFound, "No such object: "+bucket+"/"+name)
		return
	}
	if r.URL.Query().Get("alt") == "media" {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, map[string]any{"bucket": bucket, "name": name, "size": fmt.Sprint(len(data))})
}

// upload handles "<bucket>/o" with uploadType=multipart or media.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, rest string) {
	bucket := strings.TrimSuffix(rest, "/o")
	var name string
	var data []byte
	switch r.URL.Query().Get("uploadType") {
	case "media":
		name = r.URL.Query().Get("name")
		data, _ = io.ReadAll(r.Body)
	case "multipart":
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mt, "multipart/") {
			apiError(w, http.StatusBadRequest, "expected multipart body")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		meta, err := mr.NextPart()
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(meta).Decode(&obj); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		media, err := mr.NextPart()
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		name = obj.Name
		data, _ = io.ReadAll(media)
	default:
		apiError(w, http.StatusBadRequest, "unsupported uploadType")
		return
	}
	if name == "" {
		apiError(w, http.StatusBadRequest, "missing object name")
		return
	}
	s.PutObject(bucket, name, data)
	writeJSON(w, map[string]any{"bucket": bucket, "name": name, "size": fmt.Sprint(len(data))})
}

func (s *Server) runJob(w http.ResponseWriter, r *http.Request, job string) {
	s.mu.Lock()
	fn, ok := s.jobs[job]
	s.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "Resource '"+job+"' was not found")
		return
	}
	var req struct {
		Overrides struct {
			ContainerOverrides []struct {
				Env []struct{ Name, Value string } `json:"env"`
			} `json:"containerOverrides"`
		} `json:"overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	env := map[string]string{}
	for _, co := range req.Overrides.ContainerOverrides {
		for _, e := range co.Env {
			env[e.Name] = e.Value
		}
	}
	parts := strings.Split(job, "/") // projects/P/locations/L/jobs/J
	if len(parts) != 6 {
		apiError(w, http.StatusBadRequest, "bad job name")
		return
	}

	s.mu.Lock()
	s.seq++
	opName := fmt.Sprintf("projects/%s/locations/%s/operations/op-%d", parts[1], parts[3], s.seq)
	execName := fmt.Sprintf("%s/executions/%s-%d", job, parts[5], s.seq)
	op := &operation{name: opName, exec: execName}
	ex := &execution{name: execName}
	s.ops[opName] = op
	s.execs[execName] = ex
	s.Runs = append(s.Runs, env)
	s.mu.Unlock()

	go func() {
		err := fn(context.Background(), env)
		s.mu.Lock()
		defer s.mu.Unlock()
		ex.done = time.Now()
		if err != nil {
			ex.failed = true
			ex.message = err.Error()
		} else {
			ex.succeeded = true
		}
		op.done = true
	}()

	writeJSON(w, map[string]any{
		"name":     opName,
		"metadata": map[string]any{"@type": "type.googleapis.com/google.cloud.run.v2.Execution", "name": execName},
	})
}

func (s *Server) getOperation(w http.ResponseWriter, name string) {
	s.mu.Lock()
	op, ok := s.ops[name]
	var body map[string]any
	if ok {
		body = map[string]any{"name": op.name, "done": op.done,
			"metadata": map[string]any{"@type": "type.googleapis.com/google.cloud.run.v2.Execution", "name": op.exec}}
		if op.done {
			body["response"] = s.execJSON(s.execs[op.exec])
		}
	}
	s.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "operation not found")
		return
	}
	writeJSON(w, body)
}

func (s *Server) getExecution(w http.ResponseWriter, name string) {
	s.mu.Lock()
	ex, ok := s.execs[name]
	var body map[string]any
	if ok {
		body = s.execJSON(ex)
	}
	s.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "execution not found")
		return
	}
	writeJSON(w, body)
}

func (s *Server) execJSON(ex *execution) map[string]any {
	body := map[string]any{"@type": "type.googleapis.com/google.cloud.run.v2.Execution", "name": ex.name,
		"logUri": "https://console.cloud.google.com/logs/fake?execution=" + url.QueryEscape(ex.name)}
	if ex.succeeded {
		body["succeededCount"] = 1
	}
	if ex.failed {
		body["failedCount"] = 1
		body["conditions"] = []map[string]any{{"type": "Completed", "state": "CONDITION_FAILED", "message": "Task failed: " + ex.message}}
	}
	if !ex.done.IsZero() {
		body["completionTime"] = ex.done.UTC().Format(time.RFC3339Nano)
	}
	return body
}
