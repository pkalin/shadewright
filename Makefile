# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# Names of the images CI publishes to ghcr.io/pkalin (the executor is the manager binary).
IMG_EXECUTOR ?= ghcr.io/pkalin/shadewright-executor:dev
IMG_ANALYZER ?= ghcr.io/pkalin/shadewright-analyzer:dev
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
# It defaults to the year in the committed deepcopy header, so regenerating does not rewrite every header
# on 1 January; the current year is used only if that file has none. Evaluated once, when make starts.
ifeq ($(origin YEAR),undefined)
YEAR := $(shell sed -n -E 's/^Copyright ([0-9]{4}) The Shadewright Authors\.$$/\1/p' api/v1alpha1/zz_generated.deepcopy.go 2>/dev/null | head -n1)
ifeq ($(YEAR),)
YEAR := $(shell date +%Y)
endif
endif

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

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
manifests: controller-gen ## Generate WebhookConfiguration, the executor and analyzer roles, and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=executor-role paths="./internal/controller/..." output:rbac:artifacts:config=config/rbac/executor
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-role paths="./internal/analyzer" output:rbac:artifacts:config=config/rbac/analyzer
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-watch-role paths="./internal/analyzer/collector/..." output:rbac:artifacts:config=config/rbac/analyzer/watch

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the API reference (docs/reference/api.md) from the Go types in api/.
	"$(CRD_REF_DOCS)" --source-path=./api --config=hack/api-docs/config.yaml \
		--templates-dir=hack/api-docs/templates --renderer=markdown \
		--output-path=docs/reference/api.md --log-level=WARN

.PHONY: docs-requirements
docs-requirements: ## Regenerate requirements-docs.txt (versions and sha256 hashes) from requirements-docs.in.
	pipx run --spec 'pip-tools==7.6.2' pip-compile --generate-hashes --strip-extras --allow-unsafe --annotation-style=line \
		--output-file=requirements-docs.txt requirements-docs.in

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test-scripts
test-scripts: ## Test the release notes, chart docs and CI change detection scripts (hack/*_test.sh).
	hack/release-notes-by-scope_test.sh
	hack/check-chart-docs_test.sh
	hack/ci-changed_test.sh

.PHONY: test
test: manifests generate fmt vet setup-envtest test-scripts ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v -e /e2e -e /test/chart) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The kind target installs the pinned Kind (KIND_VERSION) into bin/. The setup builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= shadewright-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: kind ## Set up a Kind cluster for e2e tests if it does not exist
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: check-go-toolchain golangci-lint ## Run golangci-lint linter (and check the Go toolchain matches the Dockerfile builder)
	"$(GOLANGCI_LINT)" run

.PHONY: check-go-toolchain
check-go-toolchain: ## Fail if go.mod's toolchain line differs from the Go release of the Dockerfile builder stage.
	hack/check-go-toolchain.sh

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: vulncheck
vulncheck: govulncheck ## Report known vulnerabilities reached from the code (Go standard library of the current toolchain included)
	"$(GOVULNCHECK)" ./...

# Generated outputs owned by manifests, generate and api-docs. verify-generated deletes them first, because
# controller-gen never deletes a file it no longer generates.
GENERATED_FILES = 'config/crd/bases/*.yaml' config/rbac/executor/role.yaml config/rbac/analyzer/role.yaml \
	config/rbac/analyzer/watch/role.yaml config/webhook/manifests.yaml '*zz_generated.deepcopy.go' \
	docs/reference/api.md

.PHONY: verify-generated
verify-generated: ## Regenerate all generated files from scratch and fail if the working tree then differs from Git.
	git ls-files -z -- $(GENERATED_FILES) | xargs -0 rm -f --
	$(MAKE) manifests generate api-docs YEAR=$(YEAR)
	@status="$$(git status --porcelain)"; \
	if [ -n "$$status" ]; then \
		echo "error: generated files are stale, or the working tree was not clean." >&2; \
		echo "Run 'make manifests generate api-docs' and commit the result, deletions included:" >&2; \
		echo "$$status" >&2; \
		exit 1; \
	fi

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the executor (bin/manager) and analyzer binaries.
	go build -o bin/manager cmd/main.go
	go build -o bin/analyzer ./cmd/analyzer

.PHONY: run
run: manifests generate fmt vet ## Run the executor from your host (operator namespace from NAMESPACE).
	go run ./cmd/main.go --namespace=$(or $(NAMESPACE),shadewright-system)

.PHONY: run-analyzer
run-analyzer: manifests generate fmt vet ## Run the analyzer from your host.
	go run ./cmd/analyzer

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Base images are pinned by digest in the Dockerfile; see its header to build from a mirror.
# DOCKER_BUILD_ARGS passes extra flags to the build, such as a --build-context for a mirror.
DOCKER_BUILD_ARGS ?=
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) -t ${IMG} .

