FROM golang:1.27 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not refetch the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/weft ./cmd/weft

# Distroless static: the controller opens no shell and reads no files beyond its
# service account token and CA bundle.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/weft /weft
USER 65532:65532
ENTRYPOINT ["/weft"]
