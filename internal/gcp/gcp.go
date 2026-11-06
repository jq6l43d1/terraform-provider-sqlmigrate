// Package gcp wraps the two Google APIs the provider and runner use: Cloud
// Storage (JSON API) and Cloud Run Admin v2. Both are REST clients, so the
// endpoints can point at Private Google Access names or a test server.
package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v2"
	storage "google.golang.org/api/storage/v1"
)

// maxObjectBytes caps downloads so a stray large object cannot exhaust
// memory.
const maxObjectBytes = 64 << 20

// Storage reads and writes small objects.
type Storage struct{ svc *storage.Service }

// NewStorage builds a Storage client.
func NewStorage(ctx context.Context, opts ...option.ClientOption) (*Storage, error) {
	svc, err := storage.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating Cloud Storage client: %w", err)
	}
	return &Storage{svc: svc}, nil
}

// Get returns an object's contents. found is false when the object (or the
// bucket) does not exist.
func (s *Storage) Get(ctx context.Context, bucket, object string) (data []byte, found bool, err error) {
	resp, err := s.svc.Objects.Get(bucket, object).Context(ctx).Download()
	if err != nil {
		if IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading gs://%s/%s: %w", bucket, object, err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxObjectBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("reading gs://%s/%s: %w", bucket, object, err)
	}
	if len(data) > maxObjectBytes {
		return nil, false, fmt.Errorf("gs://%s/%s is larger than %d bytes", bucket, object, maxObjectBytes)
	}
	return data, true, nil
}

// Exists reports whether an object exists.
func (s *Storage) Exists(ctx context.Context, bucket, object string) (bool, error) {
	_, err := s.svc.Objects.Get(bucket, object).Fields("name").Context(ctx).Do()
	if err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking gs://%s/%s: %w", bucket, object, err)
	}
	return true, nil
}

// Put writes an object, replacing any existing one.
func (s *Storage) Put(ctx context.Context, bucket, object string, data []byte, contentType string) error {
	obj := &storage.Object{Name: object, ContentType: contentType}
	_, err := s.svc.Objects.Insert(bucket, obj).
		Media(bytes.NewReader(data), googleapi.ContentType(contentType)).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("writing gs://%s/%s: %w", bucket, object, err)
	}
	return nil
}

// IsNotFound reports whether err is an HTTP 404 from a Google API.
func IsNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// Run starts Cloud Run jobs and waits for them.
type Run struct{ svc *run.Service }

// NewRun builds a Run client.
func NewRun(ctx context.Context, opts ...option.ClientOption) (*Run, error) {
	svc, err := run.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating Cloud Run client: %w", err)
	}
	return &Run{svc: svc}, nil
}

// Execution is the outcome of a job run.
type Execution struct {
	Name      string
	LogURI    string
	Succeeded bool
	Message   string // why it failed, when it failed
}

// RunJob starts one execution of job with env added to (or replacing) the
// container's environment, and returns the operation name and the execution
// name. job is "projects/P/locations/L/jobs/J". The caller needs
// run.jobs.runWithOverrides on the job.
func (r *Run) RunJob(ctx context.Context, job, container string, env map[string]string) (op, execution string, err error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	override := &run.GoogleCloudRunV2ContainerOverride{Name: container}
	for _, k := range keys {
		override.Env = append(override.Env, &run.GoogleCloudRunV2EnvVar{Name: k, Value: env[k]})
	}
	req := &run.GoogleCloudRunV2RunJobRequest{Overrides: &run.GoogleCloudRunV2Overrides{
		ContainerOverrides: []*run.GoogleCloudRunV2ContainerOverride{override},
		TaskCount:          1,
	}}
	o, err := r.svc.Projects.Locations.Jobs.Run(job, req).Context(ctx).Do()
	if err != nil {
		return "", "", fmt.Errorf("starting Cloud Run job %s: %w", job, err)
	}
	var meta struct {
		Name string `json:"name"`
	}
	if len(o.Metadata) > 0 {
		_ = json.Unmarshal(o.Metadata, &meta)
	}
	return o.Name, meta.Name, nil
}

// Wait polls the execution until it completes or ctx ends. Polling the
// execution rather than the operation means job-level roles are enough
// (run.executions.get comes with roles/run.viewer on the job). It falls back
// to the operation when the execution name is unknown.
func (r *Run) Wait(ctx context.Context, op, execution string, poll time.Duration) (*Execution, error) {
	if poll <= 0 {
		poll = 5 * time.Second
	}
	if execution == "" {
		return r.waitOperation(ctx, op, poll)
	}
	notFound := 0
	for {
		ex, err := r.svc.Projects.Locations.Jobs.Executions.Get(execution).Context(ctx).Do()
		switch {
		case err == nil && ex.CompletionTime != "":
			return summarize(ex, nil), nil
		case err == nil:
		case IsNotFound(err) && notFound < 5:
			notFound++ // a just-created execution can take a moment to appear
		case ctx.Err() != nil:
			return nil, fmt.Errorf("timed out waiting for %s; it may still be running: %w", execution, ctx.Err())
		default:
			return nil, fmt.Errorf("checking %s: %w", execution, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for %s; it may still be running: %w", execution, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func (r *Run) waitOperation(ctx context.Context, op string, poll time.Duration) (*Execution, error) {
	for {
		o, err := r.svc.Projects.Locations.Operations.Get(op).Context(ctx).Do()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("timed out waiting for %s: %w", op, ctx.Err())
			}
			return nil, fmt.Errorf("polling %s: %w", op, err)
		}
		if o.Done {
			var ex run.GoogleCloudRunV2Execution
			if len(o.Response) > 0 {
				_ = json.Unmarshal(o.Response, &ex)
			}
			return summarize(&ex, o.Error), nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for %s: %w", op, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func summarize(ex *run.GoogleCloudRunV2Execution, opErr *run.GoogleRpcStatus) *Execution {
	out := &Execution{Name: ex.Name, LogURI: ex.LogUri}
	switch {
	case opErr != nil:
		out.Message = opErr.Message
	case ex.SucceededCount >= 1 && ex.FailedCount == 0 && ex.CancelledCount == 0:
		out.Succeeded = true
	case ex.CancelledCount > 0:
		out.Message = "the execution was cancelled"
	default:
		out.Message = "the execution failed"
		for _, c := range ex.Conditions {
			if c.Type == "Completed" && c.Message != "" {
				out.Message = c.Message
			}
		}
	}
	return out
}
