GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK_VERSION ?= v1.9.0
GOSEC_VERSION ?= 14ef84f6feccf40be7f5092fd2db119bf3f5f836
DOCKER ?= docker
DOCKER_COMPOSE ?= $(DOCKER) compose
HELM ?= helm
PROMTOOL_IMAGE ?= prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e
PLAYWRIGHT_IMAGE ?= mcr.microsoft.com/playwright:v1.64.1-noble@sha256:7852dc5a7d72f5fe458a597d690f6c42fbc8e9e26ddfa95f8e6ef552558d9bbf
TRIVY_IMAGE ?= aquasec/trivy:0.76.0@sha256:2416d1f74045b57274df1a340eee6b510cacd73c66a6ccb8da092b117eb7d222
TRIVY_CACHE_VOLUME ?= puppet-forge-trivy-cache
POSTGRES_TEST_IMAGE ?= postgres:18.6-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
S3_TEST_IMAGE ?= chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
GCS_TEST_IMAGE ?= fsouza/fake-gcs-server:1.56.1@sha256:797ce226d62f947c009dc40246b30cfb456b8473d8241407f9d6f2c04e4d69ef
INTEGRATION_RUNNER_IMAGE ?= golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8
CURL ?= curl
PYTHON ?= python3

PROJECT_NAME ?= puppet-forge
MAIN_PACKAGE ?= ./cmd/server
DIST_DIR ?= dist
BUILD_OUTPUT ?= $(DIST_DIR)/$(PROJECT_NAME)
CHART_DIR ?= deploy/puppet-forge
CHART_DIST_DIR ?= $(DIST_DIR)/charts
CHART_VERSION ?=
APP_VERSION ?=
CGO_ENABLED ?= 0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VCS_REF ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
VERSION_LDFLAGS ?= -X main.version=$(VERSION)
LDFLAGS ?= -s -w $(VERSION_LDFLAGS)
IMAGE_TAG ?= $(VERSION)
DOCKER_IMAGE ?= $(PROJECT_NAME):$(IMAGE_TAG)
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

COVERAGE_PROFILE ?= coverage.out
COVERAGE_REPORT ?= coverage.txt
COVERAGE_UNIT_PROFILE ?= coverage-unit.out
COVERAGE_POSTGRES_PROFILE ?= coverage-postgres.out
COVERAGE_S3_PROFILE ?= coverage-s3.out
COVERAGE_GCS_PROFILE ?= coverage-gcs.out
COVERAGE_PROFILES ?= $(COVERAGE_UNIT_PROFILE) $(COVERAGE_POSTGRES_PROFILE) $(COVERAGE_S3_PROFILE) $(COVERAGE_GCS_PROFILE)
COVERAGE_UNIT_THRESHOLD ?= 65.0
COVERAGE_THRESHOLD ?= 70.0
COVERAGE_PACKAGE_THRESHOLDS ?= internal/auth=80.0 internal/config=90.0 internal/domain=85.0 internal/httpapi=75.0 internal/httputil=90.0 internal/metrics=70.0 internal/observability=80.0 internal/proxy=70.0 internal/service=65.0 internal/throttle=90.0 internal/webauth=75.0
GO_FILES ?= $(shell find . -name '*.go' -not -path './vendor/*' -not -path './dist/*')

COMPOSE_ARGS ?= up --build
OIDC_PREFLIGHT_URL ?= http://forge.127.0.0.1.nip.io:8080
OIDC_PREFLIGHT_REDIRECT_URL ?= $(OIDC_PREFLIGHT_URL)/auth/callback
PUPPET_FORGE_TEST_POSTGRES_DSN ?= postgres://forge:forge@localhost:5432/forge?sslmode=disable
PUPPET_FORGE_TEST_S3_ENDPOINT ?= http://localhost:8333
PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID ?= forge-local
PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY ?= forge-local-secret
PUPPET_FORGE_TEST_GCS_ENDPOINT ?= http://localhost:4443
