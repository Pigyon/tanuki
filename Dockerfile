# syntax=docker/dockerfile:1

# Cross-compiles from the build platform: Go needs no emulation, so an
# arm64 image builds at native speed on an amd64 runner.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=unknown

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
	-ldflags="-s -w \
	-X github.com/Pigyon/tanuki/internal/tanuki.version=${VERSION} \
	-X github.com/Pigyon/tanuki/internal/tanuki.commit=${COMMIT} \
	-X github.com/Pigyon/tanuki/internal/tanuki.buildDate=${BUILD_DATE}" \
	-trimpath -o tanuki ./cmd/tanuki

FROM scratch

ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="tanuki" \
	org.opencontainers.image.description="Transparent anonymization proxy for AI-assisted security testing" \
	org.opencontainers.image.source="https://github.com/Pigyon/tanuki" \
	org.opencontainers.image.url="https://github.com/Pigyon/tanuki" \
	org.opencontainers.image.documentation="https://github.com/Pigyon/tanuki#readme" \
	org.opencontainers.image.licenses="Apache-2.0" \
	org.opencontainers.image.version="${VERSION}" \
	org.opencontainers.image.revision="${COMMIT}" \
	org.opencontainers.image.created="${BUILD_DATE}" \
	org.opencontainers.image.base.name="scratch"

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /build/tanuki /tanuki
WORKDIR /workspace
ENV TANUKI_DATA="/data"
ENV TANUKI_PROXY_PORT="18080"
EXPOSE 18080
ENTRYPOINT ["/tanuki"]
CMD ["proxy"]
