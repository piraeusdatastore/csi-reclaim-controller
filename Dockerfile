FROM --platform=$BUILDPLATFORM golang:1 AS builder
ARG VERSION=0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-X github.com/piraeusdatastore/csi-reclaim-controller/pkg/metadata.Version=${VERSION}" \
    -o /csi-reclaim-controller \
    ./cmd/csi-reclaim-controller

FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY --from=builder /csi-reclaim-controller /csi-reclaim-controller
ENTRYPOINT ["/csi-reclaim-controller"]
