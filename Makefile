# Image registry for ko builds (required for pushing images)
# Set this to your container registry (e.g., gcr.io/my-project, docker.io/myuser, ghcr.io/myuser)
KO_DOCKER_REPO ?= ghcr.io/nirmata/ottoflow
# Version for image tags: when repo is on a git tag use that tag (e.g. v0.1.0), else 0.0.0-g<short-sha>.
# Override IMAGE_TAG to pin a specific tag.
GIT_EXACT_TAG := $(shell git describe --tags --exact-match 2>/dev/null)
GIT_SHORT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
IMAGE_TAG ?= $(if $(GIT_EXACT_TAG),$(GIT_EXACT_TAG),0.0.0-g$(GIT_SHORT_SHA))
# Image override for deploy (when set, overrides chart defaults). Leave empty to use chart values.
IMG ?=
WORKFLOW_RUNNER_IMG ?=
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.29.0

# Helm chart configuration
HELM_CHART_PATH ?= ./charts/ottoflow
HELM_RELEASE_NAME ?= ottoflow
HELM_NAMESPACE ?= ottoflow
HELM_VALUES_FILE ?= ""
HELM_OUTPUT_DIR ?= config/generated
HELM_OUTPUT_FILE ?= $(HELM_OUTPUT_DIR)/install.yaml

# Every go invocation below builds this module alone. A go.work above the repo
# root pulls its own replace directives into the build, so a checkout resolves
# differently depending on where it sits in the caller's filesystem.
export GOWORK := off

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq ($(shell go env GOBIN),)
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Go module package path (for codegen)
PACKAGE ?= github.com/nirmata/ottoflow

# Import paths for ko builds
MANAGER_IMPORT_PATH = github.com/nirmata/ottoflow/cmd/controller
AGENT_EXECUTOR_IMPORT_PATH = github.com/nirmata/ottoflow/cmd/agent-executor
WORKFLOW_RUNNER_IMPORT_PATH = github.com/nirmata/ottoflow/cmd/workflow-runner
# Image tag for ko (semantic: repo tag or 0.0.0-dev-<sha>; override with IMAGE_TAG=)
KO_TAGS := -t $(IMAGE_TAG)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	@$(MAKE) sync-crds

