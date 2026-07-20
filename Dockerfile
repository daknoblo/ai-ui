# syntax=docker/dockerfile:1

# ---- Build-Stufe ----
# Der Builder läuft IMMER nativ auf der Build-Plattform ($BUILDPLATFORM) und
# cross-kompiliert für die Zielarchitektur ($TARGETARCH). Dadurch entfällt die
# langsame QEMU-Emulation bei Multi-Arch-Builds (Go cross-kompiliert CGO-frei).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Abhängigkeiten zuerst (besseres Layer-Caching). Der Modul-Cache wird als
# persistenter BuildKit-Cache gemountet und überlebt einzelne Builds.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
	go mod download

# Quellcode kopieren und statisches Binary cross-kompilieren.
# Der Go-Build-Cache wird pro Zielarchitektur getrennt persistiert.
ARG TARGETOS
ARG TARGETARCH
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
	CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags="-s -w" -o /out/ai-ui .

# Datenverzeichnis vorbereiten – Distroless hat keine Shell für mkdir/chown,
# deshalb wird es hier angelegt und beim COPY mit dem non-root-User (65532) belegt.
RUN mkdir -p /appdata

# ---- Laufzeit-Stufe (Distroless) ----
# static-debian12:nonroot enthält ca-certificates + tzdata und einen non-root
# User (uid/gid 65532). Passend für ein statisch gelinktes, CGO-freies Binary.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /out/ai-ui /app/ai-ui
COPY --from=builder --chown=65532:65532 /appdata /appdata

# Persistenter Datenpfad (Chats, Dokumente, Konfiguration).
VOLUME ["/appdata"]

# 65532 = "nonroot" im Distroless-Image.
USER 65532:65532

ENV PORT=8080 \
	DATA_DIR=/appdata

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD ["/app/ai-ui", "-healthcheck"]

ENTRYPOINT ["/app/ai-ui"]
