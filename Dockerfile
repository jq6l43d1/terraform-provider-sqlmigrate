# Runner image for the Cloud Run job: the sqlmigrate-runner binary plus the
# open-source Flyway CLI (Apache 2.0), which ships its own Java runtime and
# JDBC drivers for PostgreSQL, MySQL/MariaDB, SQL Server, Oracle and others.
#
#   docker build -t REGION-docker.pkg.dev/PROJECT/REPO/sqlmigrate-runner:VERSION .
#
# Push it to Artifact Registry and pin the job to the digest.

ARG GO_VERSION=1.27.3
ARG FLYWAY_VERSION=13.10.3
# sha256 of flyway-commandline-${FLYWAY_VERSION}-linux-x64.tar.gz, as published
# on the GitHub release. Update both together.
ARG FLYWAY_SHA256=2cfe30cae441fff96a4dbd4644bfd2f966dab0cc7af0e5ed5fe87098b1ae36b5

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sqlmigrate-runner ./cmd/sqlmigrate-runner

FROM debian:trixie-slim AS flyway
ARG FLYWAY_VERSION
ARG FLYWAY_SHA256
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/flyway.tar.gz \
      "https://github.com/flyway/flyway/releases/download/flyway-${FLYWAY_VERSION}/flyway-commandline-${FLYWAY_VERSION}-linux-x64.tar.gz" \
 && echo "${FLYWAY_SHA256}  /tmp/flyway.tar.gz" | sha256sum -c - \
 && mkdir -p /opt/flyway \
 && tar -xzf /tmp/flyway.tar.gz -C /opt/flyway --strip-components=1 \
 && rm /tmp/flyway.tar.gz

FROM debian:trixie-slim
COPY --from=flyway /opt/flyway /opt/flyway
COPY --from=build /out/sqlmigrate-runner /usr/local/bin/sqlmigrate-runner
# Flyway's launcher is a bash script; the slim image has bash. Run as a
# non-root user with /tmp as home (Cloud Run's /tmp is writable).
ENV SQLMIGRATE_FLYWAY=/opt/flyway/flyway \
    HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sqlmigrate-runner"]