.PHONY: sync-crds
sync-crds: ## Sync CRDs from config/crd/bases to charts/ottoflow/crds (source of truth).
	@echo "Syncing CRDs to Helm chart..."
	@mkdir -p charts/ottoflow/crds
	@if [ -n "$$(ls -A config/crd/bases/*.yaml 2>/dev/null)" ]; then \
		cp config/crd/bases/*.yaml charts/ottoflow/crds/; \
		echo "CRDs synced to charts/ottoflow/crds/"; \
	else \
		echo "Warning: No CRDs found in config/crd/bases/ - run 'make manifests' first"; \
	fi

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

##@ Code generation (docs)

.PHONY: codegen-api-docs
codegen-api-docs: crd-ref-docs ## Generate CRD API reference docs (Markdown) from Go API types (elastic/crd-ref-docs).
	@echo "Generating CRD API reference docs..."
	@mkdir -p docs/user/reference/api
	$(CRD_REF_DOCS) \
		--source-path=./api \
		--config=$(abspath hack/crd-ref-docs/config.yaml) \
		--renderer=markdown \
		--output-path=docs/user/reference/api/api-docs.md
	@echo "Generated docs/user/reference/api/api-docs.md"

.PHONY: codegen-cli-docs
codegen-cli-docs: ## Generate CLI reference docs (Markdown) from the Cobra command tree into docs/cli/.
	@echo "Generating CLI reference docs..."
	@rm -rf docs/cli
	go test -tags gendocs -count=1 ./cli/cmd/... -run '^TestGenerateCliDocs$$' -v
	@echo "Generated docs/cli/*.md"

.PHONY: codegen-docs
codegen-docs: codegen-api-docs codegen-cli-docs ## Generate all reference docs (CRD API docs + CLI docs).

##@ Verification

# Paths written by manifests/generate/codegen-api-docs/codegen-cli-docs. Checked with
# `git status --porcelain`, not `git diff`, because docs/cli/ (and any newly-added CRD file)
# starts out untracked -- a plain `git diff` only catches modified tracked files and would
# silently pass even though the freshly generated file was never committed.
CODEGEN_PATHS := config/crd/bases charts/ottoflow/crds api/v1alpha1/zz_generated.deepcopy.go docs/user/reference/api docs/cli

.PHONY: verify-codegen
verify-codegen: manifests generate codegen-docs ## Fail if generated CRDs, deepcopy code, or docs are stale or uncommitted.
	@echo "Checking for uncommitted changes from code generation..."
	@CHANGES="$$(git status --porcelain -- $(CODEGEN_PATHS))"; \
	if [ -n "$$CHANGES" ]; then \
		echo ""; \
		echo "Generated files are out of date or not committed. Run 'make manifests generate codegen-docs'" >&2; \
		echo "and commit the result. Changed/untracked paths:" >&2; \
		echo "$$CHANGES" >&2; \
		exit 1; \
	fi
	@echo "Generated files are up to date."

##@ Development (misc)

.PHONY: licenses
licenses: ## Regenerate THIRD_PARTY_LICENSES.md
	./hack/gen-third-party-licenses.sh > THIRD_PARTY_LICENSES.md

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out
	@echo "---"
	@go tool cover -func=cover.out | tail -1

# Utilize Kind or modify the e2e tests to load the image locally, enabling compatibility with other vendors.
# Set SKIP_PROMETHEUS_OPERATOR=1 to skip Prometheus install for faster runs.
.PHONY: test-e2e
test-e2e: ## Run e2e tests (expects a kube cluster; use test-e2e-kind to create kind first).
	# The e2e files are behind the `e2e` build tag so that a plain `go test ./...` does not
	# try to reach a cluster. Dropping this tag makes the suite compile to zero tests and
	# pass vacuously, so keep it in step with the //go:build lines in test/e2e.
	go test -tags e2e ./test/e2e/ -v -ginkgo.v -count=1

KIND_CLUSTER ?= kind
.PHONY: test-e2e-kind
test-e2e-kind: ## Ensure a kind cluster exists, then run e2e tests with webhooks enabled.
	@if ! kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "Creating kind cluster: $(KIND_CLUSTER)"; \
		kind create cluster --name "$(KIND_CLUSTER)"; \
	else \
		echo "Kind cluster '$(KIND_CLUSTER)' already exists"; \
	fi
	$(MAKE) test-e2e

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter & yamllint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Demo

.PHONY: demo
demo: ## Regenerate the README demo GIF from images/demo.tape (requires vhs+ttyd+ffmpeg).
	./hack/gen-demo-gif.sh

##@ Build

.PHONY: build
build: manifests generate fmt vet lint ## Build manager binary.
	go build -o bin/manager cmd/controller/main.go

##@ CLI

# CLI binary name
CLI_BINARY_NAME=ottoflow
CLI_MAIN_PACKAGE=./cli/main.go

# CLI version info
CLI_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
CLI_BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
CLI_GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
CLI_LDFLAGS=-ldflags "-X github.com/nirmata/ottoflow/cli/cmd.version=$(CLI_VERSION) -X github.com/nirmata/ottoflow/cli/cmd.buildTime=$(CLI_BUILD_TIME) -X github.com/nirmata/ottoflow/cli/cmd.gitCommit=$(CLI_GIT_COMMIT)"

.PHONY: build-cli
build-cli: manifests generate fmt vet ## Build CLI binary.
	@echo "Building $(CLI_BINARY_NAME)..."
	@mkdir -p bin
	go build -v $(CLI_LDFLAGS) -o bin/$(CLI_BINARY_NAME) $(CLI_MAIN_PACKAGE)
	@echo "Binary built: bin/$(CLI_BINARY_NAME)"

.PHONY: build-cli-linux
build-cli-linux: manifests generate fmt vet ## Build CLI binary for Linux.
	@echo "Building $(CLI_BINARY_NAME) for Linux..."
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -v $(CLI_LDFLAGS) -o bin/$(CLI_BINARY_NAME)-linux-amd64 $(CLI_MAIN_PACKAGE)
	@echo "Binary built: bin/$(CLI_BINARY_NAME)-linux-amd64"

.PHONY: build-cli-darwin
build-cli-darwin: manifests generate fmt vet ## Build CLI binary for macOS.
	@echo "Building $(CLI_BINARY_NAME) for macOS..."
	@mkdir -p bin
	GOOS=darwin GOARCH=amd64 go build -v $(CLI_LDFLAGS) -o bin/$(CLI_BINARY_NAME)-darwin-amd64 $(CLI_MAIN_PACKAGE)
	GOOS=darwin GOARCH=arm64 go build -v $(CLI_LDFLAGS) -o bin/$(CLI_BINARY_NAME)-darwin-arm64 $(CLI_MAIN_PACKAGE)
	@echo "Binaries built: bin/$(CLI_BINARY_NAME)-darwin-{amd64,arm64}"

.PHONY: build-cli-windows
build-cli-windows: manifests generate fmt vet ## Build CLI binary for Windows.
	@echo "Building $(CLI_BINARY_NAME) for Windows..."
	@mkdir -p bin
	GOOS=windows GOARCH=amd64 go build -v $(CLI_LDFLAGS) -o bin/$(CLI_BINARY_NAME)-windows-amd64.exe $(CLI_MAIN_PACKAGE)
	@echo "Binary built: bin/$(CLI_BINARY_NAME)-windows-amd64.exe"

.PHONY: build-cli-all
build-cli-all: build-cli-linux build-cli-darwin build-cli-windows ## Build CLI binaries for all platforms.
	@echo "All platform binaries built in bin/"

.PHONY: install-cli
install-cli: build-cli ## Install CLI to $GOPATH/bin or $GOBIN.
	@echo "Installing $(CLI_BINARY_NAME)..."
	go install $(CLI_LDFLAGS) $(CLI_MAIN_PACKAGE)
	@echo "Installed: $$(go env GOPATH)/bin/$(CLI_BINARY_NAME)"

.PHONY: install-cli-local
install-cli-local: build-cli ## Install CLI to /usr/local/bin (requires sudo).
	@echo "Installing $(CLI_BINARY_NAME) to /usr/local/bin..."
	sudo cp bin/$(CLI_BINARY_NAME) /usr/local/bin/
	@echo "Installed: /usr/local/bin/$(CLI_BINARY_NAME)"

.PHONY: test-cli
test-cli: build-cli ## Test CLI with sample workflows.
	@echo "Testing CLI with sample workflows..."
	@cd cli && ./test-samples.sh

.PHONY: run-samples-kind
run-samples-kind: build-cli ## Run sample workflows against local kind cluster (creates cluster if needed, installs CRDs). Set PROMETHEUS_URL for prometheusQuery steps.
	@./scripts/run-samples-kind.sh

.PHONY: validate-samples
validate-samples: manifests install ## Validate all samples/**/*.yaml against installed CRDs (requires a cluster).
	@kubectl create namespace ottoflow --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	@fail=0; \
	for f in $$(find samples -name '*.yaml' | sort); do \
	  if ! out=$$(kubectl apply --dry-run=server -f "$$f" 2>&1); then \
	    echo "FAIL $$f"; echo "$$out" | sed 's/^/      /'; fail=1; \
	  fi; \
	done; \
	if [ $$fail -eq 0 ]; then echo "All samples valid."; fi; \
	exit $$fail

