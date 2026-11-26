# syntax=docker/dockerfile:1

# Build on the machine's own architecture and cross-compile from there.
# Without --platform=$BUILDPLATFORM, an arm64 image compiles every file under
# QEMU: same output, many times the wall clock, and no benefit.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG VERSION=dev
# BuildKit fills these in from the platform being built: the host's own for a
# plain `docker build`, each entry of --platform for buildx. Only the legacy
# builder leaves them empty, where Go falls back to its own defaults.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/crawlping .

# distroless rather than scratch: the binary calls https://api.indexnow.org and
# https://<host>/<key>.txt, so it needs a CA bundle. This image also ships a
# nonroot user and a healthcheck-friendly layout.
FROM gcr.io/distroless/static-debian12:nonroot

# Redeclared because a build arg does not cross stages. The version label is
# what makes `docker inspect` answer "which build is this".
ARG VERSION=dev

# The source label is not decoration: a package linked to its repository at
# publish time inherits the repository's permissions. Adding it later means
# linking by hand in the package settings.
LABEL org.opencontainers.image.title="crawlping" \
      org.opencontainers.image.description="Submit URLs to IndexNow from a machine you control" \
      org.opencontainers.image.source="https://github.com/rokcso/crawlping" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/crawlping /crawlping

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/crawlping", "healthcheck", "-config", "/etc/crawlping/config.yaml"]

ENTRYPOINT ["/crawlping"]
CMD ["serve", "-config", "/etc/crawlping/config.yaml"]
