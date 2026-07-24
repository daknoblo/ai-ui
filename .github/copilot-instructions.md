# Copilot-Instruktionen — ai-ui

Diese Datei beschreibt die verbindlichen Konventionen, Best Practices und
Sicherheitsvorgaben für das Projekt **ai-ui**. GitHub Copilot soll sich bei
Code, Dockerfile, GitHub Actions, Tests und Dokumentation an diese Vorgaben
halten.

> **Kontext:** ai-ui ist eine kleine Web-UI für Chats und Dokumente mit
> RAG-/Websuche-Unterstützung. Die Anwendung läuft als einzelnes Go-Binary in
> Docker und speichert Chats, Dokumente, Embeddings und Konfiguration in SQLite
> unter `/data`.

## 1. Sprache, Runtime & Grundprinzipien

- **Sprache: Go 1.26**; die Go-Version in `go.mod`, Dockerfile und Workflows
  nicht auf eine ältere Version senken.
- **Module-Pfad:** `github.com/daknoblo/ai-ui`.
- **Kommentare und Dokumentation auf Deutsch**, Code-Bezeichner auf Englisch.
- **Ein einzelnes, statisches Binary** als Auslieferungsartefakt; immer
  `CGO_ENABLED=0` bauen.
- Für SQLite die reine-Go-Implementierung `modernc.org/sqlite` verwenden.
- Standardbibliothek zuerst; neue Abhängigkeiten nur einführen, wenn sie klaren
  Mehrwert bieten.
- Secrets/API-Keys werden ausschließlich über Umgebungsvariablen eingelesen und
  niemals in Konfigurationsdateien oder Quellcode committet.

## 2. Projektstruktur

```
ai-ui/
├── main.go                       # Einstiegspunkt: Env, Wiring, Server, Shutdown
├── internal/
│   ├── config/                   # Konfiguration aus Env und JSON-Datei
│   ├── llm/                      # Azure/OpenAI-kompatibler Client
│   ├── rag/                      # Dokument-Ingestion und Retrieval
│   ├── server/                   # HTTP-Server, Routing, Handler, SSE
│   ├── storage/                  # SQLite-Zugriff und Migrationen
│   └── websearch/                # Optionale Websuche
├── web/                          # eingebettete Templates und statische Assets
├── Dockerfile
├── docker-compose.example.yml
├── go.mod / go.sum
├── .golangci.yml
└── .github/workflows/            # ci.yml, release.yml, codeql.yml
```

- `main.go` bleibt schlank: Argument-Parsing, Konfiguration, Abhängigkeiten,
  Signal-Handling und Graceful Shutdown.
- Nicht öffentlich wiederverwendbarer Code gehört unter `internal/`.
- HTTP-Routen werden zentral in `internal/server.(*Server).Routes()` registriert.

## 3. Konfiguration & Env-Variablen

ai-ui nutzt keinen projektspezifischen Env-Präfix. Wichtige Variablen:

- `PORT` (Default `8080`) — HTTP-Port und Ziel des lokalen Healthchecks.
- `DATA_DIR` (Default `/data`) — persistenter Datenpfad für SQLite und Appdaten.
- `AZURE_API_KEY` — Secret für Chat-/Embedding-Zugriffe, nur aus ENV.
- `AZURE_EMBEDDING_API_KEY` — optional eigener Embedding-Key.
- `SEARCH_API_KEY` — optionaler Key für Websuche-Anbieter.
- `HEALTHCHECK_INTERVAL` — periodische Verbindungsprüfung (`60s`, `0`/`off`).
- `TZ` — Zeitzone (IANA-Name); das Binary importiert `time/tzdata` für
  distroless-Container.

## 4. Docker

- Mehrstufiges Dockerfile mit `golang:1.26-alpine` als Builder.
- Multi-Arch per Cross-Compile mit `--platform=$BUILDPLATFORM`, `TARGETOS` und
  `TARGETARCH`; `CGO_ENABLED=0` setzen.
