# Tool versions and image digests are shared by local checks and CI.
FROM golangci/golangci-lint:v2.13.2 AS lint
FROM alpine/helm:3.16.4 AS helm

FROM golang:1.26.5 AS go-tools
COPY --from=lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOBIN=/usr/local/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
ENV GOTOOLCHAIN=local GOFLAGS=-mod=readonly

FROM node:22.23.2-bookworm AS ui-tools
ENV NEXT_TELEMETRY_DISABLED=1

FROM python:3.12-slim AS manifest-tools
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
RUN pip install --no-cache-dir pre-commit==4.3.0 PyYAML==6.0.2

# E2E runner: kind, kubectl, the docker CLI, helm, chainsaw and jq, talking
# to the host Docker daemon through the mounted socket (kind shells out to
# `docker` for daemon inspection). The scenario asserts with chainsaw (see
# tests/e2e); cluster dependencies live in scripts/e2e/cluster as a
# kustomization and are applied by kubectl at run time.
FROM debian:bookworm-slim AS e2e-tools
ARG KIND_VERSION=v0.33.0
ARG KUBECTL_VERSION=v1.36.4
ARG DOCKER_VERSION=28.3.3
ARG CHAINSAW_VERSION=v0.2.15
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl jq git \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "https://github.com/kyverno/chainsaw/releases/download/${CHAINSAW_VERSION}/chainsaw_linux_amd64.tar.gz" -o /tmp/chainsaw.tgz \
    && tar -xzf /tmp/chainsaw.tgz -C /tmp chainsaw \
    && mv /tmp/chainsaw /usr/local/bin/chainsaw \
    && rm -f /tmp/chainsaw.tgz \
    && curl -fsSLo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64" \
    && chmod +x /usr/local/bin/kind \
    && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && curl -fsSL "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz" -o /tmp/docker.tgz \
    && tar -xzf /tmp/docker.tgz -C /tmp docker/docker \
    && mv /tmp/docker/docker /usr/local/bin/docker \
    && rm -rf /tmp/docker /tmp/docker.tgz
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
