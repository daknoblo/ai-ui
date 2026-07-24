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
| `AZURE_API_KEY` | –        | **Secret.** API-Key des Model-Routers (Chat). |
| `AZURE_EMBEDDING_API_KEY` | – | **Secret, optional.** Eigener Key, falls Embeddings auf einer separaten Azure-Ressource liegen. Leer ⇒ `AZURE_API_KEY` wird genutzt. |
| `SEARCH_API_KEY` | – | **Secret, optional.** API-Key für die Web-Suche (Tavily oder Brave). Für SearXNG nicht erforderlich. |
| `DATA_DIR`      | `/appdata`  | Persistenter Datenpfad (DB + `appdata/`).     |
| `PORT`          | `8080`   | HTTP-Port.                                    |
| `HEALTHCHECK_INTERVAL` | `60s` | Intervall der periodischen Verbindungsprüfung (Go-Dauer, z.B. `30s`, `2m`). `0` oder `off` deaktiviert den periodischen Check (die Prüfung beim Start läuft weiterhin). |

Die übrigen Einstellungen werden im UI-Dialog gesetzt und unter
`<DATA_DIR>/appdata/config.json` gespeichert (ohne Secret). Chat und Embeddings
können getrennte Endpoints, Deployments und API-Versionen verwenden; die
Embedding-Felder fallen bei Leereingabe auf die Chat-Werte zurück.

### Endpoint per Umgebungsvariable festlegen (optional)

Die Endpoint-Einstellungen lassen sich alternativ zum UI-Dialog vollständig über
Umgebungsvariablen vorgeben. Ist eine dieser Variablen gesetzt, hat ihr Wert
Vorrang vor `config.json` und das zugehörige Feld im Einstellungsdialog wird nur
angezeigt, aber deaktiviert (nicht über die UI änderbar):

| Variable        | Einstellung                                   |
| --------------- | --------------------------------------------- |
| `AZURE_ENDPOINT` | Azure Endpoint-URL (Chat).                   |
| `AZURE_CHAT_DEPLOYMENT` | Deployment-Name des Chat-Modells.     |
| `AZURE_CHAT_MODELS` | Auswählbare Modelle (Komma- oder Zeilen-getrennt). |
| `AZURE_API_VERSION` | API-Version (Chat).                       |
| `AZURE_EMBEDDING_ENDPOINT` | Embedding-Endpoint-URL (sonst wie Chat). |
| `AZURE_EMBEDDING_DEPLOYMENT` | Deployment-Name des Embedding-Modells. |
| `AZURE_EMBEDDING_API_VERSION` | Embedding-API-Version (sonst wie Chat). |

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

Der Container läuft bewusst als non-root-User mit **UID/GID `65532`**. Der
Datenpfad `/appdata` muss für diesen User beschreibbar sein, sonst bricht der
Start mit `mkdir /appdata/appdata: permission denied` ab.

**Einfachste Variante: Named Volume** (funktioniert ohne jede manuelle
Rechtevergabe). Ein frisch angelegtes Named Volume übernimmt die Eigentümerschaft
automatisch aus dem Image (`65532`) und läuft out-of-the-box – auch in
Dockge/Portainer als normaler User:

```yaml
services:
  ai-ui:
    image: ghcr.io/daknoblo/ai-ui:latest
    volumes:
      - ai-ui-data:/appdata   # Named Volume, kein Bind-Mount
volumes:
  ai-ui-data:
```

Hintergrund: Bei einem **Bind-Mount** (`- ./daten:/appdata`) legt der
Docker-Daemon ein fehlendes Host-Verzeichnis als **root** an; der non-root-User
im Container kann dort nicht schreiben. Bei einem Named Volume seedet Docker die
Rechte aus dem Image – deshalb tritt der Fehler dort nie auf.

**Bind-Mount (z.B. direkter Host-Zugriff auf die Daten):** die Rechte per
einmaligem Init-Container setzen – ganz ohne manuelles `chown` auf dem Host. Genau
diesen Aufbau (für zwei Instanzen) zeigt die
[docker-compose.example.yml](docker-compose.example.yml):

```yaml
services:
  ai-ui-init:                       # läuft als root, setzt einmalig die Rechte
    image: busybox:1.37
    command: chown -R 65532:65532 /appdata
    user: "0:0"
    volumes:
      - ./daten:/appdata
  ai-ui:
    image: ghcr.io/daknoblo/ai-ui:latest
    depends_on:
      ai-ui-init:
        condition: service_completed_successfully
    volumes:
      - ./daten:/appdata
```

Ownership eines Named Volumes prüfen: `docker run --rm -v <name>:/d busybox ls -lna /d`
(sollte `65532` zeigen). Ein noch aus einem älteren, root-owned Image stammendes
Volume einmalig neu anlegen (`docker volume rm <name>`, dann neu starten).

## Deployment

Die [docker-compose.example.yml](docker-compose.example.yml) enthält einen
Stack mit zwei generischen Instanzen (`ai-ui-1` + `ai-ui-2`) in einem Dockge-Stack:
Bind-Mounts, je ein Init-Container für die Rechte und veröffentlichte Ports
(`8080`/`8081`); Traefik-Labels sind optional auskommentiert enthalten. Das Image
wird per GitHub Actions nach `ghcr.io/daknoblo/ai-ui` gebaut und veröffentlicht
(Push auf `main` sowie `v*`-Tags).