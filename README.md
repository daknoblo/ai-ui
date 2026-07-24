# ai-ui

Eine schlanke, selbst gehostete ChatGPT-ähnliche Weboberfläche in Go mit
Dokumenten-Kontext (RAG), angebunden an einen Azure-Foundry-Model-Router
(Azure-OpenAI-kompatibel).

## Funktionen

- Chat-Oberfläche mit Seitenleiste, mehreren Konversationen und Verlauf
- Antwort-Streaming (Token für Token) via Server-Sent Events
- Modellauswahl oben rechts im Chatfenster (gepflegte Liste; "Auto" lässt den
  Router entscheiden); die Auswahl gilt global und bleibt beim Chatwechsel erhalten
- Dokumenten-Upload (Text/Markdown, PDF, DOCX) als RAG-Kontext
  (Embeddings + Brute-Force-Cosine-Suche)
- Dokumente direkt am Eingabefeld anhängen (📎) oder per Drag & Drop ins
  Chatfenster ziehen; angehängte Dokumente werden als Chips über der Eingabe gezeigt
- Optionale Web-Suche (🌐) pro Anfrage: bezieht aktuelle Online-Ergebnisse als
  Kontext ein – provider-agnostisch (Tavily, Brave Search, SearXNG)
- Dokumente sind an den jeweiligen Chat gebunden und werden beim Löschen des
  Chats automatisch mit entfernt (inkl. Embeddings)
- Konfigurationsdialog in der UI (Endpoint, Deployments, API-Version,
  System-Prompt, Temperatur, Modell-Liste)
- Bereitschafts-/Verbindungsprüfung: Uploads sind erst möglich, wenn Speicher
  und Embedding-Endpoint verifiziert sind; Prüfung beim Start und periodisch im
  Hintergrund, mit Statusanzeige in der Seitenleiste
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
| `AZURE_API_KEY` | –        | **Secret.** API-Key des AI-Endpoints (Chat). |
| `AZURE_EMBEDDING_API_KEY` | – | **Secret, optional.** Eigener Key, falls Embeddings auf einer separaten Azure-Ressource liegen. Leer ⇒ `AZURE_API_KEY` wird genutzt. |
| `SEARCH_API_KEY` | – | **Secret, optional.** API-Key für die Web-Suche (Tavily oder Brave). Für SearXNG nicht erforderlich. |
| `DATA_DIR`      | `/appdata`  | Persistenter Datenpfad (DB + `appdata/`).     |
| `PORT`          | `8080`   | HTTP-Port.                                    |
| `HEALTHCHECK_INTERVAL` | `60s` | Intervall der periodischen Verbindungsprüfung (Go-Dauer, z.B. `30s`, `2m`). `0` oder `off` deaktiviert den periodischen Check (die Prüfung beim Start läuft weiterhin). |

Die übrigen Einstellungen werden im UI-Dialog gesetzt und unter
`<DATA_DIR>/appdata/config.json` gespeichert (ohne Secret). Der generelle
AI-Endpoint und die Embeddings können getrennte Endpoints, Deployments und
API-Versionen verwenden; die Embedding-Felder fallen bei Leereingabe auf die
Werte des AI-Endpoints zurück.

Zwei Endpoint-Schemata werden automatisch erkannt: das klassische
Azure-OpenAI-Format (`https://<ressource>.openai.azure.com`, Deployment im Pfad,
`api-version` erforderlich) und das neue OpenAI-kompatible **v1-Format** von Azure
AI Foundry, erkennbar am Pfad `/openai/v1`
(`https://<ressource>.services.ai.azure.com/openai/v1`). Beim v1-Format wird das
Deployment als `model` im Request übergeben und `api-version` ist optional. Chat-
und Embedding-Endpoint dürfen unterschiedliche Schemata verwenden.

### Endpoint per Umgebungsvariable festlegen (optional)

Die Endpoint-Einstellungen lassen sich alternativ zum UI-Dialog vollständig über
Umgebungsvariablen vorgeben. Ist eine dieser Variablen gesetzt, hat ihr Wert
Vorrang vor `config.json` und das zugehörige Feld im Einstellungsdialog wird nur
angezeigt, aber deaktiviert (nicht über die UI änderbar):

Das Namensschema ist einheitlich: der **generelle AI-Endpoint** nutzt die
Basis-Namen `AZURE_*`, die **Embeddings** durchgängig `AZURE_EMBEDDING_*`.

Genereller AI-Endpoint:

