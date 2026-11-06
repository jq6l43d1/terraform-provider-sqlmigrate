VERSION   ?= 0.1.0
NAMESPACE ?= jq6l43d1
GOOS      ?= $(shell go env GOOS)
GOARCH    ?= $(shell go env GOARCH)
FLYWAY    ?= flyway

.PHONY: build runner test testacc fmt vet demo mirror clean

build: ## Build the provider into bin/
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/terraform-provider-sqlmigrate_v$(VERSION) .

runner: ## Build the runner binary into bin/ (the image builds its own)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/sqlmigrate-runner ./cmd/sqlmigrate-runner

test: ## Unit tests (no Flyway, no Terraform)
	go test ./...

testacc: ## End-to-end tests: real Terraform + real Flyway against SQLite through a fake GCP
	TF_ACC=1 CHECKPOINT_DISABLE=1 SQLMIGRATE_TEST_FLYWAY=$(FLYWAY) go test -count=1 ./...

fmt:
	gofmt -w .
	terraform fmt -recursive modules examples

vet:
	go vet ./...

demo: ## Fake GCP on 127.0.0.1:8099 for trying the provider locally
	go run ./tools/localdemo -flyway $(FLYWAY) -db $(CURDIR)/bin/demo.sqlite

mirror: ## Build a filesystem mirror in dist/mirror for linux_amd64 (see README)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
	  -o dist/mirror/registry.terraform.io/$(NAMESPACE)/sqlmigrate/$(VERSION)/linux_amd64/terraform-provider-sqlmigrate_v$(VERSION) .

clean:
	rm -rf bin dist
