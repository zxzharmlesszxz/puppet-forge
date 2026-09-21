GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK_VERSION ?= v1.6.0
GOSEC_VERSION ?= v2.28.0
DOCKER ?= docker
DOCKER_COMPOSE ?= $(DOCKER) compose
HELM ?= helm
PROMTOOL_IMAGE ?= prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0
PLAYWRIGHT_IMAGE ?= mcr.microsoft.com/playwright:v1.62.1-noble@sha256:dcc5531e97840b9b5e794f2814476b21571c5124a3fca2267d73041f56e7580e
TRIVY_IMAGE ?= aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969
TRIVY_CACHE_VOLUME ?= puppet-forge-trivy-cache
POSTGRES_TEST_IMAGE ?= postgres:18.6-alpine
MINIO_TEST_IMAGE ?= quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e
GCS_TEST_IMAGE ?= fsouza/fake-gcs-server:1.56.1@sha256:797ce226d62f947c009dc40246b30cfb456b8473d8241407f9d6f2c04e4d69ef
INTEGRATION_RUNNER_IMAGE ?= golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244
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
PUPPET_FORGE_TEST_S3_ENDPOINT ?= http://localhost:9000
PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID ?= minioadmin
PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY ?= minioadmin
PUPPET_FORGE_TEST_GCS_ENDPOINT ?= http://localhost:4443
