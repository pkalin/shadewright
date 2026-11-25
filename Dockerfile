# Builds the executor (manager), analyzer and verifier images from one Go build.
# Targets: executor, analyzer, verifier (one binary each, what CI publishes) and local (all binaries; the default, so a
# plain docker build keeps working for make deploy and the e2e tests).
# Base images are pinned by tag and digest; Dependabot (docker ecosystem) keeps both current.
# To build from a mirror, override a base with a named build context whose name is the FROM reference below,
# copied exactly (tag and digest; it changes with every Dependabot bump, and a name that does not match is
# silently ignored):
#   make docker-build DOCKER_BUILD_ARGS='--build-context <FROM reference>=docker-image://<mirror>/<image>'
# The builder always runs on the build host and cross-compiles (CGO is off), so a multi-arch build needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder
ARG TARGETOS
ARG TARGETARCH

# Enforce read-only module resolution and disable Go workspaces in the builder stage (ADR-0048).
ENV GOFLAGS=-mod=readonly \
    GOWORK=off

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o manager cmd/main.go && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o analyzer ./cmd/analyzer && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o verifier ./cmd/verifier

# Distroless static, nonroot: no shell or package manager, and it runs as 65532, so the pod can set
# runAsNonRoot and readOnlyRootFilesystem (the binaries write nothing to disk).
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS executor
WORKDIR /
COPY --from=builder /workspace/LICENSE /workspace/NOTICE /licenses/
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]

FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS analyzer
WORKDIR /
COPY --from=builder /workspace/LICENSE /workspace/NOTICE /licenses/
COPY --from=builder /workspace/analyzer .
USER 65532:65532
ENTRYPOINT ["/analyzer"]

FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS verifier
WORKDIR /
COPY --from=builder /workspace/LICENSE /workspace/NOTICE /licenses/
COPY --from=builder /workspace/verifier .
USER 65532:65532
ENTRYPOINT ["/verifier"]

# All binaries, for config/manager (the analyzer and verifier Deployments set their own command). Last, so it is the default target.
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS local
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/analyzer .
COPY --from=builder /workspace/verifier .
COPY --from=builder /workspace/LICENSE /workspace/NOTICE /licenses/
USER 65532:65532
ENTRYPOINT ["/manager"]
