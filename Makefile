# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# Names of the images CI publishes to ghcr.io/pkalin (the executor is the manager binary).
IMG_EXECUTOR ?= ghcr.io/pkalin/shadewright-executor:dev
IMG_ANALYZER ?= ghcr.io/pkalin/shadewright-analyzer:dev
IMG_VERIFIER ?= ghcr.io/pkalin/shadewright-verifier:dev
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
manifests: controller-gen ## Generate WebhookConfiguration, the executor, analyzer and verifier roles, and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=executor-role paths="./internal/controller/..." output:rbac:artifacts:config=config/rbac/executor
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-role paths="./internal/analyzer" output:rbac:artifacts:config=config/rbac/analyzer
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-watch-role paths="./internal/analyzer/collector/..." output:rbac:artifacts:config=config/rbac/analyzer/watch
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-watch-keda paths="./internal/analyzer/modulerbac/keda" output:rbac:artifacts:config=config/rbac/analyzer/modules/keda
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-watch-flux paths="./internal/analyzer/modulerbac/flux" output:rbac:artifacts:config=config/rbac/analyzer/modules/flux
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-watch-kyverno paths="./internal/analyzer/modulerbac/kyverno" output:rbac:artifacts:config=config/rbac/analyzer/modules/kyverno
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-kyverno-cluster paths="./internal/analyzer/modulerbac/kyvernocluster" output:rbac:artifacts:config=config/rbac/analyzer/modules/kyverno-cluster
	"$(CONTROLLER_GEN)" rbac:roleName=analyzer-policy-reader paths="./internal/analyzer/policyrbac" output:rbac:artifacts:config=config/rbac/analyzer/policies
	"$(CONTROLLER_GEN)" rbac:roleName=verifier-role paths="./internal/verifier" output:rbac:artifacts:config=config/rbac/verifier
	"$(CONTROLLER_GEN)" rbac:roleName=verifier-watch-role paths="./internal/verifier/watchrbac" output:rbac:artifacts:config=config/rbac/verifier/watch
	$(MAKE) chart-crds

.PHONY: chart-crds
chart-crds: ## Copy config/crd/bases into dist/chart/templates/crd with the chart's wrapper, keep annotation and labels (a CRD that is gone is removed).
	hack/chart-crds.sh

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the API reference (docs/reference/api.md) from the Go types in api/.
	"$(CRD_REF_DOCS)" --source-path=./api --config=hack/api-docs/config.yaml \
		--templates-dir=hack/api-docs/templates --renderer=markdown \
		--output-path=docs/reference/api.md --log-level=WARN

.PHONY: docs-requirements
docs-requirements: ## Regenerate requirements-docs.txt (versions and sha256 hashes) from requirements-docs.in.
	pipx run --spec 'pip-tools==7.6.2' pip-compile --generate-hashes --strip-extras --allow-unsafe --annotation-style=line \
		--output-file=requirements-docs.txt requirements-docs.in

.PHONY: docs-check
docs-check: ## Build the docs site and check its internal links, heading anchors and published URLs (needs .venv, see AGENTS.md).
	python3 -I hack/docs-reasons-check.py
	.venv/bin/zensical build --clean
	python3 -I hack/docs-check.py check site hack/docs-published-paths.txt

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
test-scripts: ## Test the release notes, release steps, chart docs, chart CRD copy, chart digest, CI change detection, fuzz target, docs link check, condition reasons page check, plugin dependency check and side-by-side run scripts (hack/*_test.sh).
	hack/release-notes-by-scope_test.sh
	hack/check-chart-docs_test.sh
	hack/chart-crds_test.sh
	hack/chart-set-digests_test.sh
	hack/image-platform-digests_test.sh
	hack/resolve-image-digests_test.sh
	hack/ci-changed_test.sh
	hack/release-verify-notes_test.sh
	hack/publish-release_test.sh
	hack/require-on-main_test.sh
	hack/fuzz-targets_test.sh
	hack/docs-check_test.sh
	hack/docs-reasons-check_test.sh
	python3 -I hack/docs-reasons-check.py
	hack/check-plugin-deps_test.sh
	hack/side-by-side_test.sh