.PHONY: cli-version
cli-version: ## Show CLI version information.
	@echo "Version: $(CLI_VERSION)"
	@echo "Build Time: $(CLI_BUILD_TIME)"
	@echo "Git Commit: $(CLI_GIT_COMMIT)"

.PHONY: image-version
image-version: ## Show image tag used by ko-build/ko-push (repo tag or 0.0.0-g<short-sha>). Override with IMAGE_TAG=.
	@echo "IMAGE_TAG: $(IMAGE_TAG)"
	@echo "  (from git tag: $(or $(GIT_EXACT_TAG),none), short sha: $(GIT_SHORT_SHA))"

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host (creates ottoflow namespace for leader election).
	kubectl create namespace ottoflow --dry-run=client -o yaml | kubectl apply -f -
	go run ./cmd/controller/main.go --namespace=ottoflow --workflow-runner-cluster-role=ottoflow-runner-role

##@ Container Build (using ko)

# ko builds container images directly from Go source without Dockerfiles.
# Image tag is semantic: when HEAD is on a git tag we use that tag (e.g. v0.1.0);
# otherwise 0.0.0-g<short-sha>. Override with IMAGE_TAG= when running make.
# Set KO_DOCKER_REPO to your container registry. For local testing use KO_DOCKER_REPO=ko.local.
.PHONY: ko-build
ko-build: ko ## Build container images with ko (manager, agent-executor, workflow-runner).
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(MANAGER_IMPORT_PATH)
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(AGENT_EXECUTOR_IMPORT_PATH)
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(WORKFLOW_RUNNER_IMPORT_PATH)

