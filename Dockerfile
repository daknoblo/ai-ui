# syntax=docker/dockerfile:1

# ---- Build-Stufe ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Abhängigkeiten zuerst (besseres Layer-Caching).
COPY go.mod go.sum ./
RUN go mod download

# Quellcode kopieren und statisches Binary bauen (CGO-frei dank modernc.org/sqlite).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ai-ui .

# ---- Laufzeit-Stufe ----
FROM alpine:3.21

# CA-Zertifikate für HTTPS-Aufrufe an Azure.
RUN apk add --no-cache ca-certificates tzdata && \
	addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/ai-ui /app/ai-ui

# Persistenter Datenpfad (Chats, Dokumente, Konfiguration).
RUN mkdir -p /data && chown -R app:app /data
VOLUME ["/data"]

USER app

ENV PORT=8080 \
	DATA_DIR=/data

EXPOSE 8080

ENTRYPOINT ["/app/ai-ui"]