# The shards of the run of internal/redact with the tag redactwork in make test (top-level test names, joined
# with |): the tests that took longest in that run, one or a few to a binary.
REDACT_SHARD_1 := TestRepeatedPatternsStayLinear
REDACT_SHARD_2 := TestTokenShapesWorkIsLinear
REDACT_SHARD_3 := TestCanonEntryPointsLeakNothingMainHid|TestCanonStairWorkIsLinear|TestEveryStepIsMetered|TestCanonCorpusWorkIsLinear|TestNestedBlocksStayLinear|TestMonotonicStaysLinear
REDACT_SHARD_4 := TestReadingsKeepTheSignalOfAHiddenText|TestConnStringsThroughEveryEntryPointAgainstMain|TestSpansOnRuneBoundariesCanonCorpus|TestSpansOnRuneBoundariesMultiByteCorpus|TestReplacementsEndOnCharacterBoundaries|TestFieldsOfEveryToolHideWhatMainHid|TestWindowedChunksFindWhatTheExpressionFinds|TestStringsDecidesAsTheJoinedText
REDACT_SHARD_5 := TestCorpusWorkGrowsLinearly|TestBlockCorporaThroughEveryEntryPoint

# make test runs each test line through gotestsum. Every line writes <name>.xml (junit) and <name>.json
# (go test -json) under TEST_REPORT_DIR, which CI uploads and reads for the slowest tests. TEST_FORMAT keeps the
# "--- FAIL", "FAIL" and "ok" lines that scripts grep for.
TEST_REPORT_DIR ?= test-reports
TEST_FORMAT ?= standard-verbose
gotestsum = "$(GOTESTSUM)" --format "$(TEST_FORMAT)" --junitfile "$(TEST_REPORT_DIR)/$(1).xml" --jsonfile "$(TEST_REPORT_DIR)/$(1).json" --

.PHONY: test
test: manifests generate fmt vet setup-envtest gotestsum test-scripts ## Run tests (junit and JSON files in TEST_REPORT_DIR).
	@mkdir -p "$(TEST_REPORT_DIR)"
	@# -timeout applies to each package's test binary. internal/controller (the plain tests, then the envtest suite
	@# TestControllers) took 14 to 15 minutes under -race on a hosted runner before #838 and ran past 20 after it (#961),
	@# so the limit is 30 minutes; the packages still run side by side.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" $(call gotestsum,unit-race) $$(go list ./... | grep -v -e /e2e -e /test/chart) -race -timeout 30m -coverprofile cover.out
	@# The redact timing tests and the 100,000 seeded keys skip under -race, and the generated corpora check a seeded
	@# sample there (internal/redact/race_test.go, racesample_test.go), and so do the log source's tests on logs of
	@# about 1 MiB (internal/providers/logs/kube/racesample_test.go) and the context builder's two largest generated
	@# corpora (internal/incidentcontext/racesample_test.go): run those packages again without it, so
	@# that no test is selected by name and a renamed one cannot drop out. The
	@# linearity tests count redaction work only in a build with the tag redactwork (internal/redact/work_counted.go;
	@# the product build spends each step on the budget of its call and keeps no shared counter), so this run, the log
	@# excerpt's package and the context builder's get it.
	@# The differential tests of the release's reading against its code before #873 check a share of their inputs
	@# under -race (internal/redact/released/race_test.go) and all of them here.
	@# internal/redact with the tag took about 555 s in one binary on a laptop, so on a hosted runner (2.4 times
	@# slower on main) it would pass -timeout 20m: it runs in six binaries (test-redactwork-*). The first five select
	@# the heaviest tests by name (REDACT_SHARD_*), the last runs every other test with -skip, so a test renamed out
	@# of a shard runs in the last binary and none drops out. The tools and the meter run without -race only in
	@# test-redactwork-other (some of their checks sample under -race, and the meter's allocation test needs a build
	@# without it). They and the run of the other packages go side by
	@# side (hack/side-by-side.sh: one process each, each output printed whole when all are done, any failure fails
	@# make test); one after the other they took about 1,800 s of the test job on a hosted runner, and side by side on
	@# its four cores about 1,300 s, two of them past their 20 minutes (2026-11-21). CI therefore runs make test with
	@# REDACTWORK_IN_TEST=no and each of REDACTWORK_TARGETS in a job of its own (.github/workflows/test.yml, the list
	@# from make redactwork-list), so every input is still checked on every pull request.
	$(if $(filter yes,$(REDACTWORK_IN_TEST)),MAKE="$(MAKE)" hack/side-by-side.sh $(REDACTWORK_TARGETS),@echo "make test: the runs with the tag redactwork are left out (REDACTWORK_IN_TEST=$(REDACTWORK_IN_TEST)): run make $(REDACTWORK_TARGETS)")
	@# The time bound of the wide mapping refusal is checked only without -race (internal/gitops/patch/race_test.go).
	$(call gotestsum,wide-mapping) -run 'TestWideMappingRefusedInTime$$' ./internal/gitops/patch

