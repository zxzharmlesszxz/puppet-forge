include Makefile.mk

.PHONY: help fmt fmt-check tidy mod-download build release-archives release-checksums release vet lint govulncheck gosec security-go test test-postgres test-s3-storage test-gcs-storage test-object-storage test-race test-browser coverage coverage-check docker-build docker-smoke docker-buildx docker-buildx-push docker-push compose compose-up compose-down compose-logs compose-ps compose-config compose-smoke r10k r10k-logs http-smoke oidc-preflight prometheus-rules-check helm-lint helm-template-check helm-package check ci clean size
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
		cp README.md METRICS.md ARCHITECTURE.md BACKUP_RESTORE.md "$$workdir/"; \
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

govulncheck: ## Scan reachable Go code for known vulnerabilities.
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

gosec: ## Run Go security static analysis.
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -exclude-generated ./...

security-go: govulncheck gosec ## Run Go vulnerability and security scans.

test: ## Run Go tests.
	$(GO) test -buildvcs=false ./...

test-postgres: ## Run store parity and concurrent schema tests against PostgreSQL. Override PUPPET_FORGE_TEST_POSTGRES_DSN if needed.
	@test -n "$(PUPPET_FORGE_TEST_POSTGRES_DSN)" || (echo "PUPPET_FORGE_TEST_POSTGRES_DSN is required"; exit 1)
	PUPPET_FORGE_TEST_POSTGRES_DSN="$(PUPPET_FORGE_TEST_POSTGRES_DSN)" $(GO) test -buildvcs=false ./internal/store -run 'TestStoreParity|TestPostgresStoreConcurrentSchemaSetup'

test-s3-storage: ## Run S3 lifecycle integration tests against MinIO or another S3-compatible endpoint.
	@test -n "$(PUPPET_FORGE_TEST_S3_ENDPOINT)" || (echo "PUPPET_FORGE_TEST_S3_ENDPOINT is required"; exit 1)
	PUPPET_FORGE_TEST_S3_ENDPOINT="$(PUPPET_FORGE_TEST_S3_ENDPOINT)" \
	PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID="$(PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID)" \
	PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY="$(PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY)" \
	$(GO) test -buildvcs=false ./internal/storage -run 'TestS3StorageIntegration'

test-gcs-storage: ## Run GCS lifecycle integration tests against fake-gcs-server or another emulator.
	@test -n "$(PUPPET_FORGE_TEST_GCS_ENDPOINT)" || (echo "PUPPET_FORGE_TEST_GCS_ENDPOINT is required"; exit 1)
	PUPPET_FORGE_TEST_GCS_ENDPOINT="$(PUPPET_FORGE_TEST_GCS_ENDPOINT)" \
	$(GO) test -buildvcs=false ./internal/storage -run 'TestGCSStorageIntegration'

test-object-storage: test-s3-storage test-gcs-storage ## Run S3 and GCS lifecycle integration tests.

test-race: ## Run Go tests with the race detector.
	CGO_ENABLED=1 $(GO) test -buildvcs=false -race -ldflags "$(LDFLAGS)" ./...

test-browser: ## Run browser interaction regressions with Playwright.
	npm run test:browser

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
		--build-arg VERSION="$(VERSION)" \
		--build-arg VCS_REF="$(VCS_REF)" \
		-t $(DOCKER_IMAGE) \
		.

docker-smoke: docker-build ## Build and smoke-test the Docker image user, version, help, and OCI metadata.
	@set -eu; \
	uid="$$($(DOCKER) run --rm --entrypoint id $(DOCKER_IMAGE) -u)"; \
	if [ "$$uid" != "10001" ]; then \
		echo "Docker image runs as UID $$uid, want 10001"; \
		exit 1; \
	fi; \
	gid="$$($(DOCKER) run --rm --entrypoint id $(DOCKER_IMAGE) -g)"; \
	if [ "$$gid" != "10001" ]; then \
		echo "Docker image runs as GID $$gid, want 10001"; \
		exit 1; \
	fi; \
	configured_user="$$($(DOCKER) image inspect --format '{{ .Config.User }}' $(DOCKER_IMAGE))"; \
	if [ "$$configured_user" != "10001:10001" ]; then \
		echo "Docker image declares user $$configured_user, want 10001:10001"; \
		exit 1; \
	fi; \
	actual_version="$$($(DOCKER) run --rm $(DOCKER_IMAGE) --version)"; \
	if [ "$$actual_version" != "$(VERSION)" ]; then \
		echo "Docker image version $$actual_version, want $(VERSION)"; \
		exit 1; \
	fi; \
	$(DOCKER) run --rm $(DOCKER_IMAGE) --help 2>&1 | grep -F "Usage of $(PROJECT_NAME):" >/dev/null; \
	label_version="$$($(DOCKER) image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' $(DOCKER_IMAGE))"; \
	if [ "$$label_version" != "$(VERSION)" ]; then \
		echo "Docker image OCI version $$label_version, want $(VERSION)"; \
		exit 1; \
	fi

