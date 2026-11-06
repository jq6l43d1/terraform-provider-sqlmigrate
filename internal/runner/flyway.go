package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
)

// flywayOutput is the union of the JSON shapes Flyway prints with
// -outputType=json. migrate and info use "success"/"error"; validate uses
// "validationSuccessful"/"errorDetails".
type flywayOutput struct {
	Operation            string            `json:"operation"`
	FlywayVersion        string            `json:"flywayVersion"`
	DatabaseType         string            `json:"databaseType"`
	SchemaVersion        *string           `json:"schemaVersion"`
	TargetSchemaVersion  *string           `json:"targetSchemaVersion"`
	Success              *bool             `json:"success"`
	ValidationSuccessful *bool             `json:"validationSuccessful"`
	Migrations           []flywayMigration `json:"migrations"`
	Warnings             []json.RawMessage `json:"warnings"`
	Error                *flywayError      `json:"error"`
	ErrorDetails         *struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"errorDetails"`
	AllErrorMessages string `json:"allErrorMessages"`
}

type flywayMigration struct {
	Category       string `json:"category"`
	Version        string `json:"version"`
	Description    string `json:"description"`
	Type           string `json:"type"`
	State          string `json:"state"`
	InstalledOnUTC string `json:"installedOnUTC"`
	Filepath       string `json:"filepath"`
	ExecutionTime  int64  `json:"executionTime"`
}

type flywayError struct {
	ErrorCode  string `json:"errorCode"`
	Message    string `json:"message"`
	LineNumber int    `json:"lineNumber"`
	Path       string `json:"path"`
}

// result is one Flyway invocation.
type result struct {
	out    *flywayOutput
	ok     bool
	err    *manifest.RunError
	stderr string
}

const maxMessage = 8000

// flyway runs the Flyway CLI.
type flyway struct {
	path         string
	locationDir  string
	placeholders map[string]string
	env          []string
	stderr       io.Writer
}

func (f *flyway) run(ctx context.Context, command string) result {
	args := []string{"-skipCheckForUpdate", "-outputType=json", "-locations=filesystem:" + f.locationDir}
	for _, k := range sortedKeys(f.placeholders) {
		args = append(args, "-placeholders."+k+"="+f.placeholders[k])
	}
	args = append(args, command)

	cmd := exec.CommandContext(ctx, f.path, args...)
	cmd.Env = f.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, f.stderr)
	runErr := cmd.Run()

	res := result{stderr: tail(stderr.String(), 2000)}
	var out flywayOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		msg := "Flyway did not print JSON output"
		if runErr != nil {
			msg = fmt.Sprintf("running Flyway: %v", runErr)
			var ee *exec.Error
			if errors.As(runErr, &ee) {
				msg = fmt.Sprintf("cannot start Flyway at %q: %v", f.path, ee.Err)
			}
		}
		if res.stderr != "" {
			msg += "\n" + res.stderr
		} else if s := strings.TrimSpace(stdout.String()); s != "" {
			msg += "\n" + tail(s, 2000)
		}
		res.err = &manifest.RunError{Code: "RUNNER_FLYWAY_OUTPUT", Message: f.clean(msg)}
		return res
	}
	res.out = &out

	switch {
	case out.Error != nil:
		res.err = &manifest.RunError{
			Code:    out.Error.ErrorCode,
			Message: f.clean(out.Error.Message),
			Script:  f.rel(out.Error.Path),
			Line:    out.Error.LineNumber,
		}
	case out.ErrorDetails != nil:
		msg := out.AllErrorMessages
		if msg == "" {
			msg = out.ErrorDetails.ErrorMessage
		}
		res.err = &manifest.RunError{Code: out.ErrorDetails.ErrorCode, Message: f.clean(msg)}
	case out.Success != nil && !*out.Success, out.ValidationSuccessful != nil && !*out.ValidationSuccessful:
		res.err = &manifest.RunError{Code: "FLYWAY_FAILED", Message: "Flyway reported failure without an error message"}
	case runErr != nil:
		res.err = &manifest.RunError{Code: "FLYWAY_EXIT", Message: f.clean(fmt.Sprintf("Flyway exited with %v\n%s", runErr, res.stderr))}
	}
	res.ok = res.err == nil
	return res
}

// rel turns an absolute path inside the bundle into the bundle-relative path.
func (f *flyway) rel(p string) string {
	if p == "" {
		return ""
	}
	r, err := filepath.Rel(f.locationDir, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return ""
	}
	return filepath.ToSlash(r)
}

// clean removes the temporary bundle path from messages and caps their size.
func (f *flyway) clean(msg string) string {
	msg = strings.ReplaceAll(msg, f.locationDir+string(filepath.Separator), "")
	msg = strings.ReplaceAll(msg, f.locationDir, "<bundle>")
	if i := strings.Index(msg, "\n\tat "); i > 0 {
		msg = msg[:i] // drop Java stack traces
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "\n[truncated]"
	}
	return strings.TrimSpace(msg)
}

func (o *flywayOutput) warnings() []string {
	var out []string
	for _, w := range o.Warnings {
		var s string
		if json.Unmarshal(w, &s) == nil {
			out = append(out, s)
		} else {
			out = append(out, string(w))
		}
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
