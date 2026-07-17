include Makefile.mk

.PHONY: help fmt fmt-check tidy mod-download build release-archives release-checksums release vet lint test test-postgres test-race coverage coverage-check docker-build docker-buildx docker-buildx-push docker-push compose compose-up compose-down compose-logs compose-ps compose-config compose-smoke r10k r10k-logs http-smoke oidc-preflight helm-lint helm-package check ci clean size
.SILENT: compose compose-config compose-down compose-logs compose-ps compose-up r10k r10k-logs size

help: ## Show available make targets.
	@printf "\033[33mUsage:\033[0m\n"
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "};{printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

fmt: ## Format Go files.
	$(GOFMT) -w $(GO_FILES)

fmt-check: ## Check Go formatting.
	@test -z "$$($(GOFMT) -l $(GO_FILES))"

tidy: ## Run go mod tidy.
	$(GO) mod tidy

mod-download: ## Download Go modules.
	$(GO) mod download

build: ## Build the server binary into dist/.
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_OUTPUT) $(MAIN_PACKAGE)
	$(MAKE) size

release-archives: ## Cross-build release archives into dist/.
	mkdir -p $(DIST_DIR)
	@set -e; \
	for platform in $(PLATFORMS); do \
		goos="$${platform%/*}"; \
		goarch="$${platform#*/}"; \
		archive="$(PROJECT_NAME)_$(VERSION)_$${goos}_$${goarch}"; \
		workdir="$(DIST_DIR)/$$archive"; \
		binary="$(PROJECT_NAME)"; \
		if [ "$$goos" = "windows" ]; then binary="$$binary.exe"; fi; \
		rm -rf "$$workdir"; \
		mkdir -p "$$workdir"; \
		echo "building $$archive"; \
		CGO_ENABLED=$(CGO_ENABLED) GOOS="$$goos" GOARCH="$$goarch" $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o "$$workdir/$$binary" $(MAIN_PACKAGE); \
		cp README.md METRICS.md ARCHITECTURE.md "$$workdir/"; \
		COPYFILE_DISABLE=1 tar -C $(DIST_DIR) -czf "$(DIST_DIR)/$$archive.tar.gz" "$$archive"; \
		rm -rf "$$workdir"; \
	done

release-checksums: release-archives ## Write SHA256 checksums for release archives.
	@set -e; \
	cd $(DIST_DIR); \
	if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum *.tar.gz > checksums.txt; \
	else \
		shasum -a 256 *.tar.gz > checksums.txt; \
	fi; \
	cat checksums.txt

release: clean release-checksums ## Build release archives and checksums.

release-smoke: release ## Build release archives and smoke-test the native archive.
	@set -e; \
	goos="$$( $(GO) env GOOS )"; \
	goarch="$$( $(GO) env GOARCH )"; \
	archive="$(DIST_DIR)/$(PROJECT_NAME)_$(VERSION)_$${goos}_$${goarch}.tar.gz"; \
	if [ ! -f "$$archive" ]; then \
		echo "skipping release smoke: native archive $$archive was not built"; \
		exit 0; \
	fi; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	COPYFILE_DISABLE=1 tar -C "$$tmp" -xzf "$$archive"; \
	binary="$$tmp/$(PROJECT_NAME)_$(VERSION)_$${goos}_$${goarch}/$(PROJECT_NAME)"; \
	if [ "$$goos" = "windows" ]; then binary="$$binary.exe"; fi; \
	"$$binary" --help 2>&1 | grep -F "Usage of $(PROJECT_NAME):" >/dev/null; \
	"$$binary" --version 2>&1 | grep -F "$(VERSION)" >/dev/null

vet: ## Run go vet.
	$(GO) vet ./...

lint: ## Run golangci-lint.
	PATH="$(dir $(GO)):$$PATH" $(GOLANGCI_LINT) run

test: ## Run Go tests.
	$(GO) test -buildvcs=false ./...

test-postgres: ## Run store parity tests against PostgreSQL. Override PUPPET_FORGE_TEST_POSTGRES_DSN if needed.
	@test -n "$(PUPPET_FORGE_TEST_POSTGRES_DSN)" || (echo "PUPPET_FORGE_TEST_POSTGRES_DSN is required"; exit 1)
	PUPPET_FORGE_TEST_POSTGRES_DSN="$(PUPPET_FORGE_TEST_POSTGRES_DSN)" $(GO) test -buildvcs=false ./internal/store -run 'TestStoreParity'

