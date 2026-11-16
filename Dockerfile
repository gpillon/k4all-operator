FROM golang:1.24 AS builder
LABEL org.opencontainers.image.source = "https://github.com/gpillon/k4all-operator"
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
# Kept as a regular layer (no cache mount) so the module download is reused
# from the image layer cache (e.g. type=gha in CI) as long as go.sum is unchanged.
RUN go mod download

COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# The Go build cache lives in a BuildKit cache mount: incremental rebuilds only
# recompile changed packages. In CI it is persisted with buildkit-cache-dance.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOCACHE=/root/.cache/go-build \
    go build -o manager cmd/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