docker-buildx: ## Build a multi-platform Docker image with buildx.
	$(DOCKER) buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		--build-arg VERSION="$(VERSION)" \
		--build-arg VCS_REF="$(VCS_REF)" \
		-t $(DOCKER_IMAGE) \
		.

docker-buildx-push: ## Build and push a multi-platform Docker image with buildx.
	$(DOCKER) buildx build \
		--push \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		--build-arg VERSION="$(VERSION)" \
		--build-arg VCS_REF="$(VCS_REF)" \
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

prometheus-rules-check: ## Validate standalone and Helm-rendered Prometheus rules with promtool.
	$(DOCKER) run --rm --entrypoint promtool \
		-v "$(CURDIR)/examples/prometheus/alerts:/rules:ro" \
		$(PROMTOOL_IMAGE) check rules /rules/puppet-forge.yml
	$(DOCKER) run --rm --entrypoint promtool \
		-v "$(CURDIR)/examples/prometheus/alerts:/rules:ro" \
		$(PROMTOOL_IMAGE) test rules /rules/puppet-forge.test.yml
	@set -eu; \
	rendered="$$(mktemp)"; \
	trap 'rm -f "$$rendered"' EXIT; \
	$(HELM) template puppet-forge $(CHART_DIR) \
		--show-only templates/prometheusrule.yaml \
		--set prometheusRule.enabled=true \
		--set secret.create=true \
		--set-string secret.stringData.DATABASE_DSN=postgres://forge:forge@postgres:5432/forge \
		--set-string secret.stringData.ACCESS_TOKEN_PEPPER=promtool-access-token-pepper-32-bytes \
		--set-string secret.stringData.MANAGE_SESSION_SECRET=promtool-manage-session-secret-32-bytes \
		> "$$rendered"; \
	awk 'found { sub(/^  /, ""); print } /^spec:/ { found=1 }' "$$rendered" \
		| $(DOCKER) run --rm -i --entrypoint promtool $(PROMTOOL_IMAGE) check rules /dev/stdin

helm-lint: helm-template-check ## Lint Helm charts and render supported configurations.
	$(HELM) lint deploy/puppet-forge \
		--set secret.create=true \
		--set-string secret.stringData.DATABASE_DSN=postgres://forge:forge@postgres:5432/forge \
		--set-string secret.stringData.ACCESS_TOKEN_PEPPER=helm-lint-access-token-pepper-32-bytes \
		--set-string secret.stringData.MANAGE_SESSION_SECRET=helm-lint-manage-session-secret-32-bytes

helm-template-check: ## Render supported Helm modes and require invalid combinations to fail.
	scripts/helm-template-check.sh

helm-package: ## Package the Helm chart into dist/charts/. Override CHART_VERSION/APP_VERSION for releases.
	mkdir -p $(CHART_DIST_DIR)
	$(HELM) package $(CHART_DIR) --destination $(CHART_DIST_DIR) $(if $(CHART_VERSION),--version $(CHART_VERSION)) $(if $(APP_VERSION),--app-version $(APP_VERSION))

check: fmt-check vet lint coverage-check compose-config helm-lint prometheus-rules-check ## Run the standard local checks.

full-check: check release-smoke size ## Run all local checks and release smoke.

ci: check test-race docker-smoke ## Run extended checks.

clean: ## Remove generated local artifacts.
	rm -rf $(DIST_DIR)
	rm -f $(COVERAGE_PROFILE) $(COVERAGE_REPORT)

size:
	@du -h $(BUILD_OUTPUT)* 2>/dev/null || true