test-race: ## Run Go tests with the race detector.
	CGO_ENABLED=1 $(GO) test -buildvcs=false -race -ldflags "$(LDFLAGS)" ./...

coverage: ## Run tests with coverage and write coverage reports.
	$(GO) test -buildvcs=false -ldflags "$(LDFLAGS)" -covermode=atomic -coverprofile=$(COVERAGE_PROFILE) ./...
	$(GO) tool cover -func=$(COVERAGE_PROFILE) | tee $(COVERAGE_REPORT)

coverage-check: coverage ## Enforce the coverage threshold.
	@coverage="$$(awk '/^total:/ {gsub(/%/, "", $$3); print $$3}' $(COVERAGE_REPORT))"; \
	awk -v coverage="$$coverage" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN { \
		if (coverage + 0 < threshold + 0) { \
			printf "coverage %.1f%% is below %.1f%%\n", coverage, threshold; \
			exit 1; \
		} \
		printf "coverage %.1f%% meets threshold %.1f%%\n", coverage, threshold; \
	}'

docker-build: ## Build the Docker image.
	$(DOCKER) build \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		-t $(DOCKER_IMAGE) \
		.

docker-buildx: ## Build a multi-platform Docker image with buildx.
	$(DOCKER) buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		-t $(DOCKER_IMAGE) \
		.

docker-buildx-push: ## Build and push a multi-platform Docker image with buildx.
	$(DOCKER) buildx build \
		--push \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		-t $(DOCKER_IMAGE) \
		.

docker-push: ## Push the Docker image.
	$(DOCKER) push $(DOCKER_IMAGE)

compose: ## Run Docker Compose. Override COMPOSE_ARGS as needed.
	$(DOCKER_COMPOSE) $(COMPOSE_ARGS)

compose-up: ## Start the local Docker Compose stack.
	$(MAKE) compose COMPOSE_ARGS="up --build"

compose-down: ## Stop the local Docker Compose stack.
	$(MAKE) compose COMPOSE_ARGS="down --remove-orphans"

compose-logs: ## Follow Docker Compose logs.
	$(MAKE) compose COMPOSE_ARGS="logs -f"

compose-ps: ## Show Docker Compose services.
	$(MAKE) compose COMPOSE_ARGS="ps"

compose-config: ## Validate the Docker Compose config.
	$(MAKE) compose COMPOSE_ARGS="config >/dev/null"

compose-smoke: ## Start the local Compose stack and run HTTP plus r10k smoke checks.
	$(MAKE) compose COMPOSE_ARGS="up -d --build postgres minio minio-init app"
	$(MAKE) http-smoke
	$(MAKE) r10k

r10k: ## Run the r10k one-shot Compose service.
	$(MAKE) compose COMPOSE_ARGS="up --build --exit-code-from r10k r10k"

r10k-logs: ## Show r10k Compose logs.
	$(MAKE) compose COMPOSE_ARGS="logs r10k"

http-smoke: ## Smoke-test a running HTTP instance. Override SMOKE_BASE_URL/SMOKE_PUBLIC_MODULE_ACCESS/SMOKE_ADMIN_TOKEN.
	CURL="$(CURL)" scripts/http-smoke.sh "$(SMOKE_BASE_URL)"

oidc-preflight: ## Check OIDC redirect, state cookie, and token fallback. Override OIDC_PREFLIGHT_URL if needed.
	CURL="$(CURL)" PYTHON="$(PYTHON)" scripts/oidc-preflight.sh "$(OIDC_PREFLIGHT_URL)" "$(OIDC_PREFLIGHT_REDIRECT_URL)"

helm-lint: ## Lint Helm charts.
	$(HELM) lint deploy/puppet-forge

helm-package: ## Package the Helm chart into dist/charts/. Override CHART_VERSION/APP_VERSION for releases.
	mkdir -p $(CHART_DIST_DIR)
	$(HELM) package $(CHART_DIR) --destination $(CHART_DIST_DIR) $(if $(CHART_VERSION),--version $(CHART_VERSION)) $(if $(APP_VERSION),--app-version $(APP_VERSION))

check: fmt-check vet lint coverage-check compose-config helm-lint ## Run the standard local checks.

full-check: check release-smoke size ## Run all local checks and release smoke.

ci: check test-race docker-build ## Run extended checks.

clean: ## Remove generated local artifacts.
	rm -rf $(DIST_DIR)
	rm -f $(COVERAGE_PROFILE) $(COVERAGE_REPORT)

size:
	@du -h $(BUILD_OUTPUT)* 2>/dev/null || true