# The runs with the tag redactwork of make test, which hack/side-by-side.sh starts side by side. CI sets
# REDACTWORK_IN_TEST=no and runs each in a job of its own (make redactwork-list).
REDACTWORK_TARGETS := test-redactwork-1 test-redactwork-2 test-redactwork-3 test-redactwork-4 test-redactwork-5 \
	test-redactwork-rest test-redactwork-other
REDACTWORK_IN_TEST ?= yes
REDACTWORK_TEST := go test -timeout 20m -tags redactwork

.PHONY: $(REDACTWORK_TARGETS)
test-redactwork-1 test-redactwork-2 test-redactwork-3 test-redactwork-4 test-redactwork-5:
	$(REDACTWORK_TEST) -run '^($(REDACT_SHARD_$(subst test-redactwork-,,$@)))$$' ./internal/redact
test-redactwork-rest:
	$(REDACTWORK_TEST) -skip '^($(REDACT_SHARD_1)|$(REDACT_SHARD_2)|$(REDACT_SHARD_3)|$(REDACT_SHARD_4)|$(REDACT_SHARD_5))$$' ./internal/redact
test-redactwork-other:
	$(REDACTWORK_TEST) ./internal/redact/released ./internal/redact/meter ./internal/providers/logs/kube ./internal/incidentcontext ./internal/tools

# One line of JSON for the matrix of the redactwork jobs of .github/workflows/test.yml: every entry of
# REDACTWORK_TARGETS, so that a run added to make test is a job in CI as well.
.PHONY: redactwork-list
redactwork-list: ## Print REDACTWORK_TARGETS as a JSON array (the matrix of the redactwork jobs in CI).
	@printf '[%s]\n' "$$(printf '"%s",' $(REDACTWORK_TARGETS) | sed 's/,$$//')"

# The evaluation harness (test/eval, docs/evaluation.md). make test runs it too; these targets write the report.
EVAL_REPORT_DIR ?= $(LOCALBIN)/eval

.PHONY: eval
eval: ## Replay every lab scenario's recorded context and replies through the analysis loop; report in EVAL_REPORT_DIR.
	EVAL_REPORT_DIR="$(EVAL_REPORT_DIR)" go test ./test/eval -run '^TestEvalReplay$$' -count=1 -v

# The provider of make eval-model: foundry (Azure workload identity), anthropic or openaicompatible (an API key file).
EVAL_MODEL ?= foundry

.PHONY: eval-model
eval-model: ## Evaluate a real model through EVAL_MODEL (foundry, anthropic or openaicompatible; the EVAL_* variables, see docs/evaluation.md). Costs money. Times out at 50m, so a report is written first.
	@case "$(EVAL_MODEL)" in foundry) t=TestEvalFoundry ;; anthropic) t=TestEvalAnthropic ;; openaicompatible) t=TestEvalOpenAICompatible ;; \
		*) echo "EVAL_MODEL must be foundry, anthropic or openaicompatible" >&2; exit 1 ;; esac; \
	EVAL_MODEL="$(EVAL_MODEL)" EVAL_REPORT_DIR="$(EVAL_REPORT_DIR)" go test ./test/eval -run "^$$t\$$" -count=1 -v -timeout 50m