.PHONY: ko-build-manager
ko-build-manager: ko ## Build manager container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(MANAGER_IMPORT_PATH)

.PHONY: ko-build-agent-executor
ko-build-agent-executor: ko ## Build agent-executor container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(AGENT_EXECUTOR_IMPORT_PATH)

.PHONY: ko-build-workflow-runner
ko-build-workflow-runner: ko ## Build workflow-runner container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --push=false --base-import-paths --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(WORKFLOW_RUNNER_IMPORT_PATH)

.PHONY: ko-push
ko-push: ko ## Build and push container images with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(MANAGER_IMPORT_PATH)
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(AGENT_EXECUTOR_IMPORT_PATH)
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(WORKFLOW_RUNNER_IMPORT_PATH)

.PHONY: ko-push-manager
ko-push-manager: ko ## Build and push manager container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(MANAGER_IMPORT_PATH)

.PHONY: ko-push-agent-executor
ko-push-agent-executor: ko ## Build and push agent-executor container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(AGENT_EXECUTOR_IMPORT_PATH)

.PHONY: ko-push-workflow-runner
ko-push-workflow-runner: ko ## Build and push workflow-runner container image with ko.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --base-import-paths --push --platform=linux/amd64,linux/arm64 $(KO_TAGS) $(WORKFLOW_RUNNER_IMPORT_PATH)

# Legacy docker-build targets for backward compatibility (now use ko)
.PHONY: docker-build
docker-build: ko-build-manager ## Build manager image (uses ko).
	@echo "Note: docker-build now uses ko. Use 'make ko-build-manager' directly."

.PHONY: docker-build-agent-executor
docker-build-agent-executor: ko-build-agent-executor ## Build agent-executor image (uses ko).
	@echo "Note: docker-build-agent-executor now uses ko. Use 'make ko-build-agent-executor' directly."

.PHONY: docker-build-workflow-runner
docker-build-workflow-runner: ko-build-workflow-runner ## Build workflow-runner image (uses ko).
	@echo "Note: docker-build-workflow-runner now uses ko. Use 'make ko-build-workflow-runner' directly."

.PHONY: docker-push
docker-push: ko-push-manager ## Push manager image (uses ko).
	@echo "Note: docker-push now uses ko. Use 'make ko-push-manager' directly."

.PHONY: docker-push-agent-executor
docker-push-agent-executor: ko-push-agent-executor ## Push agent-executor image (uses ko).
	@echo "Note: docker-push-agent-executor now uses ko. Use 'make ko-push-agent-executor' directly."

