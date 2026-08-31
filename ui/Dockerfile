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
FROM node:22-alpine AS build
WORKDIR /src

# Install with the lockfile for reproducible dependency resolution.
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY . .
RUN npm run build

# --- serve ---
FROM nginx:alpine

# Unprivileged-friendly config: listens on 8080, pid + all temp dirs under
# /tmp, logs on stdout/stderr (the image symlinks /var/log/nginx/* there).
# --chmod=0644 keeps it readable by the nginx user regardless of the umask
# of whoever runs the build.
COPY --chmod=0644 nginx.conf /etc/nginx/nginx.conf

COPY --from=build /src/dist /usr/share/nginx/html

EXPOSE 8080