| Variable        | Einstellung                                   |
| --------------- | --------------------------------------------- |
| `AZURE_ENDPOINT` | Endpoint-URL des AI-Endpoints.               |
| `AZURE_DEPLOYMENT` | Deployment-Name des Chat-Modells.          |
| `AZURE_MODELS` | Auswählbare Modelle (Komma- oder Zeilen-getrennt). |
| `AZURE_API_VERSION` | API-Version des AI-Endpoints.             |

Embeddings (fallen bei Leereingabe auf den AI-Endpoint zurück):

| Variable        | Einstellung                                   |
| --------------- | --------------------------------------------- |
| `AZURE_EMBEDDING_ENDPOINT` | Embedding-Endpoint-URL.               |
| `AZURE_EMBEDDING_DEPLOYMENT` | Deployment-Name des Embedding-Modells. |
| `AZURE_EMBEDDING_API_VERSION` | Embedding-API-Version.              |

Die zugehörigen Secrets sind `AZURE_API_KEY` bzw. `AZURE_EMBEDDING_API_KEY`
(siehe Tabelle oben).

Nicht gesetzte Variablen bleiben im UI frei editierbar. Leere Werte gelten als
„nicht gesetzt“ und aktivieren keine Sperre.


### Bereitschaft & Verbindungsprüfung

Nach dem ersten Konfigurieren im UI auf **Speichern** und dann **Verbindung
testen** klicken. Geprüft werden Speicher (Datenpfad schreibbar), Chat-Endpoint
und Embedding-Endpoint. Dokument-Uploads sind erst freigegeben, wenn Speicher
und Embedding-Endpoint grün sind. Jede Konfigurationsänderung setzt die
Verifizierung zurück. Beim Container-Start wird automatisch verifiziert (sofern
konfiguriert); ein Hintergrund-Check (`HEALTHCHECK_INTERVAL`) überwacht die
Verbindung laufend und meldet Ausfälle über den Status in der Seitenleiste sowie
im Log.

### Web-Suche (optional)

Im Einstellungsdialog unter **Web-Suche** einen Anbieter wählen:

- **Tavily** – auf LLM/RAG optimiert, liefert direkt extrahierte Inhalte
  (benötigt `SEARCH_API_KEY`).
- **Brave Search** – REST-API (benötigt `SEARCH_API_KEY`).
- **SearXNG** – selbst gehostete Meta-Suche; nur die Basis-URL angeben, kein Key
  nötig.

Ist ein Anbieter konfiguriert, erscheint im Chat neben dem Eingabefeld ein
🌐-Umschalter. Ist er aktiv, wird die jeweilige Nachricht mit aktuellen
Web-Ergebnissen angereichert; der Zustand bleibt über Chatwechsel hinweg
erhalten. Der Such-API-Key wird – wie die Azure-Keys – ausschließlich über die
Umgebungsvariable `SEARCH_API_KEY` bezogen und nie in `config.json` gespeichert.

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
  -v ai-ui-data:/appdata \
  ai-ui
```

### Datenpfad-Berechtigungen (non-root)

Der Container läuft als non-root-User (**UID/GID `65532`**) und speichert alle
Daten (Chats, Dokumente, Embeddings, Konfiguration) unter `/appdata`. Als
persistenter Speicher dient ein von Docker verwaltetes **Named Volume**:

```yaml
services:
  ai-ui:
    image: ghcr.io/daknoblo/ai-ui:latest
    volumes:
      - ai-ui-data:/appdata
volumes:
  ai-ui-data:
```

Ein frisch angelegtes Named Volume übernimmt die Eigentümerschaft automatisch aus
dem Image (`65532`) und läuft damit out-of-the-box – auch in Dockge/Portainer als
normaler User, ohne jede manuelle Rechtevergabe. Docker verwaltet das Volume; es
sind keine Eingriffe auf dem Host nötig.

## Deployment

Die [docker-compose.example.yml](docker-compose.example.yml) enthält **einen**
`ai-ui`-Container mit Named Volume und veröffentlichtem Port `8080`;
Traefik-Labels sind optional auskommentiert enthalten. Das Projekt ist auf genau
einen Container ausgelegt – wie viele Instanzen davon betrieben werden, bleibt dem
Nutzer überlassen (z.B. mehrere Services in einem Stack). Das Image wird per
GitHub Actions nach `ghcr.io/daknoblo/ai-ui` gebaut und veröffentlicht (Push auf
`main` sowie `v*`-Tags).