# Fuzz targets as <package dir>:<FuzzName>. hack/fuzz-targets.sh checks this list against the tree both ways, so a
# new target cannot be left out of the weekly Fuzz workflow and a stale entry cannot fuzz nothing and pass.
FUZZ_TARGETS := cmd/kubectl-shadewright:FuzzClean internal/gitops/gityaml:FuzzNodes internal/gitops/gityaml:FuzzMappingKeys internal/redact:FuzzString internal/redact:FuzzMonotonicAgainstMain internal/redact:FuzzReplacementBoundaries internal/incidentcontext:FuzzLogExcerpt internal/gitops/patch:FuzzPatch internal/gitops/semdiff:FuzzDocuments internal/gitops/sourcemap:FuzzFieldEquals internal/incidents/fingerprint:FuzzNormalize internal/policy/pathmatch:FuzzMatch internal/analyzer/refcheck:FuzzCheck internal/providers/model/openaicompatible:FuzzCredentialEcho internal/providers/model/anthropic:FuzzParseResponse internal/providers/model/foundry:FuzzParseResponse internal/providers/model/foundry:FuzzParseResponses internal/tools:FuzzArguments internal/redact:FuzzDiffString internal/redact:FuzzDiffStringTwice internal/redact:FuzzDiffStrings internal/redact:FuzzDiffReplaceFrom internal/tools:FuzzDiffGetResource internal/tools:FuzzDiffGetPodLogs internal/tools:FuzzDiffListEvents internal/tools:FuzzDiffQueryMetrics internal/tools:FuzzDiffDeliveryStatus internal/tools:FuzzDiffCheckMissingReference internal/tools:FuzzDiffGitTools internal/incidentcontext:FuzzDiffBuild internal/redact/released:FuzzReleasedMatchesKeptCopy
FUZZ ?= $(FUZZ_TARGETS)
FUZZTIME ?= 30s

.PHONY: fuzz
fuzz: ## Run each fuzz target (FUZZ, default all) for FUZZTIME (default 30s); a failing input is saved under the package's testdata/fuzz.
	@hack/fuzz-targets.sh "$(CURDIR)" $(FUZZ_TARGETS)
	@$(if $(filter-out $(FUZZ_TARGETS),$(FUZZ)),echo "Not in FUZZ_TARGETS: $(filter-out $(FUZZ_TARGETS),$(FUZZ))" >&2; exit 1)
	@# A target in a package with a file built only with the tag redactwork runs with it, so that the targets that
	@# check the work bound of redaction (internal/redact FuzzDiff*, P3) count the work (hack/fuzz-targets.sh --tags).
	@for t in $(FUZZ); do \
		tags="$$(hack/fuzz-targets.sh --tags "$(CURDIR)" "$${t%%:*}")" || exit 1; \
		echo "fuzz ./$${t%%:*} $${t##*:} for $(FUZZTIME)$${tags:+ with -tags $$tags}"; \
		go test -tags "$$tags" -run '^$$' -fuzz "^$${t##*:}\$$" -fuzztime "$(FUZZTIME)" "./$${t%%:*}" || exit 1; \
	done

# One line of JSON for the matrix of .github/workflows/fuzz.yml: every entry of FUZZ_TARGETS as <package dir>:<FuzzName>.
.PHONY: fuzz-list
fuzz-list: ## Print FUZZ_TARGETS as a JSON array (the Fuzz workflow matrix).
	@hack/fuzz-targets.sh "$(CURDIR)" $(FUZZ_TARGETS)
	@printf '[%s]\n' "$$(printf '"%s",' $(FUZZ_TARGETS) | sed 's/,$$//')"

.PHONY: fuzz-one
fuzz-one: ## Run one fuzz target, TARGET=<package dir>:<FuzzName> from FUZZ_TARGETS, for FUZZTIME (a Fuzz workflow job).
	@$(if $(filter $(TARGET),$(FUZZ_TARGETS)),,echo "TARGET must be one of FUZZ_TARGETS, got '$(TARGET)'" >&2; exit 1)
	@$(MAKE) --no-print-directory fuzz FUZZ="$(TARGET)" FUZZTIME="$(FUZZTIME)"

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
lint: check-go-toolchain check-plugin-deps golangci-lint ## Run golangci-lint linter (and check the Go toolchain matches the Dockerfile builder, and the plugin's dependencies and size)
	"$(GOLANGCI_LINT)" run

.PHONY: check-go-toolchain
check-go-toolchain: ## Fail if go.mod's toolchain line differs from the Go release of the Dockerfile builder stage.
	hack/check-go-toolchain.sh

