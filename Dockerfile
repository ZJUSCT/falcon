FROM golang:1.26.5 AS go-build
WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/falcon-controller ./cmd/controller

FROM scratch AS runtime
COPY --from=go-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=go-build /out/falcon-controller /falcon-controller

USER 65532:65532
EXPOSE 8080 8081 8082
ENTRYPOINT ["/falcon-controller"]
