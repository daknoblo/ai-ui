# Copilot instructions — ai-ui

This file describes the binding conventions, best practices and security
requirements for the **ai-ui** project. GitHub Copilot must follow them for
code, the Dockerfile, GitHub Actions, tests and documentation.

> **Context:** ai-ui is a small web UI for chats and documents with RAG/web
> search support. The application runs as a single Go binary in Docker and
> stores chats, documents, embeddings and configuration in SQLite under the
> data path.

## 1. Language, runtime & core principles

- **Language: Go 1.26**; do not lower the Go version in `go.mod`, the Dockerfile
  or the workflows.
- **Module path:** `github.com/daknoblo/ai-ui`.
- **All code comments, documentation, error strings and log messages are in
  English.** Identifiers are English as well. User-facing UI text is never
  hard-coded; it goes through `internal/i18n`.
- **A single, static binary** as the deliverable; always build with
  `CGO_ENABLED=0`.
- Use the pure-Go SQLite implementation `modernc.org/sqlite`.
- Standard library first; add new dependencies only when they provide clear
  value.
- Secrets/API keys are read exclusively from environment variables and are never
  committed to configuration files or source code.

## 2. Project structure

```
ai-ui/
├── main.go                       # entry point: env, wiring, server, shutdown
├── cmd/
│   ├── demo/                     # demo instance with a stub backend (no API key)
│   └── site/                     # generator of the GitHub Pages website
├── internal/
│   ├── config/                   # configuration from env and JSON file
│   ├── demo/                     # stub backend + seeded demo content
│   ├── docparse/                 # plain text extraction from uploads
│   ├── i18n/                     # UI message catalog (en/de)
│   ├── llm/                      # Azure/OpenAI compatible client
│   ├── rag/                      # document ingestion and retrieval
│   ├── server/                   # HTTP server, routing, handlers, SSE
│   ├── storage/                  # SQLite access and migrations
│   └── websearch/                # optional web search
├── tools/screenshots/            # Playwright capture of the documentation shots
├── docs/screenshots/             # generated screenshots + manifest (committed)
├── web/                          # embedded templates and static assets
├── Dockerfile
├── docker-compose.example.yml
├── go.mod / go.sum
├── .golangci.yml
└── .github/workflows/            # ci.yml, release.yml, codeql.yml, docs.yml
```

- `main.go` stays lean: argument parsing, configuration, dependencies, signal
  handling and graceful shutdown.
- Code that is not meant for public reuse lives under `internal/`.
- HTTP routes are registered centrally in `internal/server.(*Server).Routes()`.
- The demo is not part of the container image: the Dockerfile only builds the
  root package.

## 3. Configuration & environment variables

ai-ui does not use a project specific env prefix. The relevant variables are:

- `PORT` (default `8080`) — HTTP port and target of the local health check.
- `DATA_DIR` (default `/appdata`) — persistent data path. The SQLite database
  lives directly in it, the UI settings in `<DATA_DIR>/appdata/config.json`.
- `AZURE_API_KEY` — secret for the AI endpoint (chat), from env only.
- `AZURE_EMBEDDING_API_KEY` — optional dedicated embedding key.
- `SEARCH_API_KEY` — optional key for the web search provider.
- Optional endpoint overrides (they lock the matching UI field). Naming scheme:
  general AI endpoint = `AZURE_*` (`AZURE_ENDPOINT`, `AZURE_DEPLOYMENT`,
  `AZURE_MODELS`, `AZURE_API_VERSION`), embeddings = `AZURE_EMBEDDING_*`
  (`AZURE_EMBEDDING_ENDPOINT`, `AZURE_EMBEDDING_DEPLOYMENT`,
  `AZURE_EMBEDDING_API_VERSION`).
- `HEALTHCHECK_INTERVAL` — periodic connection check (`60s`, `0`/`off`).
- `TZ` — time zone (IANA name); the binary imports `time/tzdata` for the
  distroless container.

## 4. Internationalization

- Every string shown to a user lives in `internal/i18n`; both `en` and `de` must
  contain the same keys (enforced by `TestCatalogsHaveSameKeys`).
- Templates use the `t`, `lang`, `thousands` and `chatTitle` helpers; Go code
  uses `(*Server).t`. Both resolve the language from the configuration at call
  time, so no language field is threaded through the data structs.
- The default language of a fresh installation is English.

## 5. Docker

- Multi-stage Dockerfile with `golang:1.26-alpine` as the builder.
- Multi-arch via cross compilation using `--platform=$BUILDPLATFORM`, `TARGETOS`
  and `TARGETARCH`; set `CGO_ENABLED=0`.