.PHONY: docker-build-images
docker-build-images: ## Build the executor and analyzer images (one binary each, as CI publishes them) for the host platform as IMG_EXECUTOR and IMG_ANALYZER.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target executor -t $(IMG_EXECUTOR) .
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target analyzer -t $(IMG_ANALYZER) .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name shadewright-builder
	$(CONTAINER_TOOL) buildx use shadewright-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(DOCKER_BUILD_ARGS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm shadewright-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	{ "$(KUSTOMIZE)" build config/default; echo "---"; "$(KUSTOMIZE)" build config/watch; } > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -
	"$(KUSTOMIZE)" build config/watch | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/watch | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
HELM_DOCS ?= $(LOCALBIN)/helm-docs

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0
CRD_REF_DOCS_VERSION ?= v0.3.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION ?= v1.9.0
# Generates dist/chart/README.md from values.yaml (checked through the Go checksum database).
HELM_DOCS_VERSION ?= v1.14.2
# The one kind pin: CI, the devcontainer and make test-e2e all install it through the kind target below.
KIND_VERSION ?= v0.33.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary (checked through the Go checksum database).
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: helm-docs
helm-docs: $(HELM_DOCS) ## Download helm-docs locally if necessary.
$(HELM_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(HELM_DOCS),github.com/norwoodj/helm-docs/cmd/helm-docs,$(HELM_DOCS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Helm chart

# The chart in dist/chart was generated with Kubebuilder's helm/v2-alpha plugin and then adjusted by hand (two
# Deployments, per-component values, RBAC that matches config/default). Do not regenerate it with
# "kubebuilder edit --plugins=helm/v2-alpha --force": test-chart shows what such a run would break.
HELM ?= helm
CHART_DIR ?= dist/chart
HELM_DOCS_FLAGS = --chart-search-root $(CHART_DIR) --sort-values-order file --skip-version-footer
CHART_KIND_CLUSTER ?= shadewright-chart-test

.PHONY: chart-docs
chart-docs: helm-docs ## Regenerate $(CHART_DIR)/README.md from values.yaml and README.md.gotmpl.
	"$(HELM_DOCS)" $(HELM_DOCS_FLAGS)

# Fails when a value has no description (an empty Description cell, or no "# @section": hack/check-chart-docs.sh)
# or when README.md is not what helm-docs renders from values.yaml (stale). Nothing is written.
.PHONY: chart-docs-check
chart-docs-check: helm-docs ## Fail if the chart README is stale or a value has no description.
	@"$(HELM_DOCS)" $(HELM_DOCS_FLAGS) --dry-run 2>/dev/null | hack/check-chart-docs.sh -
	@"$(HELM_DOCS)" $(HELM_DOCS_FLAGS) --dry-run 2>/dev/null | diff -u $(CHART_DIR)/README.md - >&2 || { \
		echo "error: $(CHART_DIR)/README.md is stale: run 'make chart-docs' and commit the result." >&2; exit 1; }

.PHONY: chart-lint
chart-lint: chart-docs-check ## Check the chart README, lint the chart with the test values, and check that the schema (not a template's required) refuses a tag instead of a digest.
	$(HELM) lint $(CHART_DIR) -f $(CHART_DIR)/ci/test-values.yaml --strict
	@if $(HELM) lint $(CHART_DIR) -f $(CHART_DIR)/ci/test-values.yaml --set executor.image.digest=latest >/dev/null 2>&1; then \
		echo "helm lint passed with a tag instead of a digest: values.schema.json is not enforced"; exit 1; \
	fi

.PHONY: test-chart
test-chart: kustomize ## Render the chart and fail if its RBAC, NetworkPolicies, Services or CRDs differ from kustomize build config/default.
	KUSTOMIZE="$(KUSTOMIZE)" HELM="$(HELM)" CHART_TEST_REQUIRED=1 go test -count=1 ./test/chart/ -v

.PHONY: test-chart-install
test-chart-install: kind ## Install the chart on a Kind cluster (created here) and check the Deployments and the permissions.
	@case "$$($(KIND) get clusters)" in \
		*"$(CHART_KIND_CLUSTER)"*) echo "Kind cluster '$(CHART_KIND_CLUSTER)' already exists." ;; \
		*) $(KIND) create cluster --name $(CHART_KIND_CLUSTER) ;; \
	esac
	$(KUBECTL) config use-context kind-$(CHART_KIND_CLUSTER)
	hack/chart-install-test.sh "$(KUBECTL)" "$(HELM)"

.PHONY: chart-kind-delete
chart-kind-delete: kind ## Delete the Kind cluster of test-chart-install.
	@$(KIND) delete cluster --name $(CHART_KIND_CLUSTER)