.PHONY: docker-push-workflow-runner
docker-push-workflow-runner: ko-push-workflow-runner ## Push workflow-runner image (uses ko).
	@echo "Note: docker-push-workflow-runner now uses ko. Use 'make ko-push-workflow-runner' directly."

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: generate-manifests
generate-manifests: helm ## Generate a single Kubernetes installation YAML from the Helm chart.
	@echo "Generating installation manifest from Helm chart..."
	@mkdir -p $(HELM_OUTPUT_DIR)
	@if [ -n "$(HELM_VALUES_FILE)" ] && [ -f "$(HELM_VALUES_FILE)" ]; then \
		echo "Using values file: $(HELM_VALUES_FILE)"; \
		$(HELM) template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
			--namespace $(HELM_NAMESPACE) \
			--values $(HELM_VALUES_FILE) \
			$(if $(IMG),--set controller.image.fullOverride=$(IMG),) \
			$(if $(WORKFLOW_RUNNER_IMG),--set workflowRunner.image.fullOverride=$(WORKFLOW_RUNNER_IMG),) \
			$(if $(AGENT_EXECUTOR_IMG),--set agentExecutor.image.fullOverride=$(AGENT_EXECUTOR_IMG),) > $(HELM_OUTPUT_FILE); \
	else \
		if [ -n "$(IMG)" ] || [ -n "$(WORKFLOW_RUNNER_IMG)" ] || [ -n "$(AGENT_EXECUTOR_IMG)" ]; then \
			echo "Using chart defaults with image overrides"; \
			$(HELM) template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
				--namespace $(HELM_NAMESPACE) \
				$(if $(IMG),--set controller.image.fullOverride=$(IMG),) \
				$(if $(WORKFLOW_RUNNER_IMG),--set workflowRunner.image.fullOverride=$(WORKFLOW_RUNNER_IMG),) \
				$(if $(AGENT_EXECUTOR_IMG),--set agentExecutor.image.fullOverride=$(AGENT_EXECUTOR_IMG),) > $(HELM_OUTPUT_FILE); \
		else \
			echo "Using default values from chart"; \
			$(HELM) template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
				--namespace $(HELM_NAMESPACE) > $(HELM_OUTPUT_FILE); \
		fi; \
	fi
	@echo "# OttoFlow Installation Manifest" > $(HELM_OUTPUT_DIR)/install-header.tmp
	@echo "" >> $(HELM_OUTPUT_DIR)/install-header.tmp
	@cat $(HELM_OUTPUT_DIR)/install-header.tmp $(HELM_OUTPUT_FILE) > $(HELM_OUTPUT_DIR)/install-combined.tmp && mv $(HELM_OUTPUT_DIR)/install-combined.tmp $(HELM_OUTPUT_FILE)
	@rm -f $(HELM_OUTPUT_DIR)/install-header.tmp
	@echo "Installation manifest generated: $(HELM_OUTPUT_FILE)"

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: generate-manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config using generated Helm manifests.
	@echo "Deploying controller using $(HELM_OUTPUT_FILE)..."
	$(KUBECTL) apply -f $(HELM_OUTPUT_FILE)
	@echo "Controller deployed successfully"

.PHONY: undeploy
undeploy: generate-manifests ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@echo "Undeploying controller..."
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f $(HELM_OUTPUT_FILE)
	@echo "Controller undeployed"

##@ Helm Chart (gh-pages)

# Chart repo URL (GitHub Pages)
HELM_REPO_URL ?= https://nirmata.github.io/ottoflow
HELM_PACKAGE_DIR ?= $(HELM_OUTPUT_DIR)/charts

.PHONY: helm-package
helm-package: helm ## Package the Helm chart for distribution.
	@echo "Packaging Helm chart..."
	@mkdir -p $(HELM_PACKAGE_DIR)
	$(HELM) package $(HELM_CHART_PATH) -d $(HELM_PACKAGE_DIR)
	@echo "Chart packaged: $(HELM_PACKAGE_DIR)/$$(ls $(HELM_PACKAGE_DIR)/*.tgz 2>/dev/null | xargs -I {} basename {})"