.PHONY: check-plugin-deps
check-plugin-deps: ## Fail if kubectl-shadewright links the operator's server-side stack or outgrows its size ceiling.
	hack/check-plugin-deps.sh

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
	config/rbac/analyzer/watch/role.yaml config/rbac/verifier/role.yaml config/rbac/verifier/watch/role.yaml \
	config/webhook/manifests.yaml '*zz_generated.deepcopy.go' \
	docs/reference/api.md 'dist/chart/templates/crd/*.yaml'

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
build: manifests generate fmt vet ## Build the executor (bin/manager), analyzer, verifier and scoring intake (bin/intake) binaries.
	go build -o bin/manager cmd/main.go
	go build -o bin/analyzer ./cmd/analyzer
	go build -o bin/verifier ./cmd/verifier
	go build -o bin/intake ./cmd/intake

# PLUGIN_VERSION is stamped into the plugin (kubectl shadewright version); status compares it with the CRDs' version label.
PLUGIN_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build-plugin
build-plugin: fmt vet ## Build the read-only kubectl plugin (bin/kubectl-shadewright; put it on PATH to run kubectl shadewright status).
	go build -trimpath -ldflags="-s -w -X main.version=$(PLUGIN_VERSION)" -o bin/kubectl-shadewright ./cmd/kubectl-shadewright

.PHONY: run
run: manifests generate fmt vet ## Run the executor from your host (operator namespace from NAMESPACE).
	go run ./cmd/main.go --namespace=$(or $(NAMESPACE),shadewright-system)

.PHONY: run-analyzer
run-analyzer: manifests generate fmt vet ## Run the analyzer from your host.
	go run ./cmd/analyzer

.PHONY: run-verifier
run-verifier: manifests generate fmt vet ## Run the verifier from your host (operator namespace from NAMESPACE, watched namespaces from WATCH_NAMESPACES).
	go run ./cmd/verifier --namespace=$(or $(NAMESPACE),shadewright-system) --watch-namespaces=$(WATCH_NAMESPACES)

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
docker-build-images: ## Build the executor, analyzer and verifier images (one binary each, as CI publishes them) for the host platform as IMG_EXECUTOR, IMG_ANALYZER and IMG_VERIFIER.
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target executor -t $(IMG_EXECUTOR) .
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target analyzer -t $(IMG_ANALYZER) .
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target verifier -t $(IMG_VERIFIER) .

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
GOTESTSUM ?= $(LOCALBIN)/gotestsum
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
# Runs the test lines of make test and writes their junit and JSON files (checked through the Go checksum database).
GOTESTSUM_VERSION ?= v1.13.0
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

.PHONY: gotestsum
gotestsum: $(GOTESTSUM) ## Download gotestsum locally if necessary.
$(GOTESTSUM): $(LOCALBIN)
	$(call go-install-tool,$(GOTESTSUM),gotest.tools/gotestsum,$(GOTESTSUM_VERSION))

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

CHART_TEST_VALUES ?= test/chart/testdata/values.yaml

# ct (chart-testing) for chart-ct-lint, run from its image (pinned by digest), which holds yamllint and yamale
# too. CI installs the same ct version with the chart-testing action.
CT_IMAGE ?= quay.io/helmpack/chart-testing:v3.15.0@sha256:54317f2d93730ea9ca1236e3d79cd95d74110b8d733b6ab124e5829209b5f4e4

.PHONY: chart-lint
chart-lint: chart-docs-check ## Check the chart README, lint the chart with the test values and with the default values (plus image.allowTag and a clusterId: a source checkout has no digests and lint asks no cluster), and check that the schema (not a template's required) refuses a tag instead of a digest.
	$(HELM) lint $(CHART_DIR) -f $(CHART_TEST_VALUES) --strict
	$(HELM) lint $(CHART_DIR) -f $(CHART_DIR)/ci/default-values.yaml --set clusterId=chart-lint --strict
	@if $(HELM) lint $(CHART_DIR) --set clusterId=chart-lint --set executor.image.digest=latest >/dev/null 2>&1; then \
		echo "helm lint passed with a tag instead of a digest: values.schema.json is not enforced"; exit 1; \
	fi

# The maintainer check reads the origin remote with git, so run it in a clone: a git worktree keeps its git
# directory outside the mounted folder.
.PHONY: chart-ct-lint
chart-ct-lint: ## Run chart-testing's lint on the chart (yamllint, the Chart.yaml schema, the maintainers, helm lint; ct.yaml) in the pinned ct image.
	$(CONTAINER_TOOL) run --rm -v "$(CURDIR):/src:ro" -w /src $(CT_IMAGE) \
		sh -c 'git config --global --add safe.directory /src && ct lint --config ct.yaml'

