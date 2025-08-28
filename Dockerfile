FROM mirror.star-home.top:4430/library/node:20-alpine AS ui-deps
WORKDIR /app/ui

COPY ui/package.json ui/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund

FROM ui-deps AS ui-build
WORKDIR /app/ui
COPY ui/ ./
RUN --mount=type=cache,target=/root/.npm \
    --mount=type=cache,target=/app/ui/.next/cache \
    npm run build

FROM mirror.star-home.top:4430/library/golang:1.25-trixie AS go-build
WORKDIR /app

RUN sed -i 's/deb.debian.org/mirrors.zju.edu.cn/g' /etc/apt/sources.list.d/debian.sources
RUN apt update && apt install -y git build-essential

COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .

COPY --from=ui-build /app/ui/dist ./ui/dist

RUN go build -o /out/mirrorgo ./

FROM mirror.star-home.top:4430/library/debian:trixie-slim AS runtime
WORKDIR /

USER 0:0

VOLUME ["/logs", "/Configs"]

COPY --from=go-build /out/mirrorgo /mirrorgo

EXPOSE 8080

ENTRYPOINT ["/mirrorgo"]