- Runtime-Basis: `gcr.io/distroless/static-debian12:nonroot`.
- Non-root-Betrieb mit UID/GID `65532:65532`.
- Persistenter Datenpfad ist `/data`; das Verzeichnis wird im Build vorbereitet
  und mit `--chown=65532:65532` in das Runtime-Image kopiert.
- Da distroless keine Shell und kein curl/wget enthält, muss das Binary den
  Healthcheck selbst implementieren: `-healthcheck` ruft lokal
  `http://127.0.0.1:<PORT>/healthz` auf.
- Dockerfile enthält eine Exec-Form-Healthcheck-Zeile:
  `CMD ["/app/ai-ui", "-healthcheck"]`.

## 5. HTTP & Healthcheck

- `GET /healthz` liefert HTTP 200 mit Body `ok` und darf keine externen Dienste
  abfragen.
- Der Container-Healthcheck verwendet ausschließlich das eigene Binary.
- Langlebige SSE-Streams dürfen nicht durch zu enge Write-Timeouts abgebrochen
  werden.

## 6. GitHub Actions & Abhängigkeiten

- Workflows sind ausschließlich auf `main` ausgerichtet (kein `develop`-Branch).
  Jeder Push auf `main` baut und veröffentlicht direkt ein Image.
- Genau diese Workflows gehören unter `.github/workflows/`:
  - `ci.yml`: gofmt-Check, `go vet ./...`, `golangci-lint` v2.12.2 über
    `golangci/golangci-lint-action@v9`, `govulncheck`, `go test -race ./...`,
    `CGO_ENABLED=0 go build ./...`.
  - `release.yml`: Multi-Arch Docker Buildx, GHCR-Push, SBOM, Provenance,
    Cosign-Keyless-Signatur und Trivy-SARIF-Upload.
  - `codeql.yml`: CodeQL für Go mit `build-mode: autobuild`.
- Dependabot überwacht `gomod`, `github-actions` und `docker` wöchentlich.
- Actions immer auf stabile Major-/Version-Tags pinnen; keine `@master` oder
  `@main` verwenden.

## 7. Linting, Formatierung & Tests

- Code ist immer `gofmt`-formatiert (`gofmt -l .` muss leer sein).
- `go vet ./...`, `CGO_ENABLED=0 go build ./...` und `go test ./...` müssen
  lokal grün sein; in CI laufen Tests bewusst mit `-race`.
- `.golangci.yml` ist die zentrale golangci-lint-Konfiguration.
- Fehler grundsätzlich behandeln; bewusst ignorierte `Close()`-Fehler nur
  gezielt und nachvollziehbar ignorieren.

## 8. Sicherheit

- Minimale Angriffsfläche: statisches Binary, distroless, non-root.
- Secrets niemals committen; `.env`-Dateien und lokale Daten bleiben außerhalb
  des Repos.
- SQL ausschließlich parametrisiert ausführen; keine String-Konkatenation von
  Nutzereingaben in Queries.
- Datei-Uploads größenbegrenzen und vor Verarbeitung validieren.
- Die App ist für den Betrieb in einem vertrauenswürdigen Netz bzw. hinter
  Reverse-Proxy/VPN gedacht und sollte nicht ungeschützt im Internet hängen.
- Container-Images werden signiert und per Trivy auf CRITICAL/HIGH-Findings
  gescannt.

## 9. Definition of Done für Änderungen

1. `gofmt -l .` ist leer.
2. `go vet ./...`, `CGO_ENABLED=0 go build ./...` und `go test ./...` sind grün.
3. CI-, Release-, CodeQL- und Dependabot-Konfiguration bleiben konsistent.
4. Docker bleibt distroless, non-root, Multi-Arch-fähig und nutzt den Binary-
   Healthcheck.
5. Keine Secrets oder lokalen Daten werden committet.
