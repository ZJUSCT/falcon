# Multi-stage build for the Falcon admin UI.
#
# Stage 1 builds the Next.js static export (output: 'export' -> dist/).
# Stage 2 serves those files with nginx — static files ONLY. The go-staging
# gateway routes /api/jobs and /api/repos/ to the controller service and
# everything else to this image, so there is deliberately no proxy config
# here: the UI's fetch('/api/...') calls are same-origin at the gateway.
#
# Build:  docker build -t ghcr.io/zjusct/falcon-ui:<short-sha> .
# Run:    docker run -p 8080:8080 ...   (nginx listens on 8080, /healthz -> 200 ok)

# --- build ---
FROM node:22-slim AS build
WORKDIR /src

# Install with the lockfile for reproducible dependency resolution.
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY . .
RUN npm run build

# --- serve ---
# nginx unprivileged (uid 101) mainline with the OpenTelemetry module
# bundled: it is inert unless the config loads it, and the image's defaults
# (port 8080, pid/temp under /tmp) match nginx.conf below.
FROM nginxinc/nginx-unprivileged:mainline-alpine-otel

# Runs as uid/gid 101 (set by the chart) with a read-only root filesystem:
# pid file and every temp path live under /tmp, logs on stdout/stderr.
# --chmod=0644 keeps the config readable regardless of the umask of whoever
# runs the build.
COPY --chmod=0644 nginx.conf /etc/nginx/nginx.conf

COPY --from=build /src/dist /usr/share/nginx/html

EXPOSE 8080
