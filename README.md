# ai-ui

Eine schlanke, selbst gehostete ChatGPT-ähnliche Weboberfläche in Go mit
Dokumenten-Kontext (RAG), angebunden an einen Azure-Foundry-Model-Router
(Azure-OpenAI-kompatibel).

## Funktionen

- Chat-Oberfläche mit Seitenleiste, mehreren Konversationen und Verlauf
- Antwort-Streaming (Token für Token) via Server-Sent Events
- Dokumenten-Upload (Text/Markdown, PDF, DOCX) als RAG-Kontext
  (Embeddings + Brute-Force-Cosine-Suche)
- Konfigurationsdialog in der UI (Endpoint, Deployments, API-Version,
  System-Prompt, Temperatur)
- API-Key ausschließlich über die Umgebungsvariable `AZURE_API_KEY`
- Persistenz in SQLite unter dem gemounteten Datenpfad
- Single-Binary, einzelnes Docker-Image (alpine), Betrieb hinter Traefik

## Architektur

- **Go** + `chi`-Router, `html/template` + **HTMX** (server-gerendert)
- **SQLite** (`modernc.org/sqlite`, CGO-frei) für Chats, Nachrichten,
  Dokumente und Embeddings
- **goldmark** für Markdown-Rendering
- RAG: Chunking → Embeddings → Kosinus-Ähnlichkeit (Top-k)

## Konfiguration

| Variable        | Default  | Beschreibung                                  |
| --------------- | -------- | --------------------------------------------- |
| `AZURE_API_KEY` | –        | **Secret.** API-Key des Model-Routers.        |
| `DATA_DIR`      | `/data`  | Persistenter Datenpfad (DB + `appdata/`).     |
| `PORT`          | `8080`   | HTTP-Port.                                    |

Die übrigen Einstellungen werden im UI-Dialog gesetzt und unter
`<DATA_DIR>/appdata/config.json` gespeichert (ohne Secret).

## Lokal starten

```sh
export AZURE_API_KEY=dein-key
DATA_DIR=./data PORT=8080 go run .
# http://localhost:8080
```

## Docker

```sh
docker build -t ai-ui .
docker run --rm -p 8080:8080 \
  -e AZURE_API_KEY=dein-key \
  -v ai-ui-data:/data \
  ai-ui
```

## Deployment hinter Traefik

Siehe [docker-compose.example.yml](docker-compose.example.yml). Das Image wird
per GitHub Actions nach `ghcr.io/daknoblo/ai-ui` gebaut und veröffentlicht
(Push auf `main` sowie `v*`-Tags).