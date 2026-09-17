FROM golang:1.26.5 AS go-build
WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# FALCON_VERSION is the release git tag (set by the release workflow); the
# binary reports it via GET /api/version and unstamped builds report "dev".
# Double quotes matter: ARG reaches the shell as an env var, so the shell
# (not the Dockerfile parser) must expand it.
ARG FALCON_VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${FALCON_VERSION}" -o /out/falcon-controller ./cmd/controller

FROM scratch AS runtime
COPY --from=go-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=go-build /out/falcon-controller /falcon-controller

USER 65532:65532
EXPOSE 8080 8081 8082
ENTRYPOINT ["/falcon-controller"]