.PHONY: test-chart
test-chart: kustomize ## Render the chart and fail if its RBAC, NetworkPolicies, Services or CRDs differ from kustomize build config/default.
	KUSTOMIZE="$(KUSTOMIZE)" HELM="$(HELM)" CHART_TEST_REQUIRED=1 go test -count=1 ./test/chart/ -v

.PHONY: test-chart-pin
test-chart-pin: ## Test hack/chart-pin-digests.sh with the pinned helm (chart-test.yml runs it; not part of test-scripts, which has no pinned helm).
	HELM="$(HELM)" hack/chart-pin-digests_test.sh

.PHONY: test-chart-install
test-chart-install: kind ## Install the chart on a Kind cluster (created here) and check the Deployments and the permissions. Without EXECUTOR_DIGEST, ANALYZER_DIGEST and VERIFIER_DIGEST (a tag run) it builds the images from this checkout under the chart's repository:appVersion and loads them into Kind.
	@$(KIND) get clusters | grep -qx '$(CHART_KIND_CLUSTER)' \
		&& echo "Kind cluster '$(CHART_KIND_CLUSTER)' already exists." \
		|| $(KIND) create cluster --name $(CHART_KIND_CLUSTER)
	@if [ -z "$$EXECUTOR_DIGEST$$ANALYZER_DIGEST$$VERIFIER_DIGEST" ]; then \
		if printf '%s\n' "$(CHART_CT_EXECUTOR)" "$(CHART_CT_ANALYZER)" "$(CHART_CT_VERIFIER)" | grep -Evxq '[a-z0-9./_-]+:[0-9A-Za-z.+-]+'; then \
			echo "no image repository or appVersion in $(CHART_DIR): '$(CHART_CT_EXECUTOR)' '$(CHART_CT_ANALYZER)' '$(CHART_CT_VERIFIER)'" >&2; exit 1; \
		fi; \
		$(MAKE) docker-build-images IMG_EXECUTOR=$(CHART_CT_EXECUTOR) IMG_ANALYZER=$(CHART_CT_ANALYZER) IMG_VERIFIER=$(CHART_CT_VERIFIER) \
			&& $(KIND) load docker-image --name $(CHART_KIND_CLUSTER) $(CHART_CT_EXECUTOR) $(CHART_CT_ANALYZER) $(CHART_CT_VERIFIER); \
	fi
	$(KUBECTL) config use-context kind-$(CHART_KIND_CLUSTER)
	hack/chart-install-test.sh "$(KUBECTL)" "$(HELM)"

.PHONY: chart-kind-delete
chart-kind-delete: kind ## Delete the Kind cluster of test-chart-install.
	@$(KIND) delete cluster --name $(CHART_KIND_CLUSTER)

# The chart-testing job runs the chart with its default values plus image.allowTag (dist/chart/ci/
# default-values.yaml), so the images must be the ones those values name: repository:appVersion, both read from
# the chart. They are built from this checkout under that tag and loaded into the Kind cluster. The local Docker
# keeps them under that tag too: after a local run, remove them before you pull the published release images.
CHART_CT_KIND_CLUSTER ?= shadewright-chart-ct
CHART_CT_VALUES ?= $(CHART_DIR)/ci/default-values.yaml
CHART_APP_VERSION = $(shell sed -n 's/^appVersion: "\{0,1\}\([0-9A-Za-z.+-]*\)"\{0,1\}.*/\1/p' $(CHART_DIR)/Chart.yaml)
# chart-repository <component>: image.repository of that component in values.yaml.
chart-repository = $(shell awk '/^$(1):/{c=1;next} /^[^ \#]/{c=0} c && /^    repository:/{print $$2; exit}' $(CHART_DIR)/values.yaml)
CHART_CT_EXECUTOR = $(call chart-repository,executor):$(CHART_APP_VERSION)
CHART_CT_ANALYZER = $(call chart-repository,analyzer):$(CHART_APP_VERSION)
CHART_CT_VERIFIER = $(call chart-repository,verifier):$(CHART_APP_VERSION)

