# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
# The build stage always runs on the builder's own architecture and Go
# cross-compiles for the target. Without --platform, a multi-arch build would
# run the whole Go toolchain under QEMU for arm64, which is many times slower.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied first so that editing source does not invalidate the
# download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

# CGO off makes the binary fully static, which is what lets the final stage be
# a scratch-like image. -trimpath strips local build paths, and -s -w drops the
# symbol table and DWARF data.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/llmtap ./cmd/llmtap

# ---- runtime ---------------------------------------------------------------
# distroless/static carries a CA bundle (needed to reach the provider APIs over
# TLS) and nothing else: no shell, no package manager, nothing to exploit.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/llmtap /usr/local/bin/llmtap
COPY configs/pricing.yaml /etc/llmtap/pricing.yaml

ENV LLMTAP_PRICING=/etc/llmtap/pricing.yaml \
    LLMTAP_ADDR=:8080

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/llmtap"]
