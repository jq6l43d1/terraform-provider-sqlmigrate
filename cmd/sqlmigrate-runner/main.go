// Command sqlmigrate-runner runs inside the Cloud Run job. See the README for
// the environment variables it reads.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/api/option"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/runner"
)

func main() { os.Exit(run()) }

func run() int {
	log := runner.NewLogger(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	cfg, err := runner.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	opts := []option.ClientOption{option.WithUserAgent("sqlmigrate-runner")}
	if ep := os.Getenv(runner.EnvStorageEndpoint); ep != "" {
		opts = append(opts, option.WithEndpoint(ep))
	}
	if os.Getenv(runner.EnvInsecureNoAuth) == "true" {
		// Only for tests against a local fake; never set this on a real job.
		opts = append(opts, option.WithoutAuthentication())
	}
	store, err := gcp.NewStorage(ctx, opts...)
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	r := &runner.Runner{Config: cfg, Store: store, Log: log, Stderr: os.Stderr}
	return r.Run(ctx)
}
