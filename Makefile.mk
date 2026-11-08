GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK_VERSION ?= v1.9.0
GOSEC_VERSION ?= v2.30.0
DOCKER ?= docker
OPENVOX_TEST_IMAGE ?=
DOCKER_COMPOSE ?= $(DOCKER) compose
HELM ?= helm
PROMTOOL_IMAGE ?= prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06
PLAYWRIGHT_IMAGE ?= mcr.microsoft.com/playwright:v1.65.0-noble@sha256:6e3fb709361521eb27161606e6da6d2674e5e89fa16516cb37d6fdd91372273d
TRIVY_IMAGE ?= aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb
TRIVY_CACHE_VOLUME ?= puppet-forge-trivy-cache
POSTGRES_TEST_IMAGE ?= postgres:18.6-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
S3_TEST_IMAGE ?= chrislusf/seaweedfs:4.52@sha256:57877e32bb90073b37dc109b438ba66e84fd5ed9ced5f5c04f56444ef243ce59
GCS_TEST_IMAGE ?= fsouza/fake-gcs-server:1.56.1@sha256:797ce226d62f947c009dc40246b30cfb456b8473d8241407f9d6f2c04e4d69ef
INTEGRATION_RUNNER_IMAGE ?= golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e
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
