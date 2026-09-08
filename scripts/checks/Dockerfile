# Tool versions and image digests are shared by local checks and CI.
FROM golangci/golangci-lint:v2.13.2@sha256:ba07dffad130794ae79ebaa0056809d18c0168f3f846480ffd3eb6c04578b83d AS lint
FROM alpine/helm:3.16.4@sha256:9b25e60ae264940b276e32866d37e3088e70c4e2d1784b964dc3f90346281a74 AS helm

FROM golang:1.26.5@sha256:705e964a93a2fd2e75c7d59bb7d781b57e30f12293ffde5175c69229e18fb678 AS go-tools
COPY --from=lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOBIN=/usr/local/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
ENV GOTOOLCHAIN=local GOFLAGS=-mod=readonly

FROM node:22.23.2-bookworm@sha256:0557ac14e0d45d02ed563067b82856ca5e7aa3437fa28d98d4350ea9c3d9494a AS ui-tools
ENV NEXT_TELEMETRY_DISABLED=1

FROM python:3.12-slim@sha256:2c941e860699f878900b0edc2403613c234d4b32eda3cc9fa7036991a2a63c4a AS manifest-tools
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
RUN pip install --no-cache-dir pre-commit==4.3.0 PyYAML==6.0.2
