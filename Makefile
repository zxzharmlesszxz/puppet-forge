include Makefile.mk

.PHONY: help fmt fmt-check tidy mod-download deps-update build release-archives release-checksums release release-smoke release-preflight release-version-check release-chart-version-check release-worktree-check release-tag-check push-release vet lint govulncheck gosec security-go trivy-filesystem trivy-image test test-postgres test-s3-storage test-gcs-storage test-object-storage test-race test-browser coverage-unit coverage-postgres coverage-s3 coverage-gcs coverage coverage-check coverage-integration coverage-integration-check coverage-merge-check docker-build docker-smoke docker-buildx docker-buildx-push docker-push compose compose-up compose-down compose-logs compose-ps compose-config compose-smoke r10k r10k-logs http-smoke oidc-preflight prometheus-rules-check helm-lint helm-template-check helm-package check ci clean-dist clean size
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

deps-update: ## Update Go module dependencies and tidy module files.
	$(GO) get -u ./...
	$(GO) mod tidy

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

release: clean-dist release-checksums ## Build release archives and checksums.

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

release-preflight: release-version-check release-chart-version-check full-check coverage-integration-check security-go test-browser test-race trivy-filesystem trivy-image ## Run all local checks required before pushing a release tag. Set VERSION=vX.Y.Z.

release-version-check: ## Validate VERSION for release targets.
	@printf '%s\n' "$(VERSION)" | grep -Eq '^v(0|1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$' || { \
		echo "VERSION must use canonical vMAJOR.MINOR.PATCH syntax; v2+ requires a /v2 module path" >&2; \
		exit 2; \
	}

release-chart-version-check: release-version-check ## Verify committed Helm chart metadata matches VERSION.
	@set -eu; \
	release_version="$(VERSION)"; \
	expected_chart_version="$${release_version#v}"; \
	chart_version="$$(awk '$$1 == "version:" { print $$2; exit }' "$(CHART_DIR)/Chart.yaml")"; \
	app_version="$$(awk '$$1 == "appVersion:" { gsub(/\"/, "", $$2); print $$2; exit }' "$(CHART_DIR)/Chart.yaml")"; \
	test "$$chart_version" = "$$expected_chart_version" || { \
		echo "Helm chart version $$chart_version does not match release $(VERSION); expected $$expected_chart_version" >&2; \
		exit 2; \
	}; \
	test "$$app_version" = "$(VERSION)" || { \
		echo "Helm chart appVersion $$app_version does not match release $(VERSION)" >&2; \
		exit 2; \
	}

release-worktree-check: ## Verify the worktree is clean before release.
	@test -z "$$(git status --porcelain)" || { echo "worktree has uncommitted changes; commit or stash before release" >&2; exit 2; }

release-tag-check: release-version-check ## Verify the release tag does not already exist locally or remotely.
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		echo "local tag already exists: $(VERSION)" >&2; \
		exit 2; \
	fi
	@remote_tags="$$(git ls-remote --tags origin "refs/tags/$(VERSION)" "refs/tags/$(VERSION)^{}")" || { \
		echo "failed to query release tags from origin" >&2; \
		exit 2; \
	}; \
	test -z "$$remote_tags" || { echo "remote tag already exists: $(VERSION)" >&2; exit 2; }

push-release: release-worktree-check release-tag-check release-preflight ## Run release preflight, push main, and push an annotated release tag. Set VERSION=vX.Y.Z.
	@set -eu; \
	current_branch="$$(git branch --show-current)"; \
	if [ "$$current_branch" != "main" ]; then \
		echo "push-release must run from main, got $$current_branch" >&2; \
		exit 2; \
	fi; \
	git push origin HEAD:main; \
	remote_head="$$(git ls-remote --heads origin refs/heads/main | awk '{print $$1}')"; \
	test "$$remote_head" = "$$(git rev-parse HEAD)" || { echo "origin/main does not match local HEAD after push" >&2; exit 2; }; \
	git tag -a "$(VERSION)" -m "Release $(VERSION)"; \
	git push origin "$(VERSION)"

vet: ## Run go vet.
	$(GO) vet ./...

lint: ## Run golangci-lint.
	PATH="$(dir $(GO)):$$PATH" $(GOLANGCI_LINT) run

govulncheck: ## Scan reachable Go code for known vulnerabilities.
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

gosec: ## Run Go security static analysis.
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -exclude-generated ./...

security-go: govulncheck gosec ## Run Go vulnerability and security scans.

trivy-filesystem: ## Scan dependencies, secrets, licenses, and repository configuration with Trivy.
	$(DOCKER) run --rm \
		-v $(TRIVY_CACHE_VOLUME):/root/.cache/trivy \
		-v "$(CURDIR):/work:ro" \
		-w /work \
		$(TRIVY_IMAGE) fs --scanners vuln,secret,license --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .
	$(DOCKER) run --rm \
		-v $(TRIVY_CACHE_VOLUME):/root/.cache/trivy \
		-v "$(CURDIR):/work:ro" \
		-w /work \
		$(TRIVY_IMAGE) fs --config .trivy-misconfig.yaml --scanners misconfig .

