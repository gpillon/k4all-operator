# The k4all metadata server (cmd/metadata-server): one static binary, which
# the operator's admission policy runs in each served VM's virt-launcher pod
# twice: as the init container that installs the 169.254.169.254 redirect
# (root, NET_ADMIN only) and as the server (uid 107, no capabilities,
# read-only root). Tagged as the operator; built by build-operator.yml.
FROM golang:1.24 AS builder
LABEL org.opencontainers.image.source="https://github.com/gpillon/k4all-operator"
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
COPY cmd/metadata-server/ cmd/metadata-server/
COPY internal/metadata/ internal/metadata/
# go build fetches only the modules the server imports (not the operator's).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o metadata-server ./cmd/metadata-server

FROM scratch
COPY --from=builder /workspace/metadata-server /metadata-server
USER 107:107
ENTRYPOINT ["/metadata-server"]