.PHONY: helm-repo-index
helm-repo-index: helm helm-package ## Generate index.yaml for the chart repository (local testing).
	@echo "Generating Helm repo index..."
	$(HELM) repo index $(HELM_PACKAGE_DIR) --url $(HELM_REPO_URL)
	@echo "Index generated: $(HELM_PACKAGE_DIR)/index.yaml"

.PHONY: helm-repo-add
helm-repo-add: ## Add the OttoFlow Helm repo (after publishing to gh-pages).
	$(HELM) repo add ottoflow $(HELM_REPO_URL) --force-update
	$(HELM) repo update ottoflow
	@echo "Repo added. Run: helm search repo ottoflow"

.PHONY: setup-gh-pages
setup-gh-pages: ## Create gh-pages branch for Helm chart hosting (run once).
	@if git show-ref --verify --quiet refs/heads/gh-pages; then \
		echo "gh-pages branch already exists"; \
	else \
		orig_branch=$$(git branch --show-current); \
		echo "Creating gh-pages branch..."; \
		git checkout --orphan gh-pages; \
		git rm -rf . 2>/dev/null || true; \
		echo "# OttoFlow Helm Charts" > README.md; \
		echo "" >> README.md; \
		echo "Add this repo: \`helm repo add ottoflow https://nirmata.github.io/ottoflow\`" >> README.md; \
		git add README.md; \
		git commit -m "Initial gh-pages for Helm chart repository"; \
		git checkout $$orig_branch; \
		git push origin gh-pages; \
		echo "gh-pages branch created. Enable GitHub Pages in repo Settings -> Pages -> Source: gh-pages"; \
	fi

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
KO ?= $(LOCALBIN)/ko-$(KO_VERSION)
# Try to use system helm first, fall back to local installation
HELM ?= $(shell which helm 2>/dev/null || echo $(LOCALBIN)/helm-$(HELM_VERSION))

## Tool Versions
KUSTOMIZE_VERSION ?= v5.3.0
CONTROLLER_TOOLS_VERSION ?= v0.20.0
ENVTEST_VERSION ?= release-0.17
GOLANGCI_LINT_VERSION ?= v2.11.4
CRD_REF_DOCS_VERSION ?= v0.3.0
KO_VERSION ?= v0.17.1
HELM_VERSION ?= v3.13.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION) || { GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION) && mv $(LOCALBIN)/kustomize $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION) ; }

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION) || { GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION) && mv $(LOCALBIN)/controller-gen $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION) ; }

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION) || { GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) && mv $(LOCALBIN)/setup-envtest $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION) ; }

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION) || { curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION) && mv $(LOCALBIN)/golangci-lint $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION) ; }

.PHONY: ko
ko: $(KO) ## Download ko locally if necessary.
$(KO): $(LOCALBIN)
	test -s $(LOCALBIN)/ko-$(KO_VERSION) || { GOBIN=$(LOCALBIN) go install github.com/google/ko@$(KO_VERSION) && mv $(LOCALBIN)/ko $(LOCALBIN)/ko-$(KO_VERSION) ; }

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download elastic/crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	@echo "Installing crd-ref-docs (elastic/crd-ref-docs)..." >&2
	@test -s $(CRD_REF_DOCS) || GOBIN=$(LOCALBIN) go install github.com/elastic/crd-ref-docs@$(CRD_REF_DOCS_VERSION)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@if [ "$(shell which helm 2>/dev/null)" = "" ]; then \
		echo "Installing helm..."; \
		mkdir -p $(LOCALBIN); \
		curl -fsSL https://get.helm.sh/helm-$(HELM_VERSION)-$(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed 's/x86_64/amd64/').tar.gz | tar -xz -C $(LOCALBIN) --strip-components=1; \
		mv $(LOCALBIN)/helm $(LOCALBIN)/helm-$(HELM_VERSION); \
	fi
