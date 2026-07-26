# syntax=docker/dockerfile:1

# ---- Build stage ----
# The builder ALWAYS runs natively on the build platform ($BUILDPLATFORM) and
# cross compiles for the target architecture ($TARGETARCH). This avoids slow
# QEMU emulation for multi-arch builds (Go cross compiles fine without CGO).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Dependencies first (better layer caching). The module cache is mounted as a
# persistent BuildKit cache and survives individual builds.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
	go mod download

# Copy the sources and cross compile a static binary.
# The Go build cache is persisted separately per target architecture.
ARG TARGETOS
ARG TARGETARCH
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
	CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags="-s -w" -o /out/ai-ui .

# Prepare the data directory - distroless has no shell for mkdir/chown, so it is
# created here and assigned to the non-root user (65532) during COPY.
RUN mkdir -p /appdata

# ---- Runtime stage (distroless) ----
# static-debian12:nonroot ships ca-certificates plus tzdata and a non-root user
# (uid/gid 65532). A good match for a statically linked, CGO-free binary.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /out/ai-ui /app/ai-ui
COPY --from=builder --chown=65532:65532 /appdata /appdata

# Persistent data path (chats, documents, configuration).
VOLUME ["/appdata"]

# 65532 = "nonroot" in the distroless image.
USER 65532:65532

ENV PORT=8080 \
	DATA_DIR=/appdata

EXPOSE 8080

# Distroless has neither a shell nor curl, so the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD ["/app/ai-ui", "-healthcheck"]

ENTRYPOINT ["/app/ai-ui"]