.PHONY: chart-ct-cluster
chart-ct-cluster: kind ## Create the Kind cluster of the chart-testing job and load images built from this checkout under the chart's repository:appVersion.
	@if printf '%s\n' "$(CHART_CT_EXECUTOR)" "$(CHART_CT_ANALYZER)" "$(CHART_CT_VERIFIER)" | grep -Evxq '[a-z0-9./_-]+:[0-9A-Za-z.+-]+'; then \
		echo "no image repository or appVersion in $(CHART_DIR): '$(CHART_CT_EXECUTOR)' '$(CHART_CT_ANALYZER)' '$(CHART_CT_VERIFIER)'" >&2; exit 1; \
	fi
	@$(KIND) get clusters | grep -qx '$(CHART_CT_KIND_CLUSTER)' \
		&& echo "Kind cluster '$(CHART_CT_KIND_CLUSTER)' already exists." \
		|| $(KIND) create cluster --name $(CHART_CT_KIND_CLUSTER)
	$(MAKE) docker-build-images IMG_EXECUTOR=$(CHART_CT_EXECUTOR) IMG_ANALYZER=$(CHART_CT_ANALYZER) IMG_VERIFIER=$(CHART_CT_VERIFIER)
	$(KIND) load docker-image --name $(CHART_CT_KIND_CLUSTER) $(CHART_CT_EXECUTOR) $(CHART_CT_ANALYZER) $(CHART_CT_VERIFIER)
	$(KUBECTL) config use-context kind-$(CHART_CT_KIND_CLUSTER)

.PHONY: test-chart-defaults
test-chart-defaults: chart-ct-cluster ## Install the chart with the default values (plus image.allowTag) on the chart-testing Kind cluster, check what it may read and write, then run helm test.
	hack/chart-default-install-test.sh "$(KUBECTL)" "$(HELM)" "$(CHART_CT_VALUES)"

.PHONY: test-chart-anthropic
test-chart-anthropic: chart-ct-cluster ## Install the chart with Anthropic-only values (test/chart/testdata/anthropic-only-values.yaml, a dummy key Secret) on the chart-testing Kind cluster and check the Deployments, the analyzer's restarts and log, and what the API server does with RemediationSettings.
	hack/chart-anthropic-install-test.sh "$(KUBECTL)" "$(HELM)"

.PHONY: chart-ct-cluster-analyzer
chart-ct-cluster-analyzer: kind ## Create the Kind cluster of the chart-testing job (if it is not there) and load only the analyzer image built from this checkout under the chart's repository:appVersion.
	@if printf '%s\n' "$(CHART_CT_ANALYZER)" | grep -Evxq '[a-z0-9./_-]+:[0-9A-Za-z.+-]+'; then \
		echo "no image repository or appVersion in $(CHART_DIR): '$(CHART_CT_ANALYZER)'" >&2; exit 1; \
	fi
	@$(KIND) get clusters | grep -qx '$(CHART_CT_KIND_CLUSTER)' \
		&& echo "Kind cluster '$(CHART_CT_KIND_CLUSTER)' already exists." \
		|| $(KIND) create cluster --name $(CHART_CT_KIND_CLUSTER)
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) --target analyzer -t $(CHART_CT_ANALYZER) .
	$(KIND) load docker-image --name $(CHART_CT_KIND_CLUSTER) $(CHART_CT_ANALYZER)
	$(KUBECTL) config use-context kind-$(CHART_CT_KIND_CLUSTER)

.PHONY: test-chart-analyzer-only
test-chart-analyzer-only: chart-ct-cluster-analyzer ## Install the chart with executor.enabled=false, no verifier namespace, one watched namespace and no action policy (only the analyzer image is built) and check that no executor or verifier object exists, a broken workload gets an Incident and no RemediationRequest is created.
	hack/chart-analyzer-only-install-test.sh "$(KUBECTL)" "$(HELM)"

.PHONY: test-chart-metrics-tls
test-chart-metrics-tls: chart-ct-cluster ## Install the chart with the metrics TLS trust profile on the chart-testing Kind cluster and scrape the metrics Services of all three components with the right CA (200), a wrong CA and a wrong server name (TLS failure).
	hack/chart-metrics-tls-test.sh "$(KUBECTL)" "$(HELM)" "$(CHART_CT_VALUES)"

.PHONY: chart-ct-kind-delete
chart-ct-kind-delete: kind ## Delete the Kind cluster of the chart-testing job.
	@$(KIND) delete cluster --name $(CHART_CT_KIND_CLUSTER)
