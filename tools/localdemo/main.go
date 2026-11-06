// Command localdemo runs a fake Cloud Storage + Cloud Run endpoint on your
// machine so you can try the provider with a real `terraform plan/apply`
// and no Google Cloud project. Its one job runs the real runner, which runs
// the Flyway CLI against a local SQLite file.
//
//	go run ./tools/localdemo -flyway /path/to/flyway -db /tmp/demo.sqlite
//
// It prints the provider block to use. Local testing only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"google.golang.org/api/option"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/fakegcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/runner"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	fw := flag.String("flyway", "flyway", "Flyway executable")
	db := flag.String("db", "demo.sqlite", "SQLite database file")
	flag.Parse()

	srv, err := fakegcp.NewAt(*addr)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	store, err := gcp.NewStorage(context.Background(), option.WithEndpoint(srv.StorageEndpoint()), option.WithoutAuthentication())
	if err != nil {
		log.Fatal(err)
	}
	base := map[string]string{
		runner.EnvBucket: "demo-bucket", runner.EnvPrefix: "demo/dev",
		runner.EnvDBURL: "jdbc:sqlite:" + *db, runner.EnvFlyway: *fw,
	}
	srv.AddJob("projects/demo/locations/us-central1/jobs/migrate", func(ctx context.Context, overrides map[string]string) error {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range overrides {
			env[k] = v
		}
		cfg, err := runner.ConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			return err
		}
		r := &runner.Runner{Config: cfg, Store: store, Log: runner.NewLogger(os.Stdout), Stderr: os.Stderr}
		if code := r.Run(ctx); code != 0 {
			return fmt.Errorf("runner exited %d", code)
		}
		return nil
	})

	fmt.Printf(`Fake GCP listening on %s. Use:

provider "sqlmigrate" {
  project          = "demo"
  region           = "us-central1"
  access_token     = "local-demo"
  storage_endpoint = %q
  run_endpoint     = %q
  poll_interval    = "200ms"
}

resource "sqlmigrate_migrations" "demo" {
  source_dir = "${path.module}/migrations"
  bucket     = "demo-bucket"
  prefix     = "demo/dev"
  job        = "migrate"
}

Ctrl-C to stop.
`, srv.URL, srv.StorageEndpoint(), srv.RunEndpoint())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
