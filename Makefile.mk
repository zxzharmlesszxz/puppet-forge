GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK_VERSION ?= v1.6.0
GOSEC_VERSION ?= v2.28.0
DOCKER ?= docker
DOCKER_COMPOSE ?= $(DOCKER) compose
HELM ?= helm
PROMTOOL_IMAGE ?= prom/prometheus:v3.13.2@sha256:508729e0e2d18e11fd742a5a5ca70e557b940a93948c3c95fd0123a6fd538b69
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
COVERAGE_THRESHOLD ?= 60.0
GO_FILES ?= $(shell find . -name '*.go' -not -path './vendor/*' -not -path './dist/*')

COMPOSE_ARGS ?= up --build
OIDC_PREFLIGHT_URL ?= http://forge.127.0.0.1.nip.io:8080
OIDC_PREFLIGHT_REDIRECT_URL ?= $(OIDC_PREFLIGHT_URL)/auth/callback
PUPPET_FORGE_TEST_POSTGRES_DSN ?= postgres://forge:forge@localhost:5432/forge?sslmode=disable
PUPPET_FORGE_TEST_S3_ENDPOINT ?= http://localhost:9000
PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID ?= minioadmin
PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY ?= minioadmin
PUPPET_FORGE_TEST_GCS_ENDPOINT ?= http://localhost:4443
