FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not refetch the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# Stamped in at link time so a running controller can say what it is.
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/alethic/weft/internal/version.Version=${VERSION} \
        -X github.com/alethic/weft/internal/version.Commit=${COMMIT} \
        -X github.com/alethic/weft/internal/version.Date=${DATE}" \
      -o /out/weft ./cmd/weft

# Distroless static: the controller opens no shell and reads no files beyond its
# service account token and the CA bundle.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/weft /weft
USER 65532:65532
ENTRYPOINT ["/weft"]