- Runtime base: `gcr.io/distroless/static-debian12:nonroot`.
- Runs non-root with UID/GID `65532:65532`.
- The persistent data path is `/appdata`; the directory is prepared during the
  build and copied into the runtime image with `--chown=65532:65532`.
- Because distroless has no shell and no curl/wget, the binary implements the
  health check itself: `-healthcheck` calls
  `http://127.0.0.1:<PORT>/healthz` locally.
- The Dockerfile contains an exec-form health check line:
  `CMD ["/app/ai-ui", "-healthcheck"]`.

## 6. HTTP & health check

- `GET /healthz` returns HTTP 200 with the body `ok` and must not query any
  external service.
- The container health check uses the binary only.
- Long-lived SSE streams must not be aborted by overly tight write timeouts;
  `IdleTimeout` is set instead of `WriteTimeout`.
- Every response carries the security headers set in
  `internal/server/middleware.go` (CSP, nosniff, frame options, referrer policy).

## 7. GitHub Actions & dependencies

- Workflows target `main` only (no `develop` branch). Every push to `main`
  builds and publishes an image.
- Exactly these workflows belong under `.github/workflows/`:
  - `ci.yml`: gofmt check, `go vet ./...`, `golangci-lint` v2.12.2 via
    `golangci/golangci-lint-action@v9`, `govulncheck`, `go test -race ./...`,
    `CGO_ENABLED=0 go build ./...`.
  - `release.yml`: multi-arch Docker Buildx, GHCR push, SBOM, provenance,
    keyless cosign signature and Trivy SARIF upload.
  - `codeql.yml`: CodeQL for Go with `build-mode: autobuild`.
  - `docs.yml`: recaptures the demo screenshots with Playwright, commits them to
    `docs/screenshots` (`[skip ci]`, so no workflow loop), builds the website
    with `go run ./cmd/site` and deploys it to GitHub Pages.
- Dependabot watches `gomod`, `github-actions`, `docker` and the `npm` project
  in `tools/screenshots` weekly.
- Always pin actions to stable major/version tags; never use `@master` or
  `@main`.

## 8. Linting, formatting & tests

- Code is always `gofmt` formatted (`gofmt -l .` must be empty).
- `go vet ./...`, `CGO_ENABLED=0 go build ./...` and `go test ./...` must pass
  locally; CI deliberately runs the tests with `-race`.
- `.golangci.yml` is the central golangci-lint configuration. On top of the
  standard set it enables `gosec`, `bodyclose`, `errorlint`, `misspell` and
  `unconvert`.
- Handle errors; deliberately ignored errors (e.g. `Close()`, SSE writes to a
  disconnected client) need a short comment explaining why.
- `#nosec` suppressions always carry a justification.

## 9. Security

- Minimal attack surface: static binary, distroless, non-root.
- Never commit secrets; `.env` files and local data stay out of the repository.
- Run SQL exclusively with bound parameters; never concatenate user input into
  queries.
- Limit the size of file uploads and validate them before processing; document
  parsing is bounded against decompression bombs.
- User supplied outbound URLs (SearXNG) are validated and the dialer rejects
  loopback and link-local destinations.
- Render model and document content as sanitized Markdown; goldmark must stay
  configured without `WithUnsafe`.
- The app is meant to run inside a trusted network or behind a reverse
  proxy/VPN and should not be exposed to the internet unprotected.
- Container images are signed and scanned by Trivy for CRITICAL/HIGH findings.

## 10. Demo, screenshots & website

- `internal/demo` holds a stub of the Azure-compatible endpoints plus the seeded
  demo content; the conversations are Markdown files under
  `internal/demo/content/<lang>/` (embedded with `all:` so `_reply.md` is
  included). Every language must provide the same section keys - a test enforces
  that.
- `cmd/demo` starts the real server against that stub, writes `demo-index.json`
  into the data path and needs no credentials.
- `tools/screenshots/capture.mjs` captures one PNG per section and language plus
  a manifest; new UI sections belong in its shot list.
- `cmd/site` generates the website from `README.md` and that manifest. The
  README stays the single source of the documentation - there is no second copy
  of the docs to maintain.

## 11. Definition of done for changes

1. `gofmt -l .` is empty.
2. `go vet ./...`, `golangci-lint run ./...`, `CGO_ENABLED=0 go build ./...` and
   `go test -race ./...` pass.
3. New user-facing strings exist in every language of `internal/i18n`.
4. CI, release, CodeQL and Dependabot configuration stay consistent.
5. Docker stays distroless, non-root, multi-arch capable and uses the binary
   health check.
6. No secrets or local data are committed.
7. A user visible feature is reflected in the README and, when it adds a new
   part of the interface, in the screenshot list of `tools/screenshots`.
