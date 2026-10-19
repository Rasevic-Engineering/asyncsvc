# One image: the Next.js static export plus the Go binaries (/bin/api, /bin/worker,
# /bin/devsetup). Cloud Run / Kubernetes choose the binary via the command.
# Build context is the repo root.

FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM golang:1.26 AS build
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ ./
# Git commit baked into the binaries, reported by GET /version.
ARG GIT_SHA=dev
RUN LDFLAGS="-s -w -X github.com/example/asyncsvc/internal/version.Commit=${GIT_SHA}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/ingester ./cmd/ingester \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/devsetup ./cmd/devsetup \
 && mkdir -p /data/reports

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /bin/
COPY --from=web /web/out /web
# Writable by the nonroot user (uid 65532). A fresh named volume mounted here
# inherits this ownership, so local report files can be written.
COPY --from=build --chown=65532:65532 /data /data
# The API serves the web app from here.
ENV WEB_DIR=/web
USER nonroot
ENTRYPOINT ["/bin/api"]