trivy-image: docker-smoke ## Scan the locally built Docker image with Trivy.
	$(DOCKER) run --rm \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v $(TRIVY_CACHE_VOLUME):/root/.cache/trivy \
		$(TRIVY_IMAGE) image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $(DOCKER_IMAGE)

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
	PLAYWRIGHT_IMAGE="$(PLAYWRIGHT_IMAGE)" DOCKER="$(DOCKER)" GO="$(GO)" CURL="$(CURL)" bash scripts/browser-tests.sh

coverage-unit: ## Run unit tests and write their coverage profile.
	$(GO) test -buildvcs=false -ldflags "$(LDFLAGS)" -covermode=atomic -coverprofile=$(COVERAGE_UNIT_PROFILE) ./...

coverage-postgres: ## Run PostgreSQL integration tests and write their coverage profile.
	@test -n "$(PUPPET_FORGE_TEST_POSTGRES_DSN)" || (echo "PUPPET_FORGE_TEST_POSTGRES_DSN is required"; exit 1)
	PUPPET_FORGE_TEST_POSTGRES_DSN="$(PUPPET_FORGE_TEST_POSTGRES_DSN)" $(GO) test -buildvcs=false -covermode=atomic -coverprofile=$(COVERAGE_POSTGRES_PROFILE) ./internal/store -run 'TestStoreParity|TestPostgresStoreConcurrentSchemaSetup'

coverage-s3: ## Run S3 integration tests and write their coverage profile.
	@test -n "$(PUPPET_FORGE_TEST_S3_ENDPOINT)" || (echo "PUPPET_FORGE_TEST_S3_ENDPOINT is required"; exit 1)
	PUPPET_FORGE_TEST_S3_ENDPOINT="$(PUPPET_FORGE_TEST_S3_ENDPOINT)" \
	PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID="$(PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID)" \
	PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY="$(PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY)" \
	$(GO) test -buildvcs=false -covermode=atomic -coverprofile=$(COVERAGE_S3_PROFILE) ./internal/storage -run 'TestS3StorageIntegration'

coverage-gcs: ## Run GCS integration tests and write their coverage profile.
	@test -n "$(PUPPET_FORGE_TEST_GCS_ENDPOINT)" || (echo "PUPPET_FORGE_TEST_GCS_ENDPOINT is required"; exit 1)
	PUPPET_FORGE_TEST_GCS_ENDPOINT="$(PUPPET_FORGE_TEST_GCS_ENDPOINT)" \
	$(GO) test -buildvcs=false -covermode=atomic -coverprofile=$(COVERAGE_GCS_PROFILE) ./internal/storage -run 'TestGCSStorageIntegration'

coverage: coverage-unit ## Merge unit coverage and write the default report.
	bash scripts/merge-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_UNIT_PROFILE)
	$(GO) tool cover -func=$(COVERAGE_PROFILE) | tee $(COVERAGE_REPORT)

coverage-check: coverage ## Enforce the coverage threshold.
	bash scripts/check-coverage.sh $(COVERAGE_REPORT) $(COVERAGE_UNIT_THRESHOLD)
	bash scripts/check-package-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_PACKAGE_THRESHOLDS)

coverage-integration: coverage-unit coverage-postgres coverage-s3 coverage-gcs ## Merge unit, PostgreSQL, S3, and GCS coverage.
	bash scripts/merge-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_PROFILES)
	$(GO) tool cover -func=$(COVERAGE_PROFILE) | tee $(COVERAGE_REPORT)

coverage-integration-check: coverage-integration ## Enforce the merged integration coverage threshold.
	bash scripts/check-coverage.sh $(COVERAGE_REPORT) $(COVERAGE_THRESHOLD)
	bash scripts/check-package-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_PACKAGE_THRESHOLDS)

coverage-merge-check: ## Merge prebuilt profiles from COVERAGE_PROFILES and enforce the threshold.
	bash scripts/merge-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_PROFILES)
	$(GO) tool cover -func=$(COVERAGE_PROFILE) | tee $(COVERAGE_REPORT)
	bash scripts/check-coverage.sh $(COVERAGE_REPORT) $(COVERAGE_THRESHOLD)
	bash scripts/check-package-coverage.sh $(COVERAGE_PROFILE) $(COVERAGE_PACKAGE_THRESHOLDS)

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

clean-dist: ## Remove generated release artifacts.
	rm -rf $(DIST_DIR)

clean: clean-dist ## Remove all generated local artifacts.
	rm -f $(COVERAGE_PROFILE) $(COVERAGE_REPORT) $(COVERAGE_UNIT_PROFILE) $(COVERAGE_POSTGRES_PROFILE) $(COVERAGE_S3_PROFILE) $(COVERAGE_GCS_PROFILE)

size:
	@du -h $(BUILD_OUTPUT)* 2>/dev/null || true
