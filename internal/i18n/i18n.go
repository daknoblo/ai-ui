// Package i18n provides the message catalog for the user interface.
//
// The active language is a global application setting stored in the
// configuration (see internal/config). It is therefore resolved lazily at
// render time instead of being threaded through every template data struct.
package i18n

import (
	"fmt"
	"strconv"
	"strings"
)

// Supported language codes.
const (
	EN = "en"
	DE = "de"
)

// Default is used when no (or an unknown) language is configured.
const Default = EN

// Option describes a selectable language for the settings dialog.
type Option struct {
	Code string
	Name string
}

// Options returns all selectable languages in display order.
func Options() []Option {
	return []Option{
		{Code: EN, Name: "English"},
		{Code: DE, Name: "Deutsch"},
	}
}

// Normalize maps arbitrary input to a supported language code. Unknown values
// fall back to Default, so a hand-edited config file can never break rendering.
func Normalize(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case DE, "de-de", "german", "deutsch":
		return DE
	case EN, "en-us", "en-gb", "english":
		return EN
	default:
		return Default
	}
}

// T returns the translation for key in the given language. Missing keys fall
// back to English and, as a last resort, to the key itself so that a forgotten
// entry is visible but never panics. If args are supplied the result is
// formatted with fmt.Sprintf.
func T(lang, key string, args ...any) string {
	s, ok := catalog[Normalize(lang)][key]
	if !ok {
		if s, ok = catalog[EN][key]; !ok {
			s = key
		}
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// GroupThousands formats an integer using the thousands separator of the given
// language (1234567 -> "1,234,567" in English, "1.234.567" in German).
func GroupThousands(lang string, n int64) string {
	sep := ","
	if Normalize(lang) == DE {
		sep = "."
	}
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) > 3 {
		var b strings.Builder
		b.Grow(len(s) + len(s)/3)
		lead := len(s) % 3
		if lead > 0 {
			b.WriteString(s[:lead])
		}
		for i := lead; i < len(s); i += 3 {
			if b.Len() > 0 {
				b.WriteString(sep)
			}
			b.WriteString(s[i : i+3])
		}
		s = b.String()
	}
	if neg {
		return "-" + s
	}
	return s
}

// catalog holds all translatable strings. Keys are grouped by area and kept
// stable; only the values differ per language.
var catalog = map[string]map[string]string{
	EN: {
		// ---- generic actions ----
		"action.close":  "Close",
		"action.delete": "Delete",
		"action.remove": "Remove",
		"action.save":   "Save",

		// ---- sidebar ----
		"nav.new_chat":         "+ New chat",
		"nav.menu":             "Menu",
		"nav.settings":         "Settings",
		"nav.stats":            "Statistics",
		"nav.logs":             "Logs",
		"nav.stats_hint":       "Storage, documents and tokens — opens the statistics",
		"chat.delete_confirm":  "Really delete this chat?",
		"chat.none_yet":        "No chats yet.",
		"doc.chip_title":       "%s – %d sections",
		"storage.title":        "Database size including embeddings",
		"doc.count_one":        "document",
		"doc.count_many":       "documents",
		"usage.tokens":         "tokens",
		"usage.tooltip_head":   "Token usage since server start",
		"usage.tooltip_chat":   "Chat: %s (%s input / %s output) across %s requests",
		"usage.tooltip_embed":  "Embeddings: %s across %s requests",
		"usage.tooltip_image":  "Images: %s across %s requests",
		"uploads.allowed_hint": "Uploads enabled",
		"uploads.blocked_hint": "Uploads blocked – test the connection",

		// ---- connection status ----
		"status.not_configured":    "Not configured",
		"status.checking":          "Checking…",
		"status.connected":         "Connected",
		"status.storage_error":     "Storage error",
		"status.endpoints_offline": "Endpoints unreachable",
		"status.chat_offline":      "Chat endpoint offline",
		"status.embedding_offline": "Embedding endpoint offline",

		// ---- chat page ----
		"chat.default_title":          "New chat",
		"chat.model_label":            "Model",
		"chat.model_auto":             "Auto (router)",
		"chat.welcome_title":          "How can I help?",
		"chat.welcome_text":           "Ask a question. Uploaded documents are used as context automatically.",
		"chat.welcome_hint":           "Tip: attach documents via 📎 or drop them into the window. Use 🌐 to include current web results.",
		"chat.not_configured":         "⚠ Please configure the Azure endpoint under Settings first.",
		"chat.attach_title":           "Attach documents",
		"chat.attach_option":          "Attach",
		"chat.image_edit":             "Edit image",
		"chat.image_edit_title":       "Modify the latest image of this chat instead of generating a new one",
		"chat.web_toggle_title":       "Web search for the next message",
		"chat.image_toggle_title":     "Generate an image from the next message",
		"chat.image_tag":              "Image",
		"chat.image_mode":             "Image mode",
		"chat.mode_label":             "Answer type",
		"chat.mode_chat":              "Chat",
		"chat.mode_image":             "Image",
		"chat.web_option":             "Web search",
		"chat.image_placeholder":      "Describe the image…",
		"image.source_title":          "%s — is edited in image mode instead of generating a new image",
		"chat.input_placeholder":      "Write a message…",
		"chat.send_title":             "Send",
		"drop.ready_title":            "Drop documents here",
		"drop.ready_sub":              "Text, Markdown, PDF or DOCX",
		"drop.blocked_title":          "Uploads blocked",
		"drop.blocked_sub":            "Please test the connection in the settings first",
		"role.user":                   "You",
		"role.assistant":              "Assistant",
		"tool.web_search_status":      "🔎 Web search: %s",
		"model.badge_title":           "Model used for this answer",
		"usage.footer":                "%s tokens · %s input / %s output",
		"stream.not_configured":       "Not configured. Please set the Azure endpoint, deployment, API version and AZURE_API_KEY.",
		"stream.no_message":           "No message found to answer.",
		"stream.request_failed":       "Request failed: %s",
		"stream.image_failed":         "Image generation failed: %s",
		"stream.image_not_configured": "Image generation is not configured. Set the image deployment in the settings and provide AZURE_IMAGE_API_KEY (or AZURE_API_KEY).",
		"stream.interrupted":          "_(connection interrupted)_",
		"stream.not_supported":        "streaming not supported",
		"error.internal":              "internal error",
		"error.invalid_model":         "invalid model",
		"error.invalid_endpoint":      "Invalid SearXNG endpoint: %s",
		"chat.web_tag":                "Web",

		// ---- settings dialog ----
		"config.title":                 "Settings",
		"config.section_general":       "General",
		"config.language":              "Language",
		"config.language_hint":         "Applies to the whole interface; the page reloads after saving.",
		"config.section_chat":          "Chat (model router)",
		"config.locks_notice":          "Some endpoint fields are set via environment variables and are therefore disabled here. Their values are shown but can only be changed through the environment.",
		"config.set_via":               "(set via %s)",
		"config.endpoint":              "Azure endpoint URL",
		"config.api_version":           "API version",
		"config.chat_deployment":       "Deployment name (chat model)",
		"config.models":                "Available models",
		"config.models_hint":           "(set via AZURE_MODELS; the first entry is the default for new chats)",
		"config.models_empty":          "No models configured - set AZURE_MODELS to fill the picker in the chat header.",
		"config.section_embeddings":    "Embeddings (document context)",
		"config.embedding_endpoint":    "Embedding endpoint URL",
		"config.optional_same_as_chat": "(optional, defaults to chat)",
		"config.embedding_api_version": "Embedding API version",
		"config.embedding_deployment":  "Embedding deployment name",
		"config.section_images":        "Image generation (optional)",
		"config.image_endpoint":        "Image endpoint URL",
		"config.image_deployment":      "Image deployment name",
		"config.image_optional":        "(empty disables image mode)",
		"config.image_api_version":     "Image API version",
		"config.image_version_hint":    "(optional, v1 endpoints default to preview)",
		"config.image_size":            "Size",
		"config.image_quality":         "Quality",
		"config.image_format":          "File format",
		"config.image_key":             "Image API key (secret):",
		"config.image_key_own":         "set via environment variable AZURE_IMAGE_API_KEY ✓",
		"config.image_key_inherit":     "inherited from AZURE_API_KEY ✓",
		"config.image_key_missing":     "not set — set AZURE_IMAGE_API_KEY or AZURE_API_KEY",
		"config.image_params_hint":     "Size, quality and file format are selected in the chat as soon as image mode is switched on.",
		"config.section_search":        "Web search (optional)",
		"config.search_provider":       "Provider",
		"config.search_off":            "Off",
		"config.search_searxng":        "SearXNG (self-hosted)",
		"config.search_max_results":    "Number of results",
		"config.searxng_endpoint":      "SearXNG endpoint",
		"config.searxng_only":          "(SearXNG only)",
		"config.search_auto":           "Automatic (tool calling) — the model decides on its own when to search the web",
		"config.search_auto_hint":      "(requires tool-calling support from the router)",
		"config.search_key":            "Search API key (secret):",
		"config.search_key_set":        "set via environment variable SEARCH_API_KEY ✓",
		"config.search_key_missing":    "not set — set SEARCH_API_KEY for Tavily/Brave (SearXNG needs no key)",
		"config.section_behavior":      "Behavior",
		"config.system_prompt":         "System prompt",
		"config.temperature":           "Temperature",
		"config.log_level":             "Log level",
		"config.log_level_hint":        "(applies immediately, see the logs page)",
		"config.chat_key":              "Chat API key (secret):",
		"config.chat_key_set":          "set via environment variable AZURE_API_KEY ✓",
		"config.chat_key_missing":      "not set — provide AZURE_API_KEY when starting the container",
		"config.embedding_key":         "Embedding API key (secret):",
		"config.embedding_key_own":     "dedicated key set via AZURE_EMBEDDING_API_KEY ✓",
		"config.embedding_key_inherit": "uses AZURE_API_KEY (no dedicated key set) ✓",
		"config.embedding_key_missing": "not set — set AZURE_EMBEDDING_API_KEY for a separate resource",
		"config.saved":                 "Saved ✓",
		"config.section_readiness":     "Readiness & connection",
		"config.readiness_hint":        "Document uploads are only possible once storage and the embedding endpoint have been verified. Save first, then test.",
		"config.uploads_allowed":       "Uploads enabled ✓",
		"config.uploads_blocked":       "Uploads blocked – not verified yet",
		"config.verify":                "Test connection",
		"config.verify_running":        "Checking storage and endpoints…",
		"config.verified_last":         "Last verified ✓",
		"verify.uploads_ready":         "Storage & embedding ready – uploads enabled ✓",
		"verify.uploads_blocked":       "Uploads stay blocked until storage and embedding are green",

		// ---- readiness checks ----
		"check.storage":            "Storage (data path)",
		"check.storage_ready":      "ready",
		"check.chat_endpoint":      "Chat endpoint",
		"check.embedding_endpoint": "Embedding endpoint",
		"check.web_search":         "Web search",
		"check.reachable":          "reachable",
		"check.provider_reachable": "%s reachable",

		// ---- uploads ----
		"upload.blocked":           "Uploads blocked – please test the connection in the settings first (storage and embedding endpoint must be ready).",
		"upload.embedding_missing": "Embeddings are not configured – please complete the settings first.",
		"upload.no_file":           "No file received.",
		"upload.too_large":         "%q (too large)",
		"upload.read_error":        "%q (read error)",
		"upload.item_failed":       "%q (%s)",
		"upload.added_one":         "1 document added.",
		"upload.added_many":        "%d documents added.",
		"upload.partial":           "%d added. Failed: %s",
		"upload.failed":            "Processing failed: %s",

		// ---- statistics page ----
		"stats.title":            "Statistics",
		"stats.heading":          "Token statistics",
		"stats.back":             "← Back",
		"stats.total_tokens":     "Total tokens",
		"stats.chat_tokens":      "Chat tokens",
		"stats.chat_sub":         "%s input / %s output",
		"stats.embedding_tokens": "Embedding tokens",
		"stats.image_tokens":     "Image tokens",
		"stats.image_sub":        "%s requests",
		"stats.storage":          "Storage used",
		"stats.documents":        "Documents",
		"stats.chart":            "Tokens per day",

		// ---- log page ----
		"logs.title":             "Logs",
		"logs.heading":           "Log output",
		"logs.level":             "Level: %s",
		"logs.copy":              "Copy",
		"logs.copied":            "Copied ✓",
		"logs.clear":             "Clear",
		"logs.follow":            "Follow",
		"logs.empty":             "No log entries yet.",
		"logs.hint":              "The last 2000 lines since the start are kept in memory. Switch the level to debug in the settings for more detail.",
		"stats.chat_requests":    "Chat requests",
		"stats.history":          "History (last 30 days)",
		"stats.day":              "Day",
		"stats.chat":             "Chat",
		"stats.embedding":        "Embedding",
		"stats.total":            "Total",
		"stats.requests":         "Requests",
		"stats.no_usage":         "No usage recorded yet.",
		"stats.by_model":         "By model",
		"stats.model":            "Model",
		"stats.no_chat_requests": "No chat requests recorded yet.",
		"stats.note":             "Statistics are stored persistently in the data path and survive restarts.",
		"stats.auto_router":      "(auto/router)",

		// ---- browser-side strings (exposed as window.AIUI_I18N) ----
		"js.upload_start": "Uploading documents…",
		"js.processing":   "Processing {n} of {total}: {name}",
		"js.processing_n": "Processing {n} of {total}…",
		"js.summary":      "{done} of {total} processed",
		"js.failed":       "({failed} failed)",

		// ---- prompts sent to the model ----
		"prompt.default_system": "You are a helpful assistant. Answer precisely and use the provided context when it is relevant.",
		"prompt.title_system":   "You create extremely short, concise titles for chat conversations.",
		"prompt.title_user":     "Create a very short, concise title (at most 6 words, no quotation marks, no trailing punctuation) for this conversation.\n\nQuestion: %s\n\nAnswer: %s",
		"prompt.rag_intro":      "\n\nUse the following context from uploaded documents if it is relevant to the question. Ignore it if it does not fit.\n\n",
		"prompt.rag_item":       "[Context %d]\n%s\n\n",
		"prompt.web_intro":      "\n\nUse the following current web results if they are relevant to the question. Cite the sources with their URL.\n\n",
		"prompt.web_item":       "[Web %d] %s\nSource: %s\n%s\n\n",
		"prompt.tool_desc":      "Searches the web for current information. Use this tool when the question concerns current events, news, prices, figures or facts that may have changed since your training cut-off.",
		"prompt.tool_query":     "The search query in natural language",
		"tool.unknown":          "Unknown tool: %s",
		"tool.empty_query":      "Empty search query.",
		"tool.search_failed":    "Web search failed: %s",
		"tool.no_results":       "No web results found.",
		"tool.results_intro":    "Web results (cite relevant sources with their URL):\n\n",
		"tool.result_item":      "[%d] %s\nURL: %s\n%s\n\n",
	},

	DE: {
		// ---- generic actions ----
		"action.close":  "Schließen",
		"action.delete": "Löschen",
		"action.remove": "Entfernen",
		"action.save":   "Speichern",

		// ---- sidebar ----
		"nav.new_chat":         "+ Neuer Chat",
		"nav.menu":             "Menü",
		"nav.settings":         "Einstellungen",
		"nav.stats":            "Statistik",
		"nav.logs":             "Logs",
		"nav.stats_hint":       "Speicher, Dokumente und Tokens — öffnet die Statistik",
		"chat.delete_confirm":  "Diesen Chat wirklich löschen?",
		"chat.none_yet":        "Noch keine Chats.",
		"doc.chip_title":       "%s – %d Abschnitte",
		"storage.title":        "Datenbankgröße inklusive Embeddings",
		"doc.count_one":        "Dokument",
		"doc.count_many":       "Dokumente",
		"usage.tokens":         "Tokens",
		"usage.tooltip_head":   "Token-Verbrauch seit Serverstart",
		"usage.tooltip_chat":   "Chat: %s (%s Eingabe / %s Antwort) über %s Anfragen",
		"usage.tooltip_embed":  "Embeddings: %s über %s Anfragen",
		"usage.tooltip_image":  "Bilder: %s über %s Anfragen",
		"uploads.allowed_hint": "Uploads freigegeben",
		"uploads.blocked_hint": "Uploads gesperrt – Verbindung prüfen",

		// ---- connection status ----
		"status.not_configured":    "Nicht konfiguriert",
		"status.checking":          "Prüfe…",
		"status.connected":         "Verbunden",
		"status.storage_error":     "Speicher-Fehler",
		"status.endpoints_offline": "Endpoints nicht erreichbar",
		"status.chat_offline":      "Chat-Endpoint offline",
		"status.embedding_offline": "Embedding-Endpoint offline",

		// ---- chat page ----
		"chat.default_title":          "Neuer Chat",
		"chat.model_label":            "Modell",
		"chat.model_auto":             "Auto (Router)",
		"chat.welcome_title":          "Womit kann ich helfen?",
		"chat.welcome_text":           "Stelle eine Frage. Hochgeladene Dokumente werden automatisch als Kontext genutzt.",
		"chat.welcome_hint":           "Tipp: Hänge Dokumente über 📎 an oder ziehe sie ins Fenster. Mit 🌐 beziehst du aktuelle Web-Ergebnisse ein.",
		"chat.not_configured":         "⚠ Bitte zuerst unter Einstellungen den Azure-Endpoint konfigurieren.",
		"chat.attach_title":           "Dokumente anhängen",
		"chat.attach_option":          "Anhang",
		"chat.image_edit":             "Bild bearbeiten",
		"chat.image_edit_title":       "Das letzte Bild dieses Chats ändern statt ein neues zu erzeugen",
		"chat.web_toggle_title":       "Websuche für die nächste Nachricht",
		"chat.image_toggle_title":     "Aus der nächsten Nachricht ein Bild erzeugen",
		"chat.image_tag":              "Bild",
		"chat.image_mode":             "Bildmodus",
		"chat.mode_label":             "Antwortart",
		"chat.mode_chat":              "Chat",
		"chat.mode_image":             "Bild",
		"chat.web_option":             "Websuche",
		"chat.image_placeholder":      "Bild beschreiben…",
		"image.source_title":          "%s — wird im Bildmodus bearbeitet statt ein neues Bild zu erzeugen",
		"chat.input_placeholder":      "Nachricht schreiben…",
		"chat.send_title":             "Senden",
		"drop.ready_title":            "Dokumente hier ablegen",
		"drop.ready_sub":              "Text, Markdown, PDF oder DOCX",
		"drop.blocked_title":          "Uploads gesperrt",
		"drop.blocked_sub":            "Bitte zuerst in den Einstellungen die Verbindung testen",
		"role.user":                   "Du",
		"role.assistant":              "Assistent",
		"tool.web_search_status":      "🔎 Websuche: %s",
		"model.badge_title":           "Für diese Antwort verwendetes Modell",
		"usage.footer":                "%s Tokens · %s Eingabe / %s Antwort",
		"stream.not_configured":       "Nicht konfiguriert. Bitte Azure-Endpoint, Deployment, API-Version und AZURE_API_KEY setzen.",
		"stream.no_message":           "Keine Nachricht zum Beantworten gefunden.",
		"stream.request_failed":       "Fehler bei der Anfrage: %s",
		"stream.image_failed":         "Bildgenerierung fehlgeschlagen: %s",
		"stream.image_not_configured": "Bildgenerierung ist nicht konfiguriert. Bitte das Bild-Deployment in den Einstellungen setzen und AZURE_IMAGE_API_KEY (oder AZURE_API_KEY) hinterlegen.",
		"stream.interrupted":          "_(Verbindung unterbrochen)_",
		"stream.not_supported":        "Streaming wird nicht unterstützt",
		"error.internal":              "Interner Fehler",
		"error.invalid_model":         "Ungültiges Modell",
		"error.invalid_endpoint":      "Ungültiger SearXNG-Endpoint: %s",
		"chat.web_tag":                "Web",

		// ---- settings dialog ----
		"config.title":                 "Einstellungen",
		"config.section_general":       "Allgemein",
		"config.language":              "Sprache",
		"config.language_hint":         "Gilt für die gesamte Oberfläche; die Seite wird nach dem Speichern neu geladen.",
		"config.section_chat":          "Chat (Model-Router)",
		"config.locks_notice":          "Einige Endpoint-Felder sind über Umgebungsvariablen festgelegt und daher hier deaktiviert. Ihre Werte werden angezeigt, können aber nur über die Umgebung geändert werden.",
		"config.set_via":               "(per %s gesetzt)",
		"config.endpoint":              "Azure Endpoint-URL",
		"config.api_version":           "API-Version",
		"config.chat_deployment":       "Deployment-Name (Chat-Modell)",
		"config.models":                "Verfügbare Modelle",
		"config.models_hint":           "(über AZURE_MODELS gesetzt; der erste Eintrag ist die Vorauswahl für neue Chats)",
		"config.models_empty":          "Keine Modelle konfiguriert - AZURE_MODELS setzen, um die Auswahl im Chat-Header zu füllen.",
		"config.section_embeddings":    "Embeddings (Dokument-Kontext)",
		"config.embedding_endpoint":    "Embedding-Endpoint-URL",
		"config.optional_same_as_chat": "(optional, sonst wie Chat)",
		"config.embedding_api_version": "Embedding-API-Version",
		"config.embedding_deployment":  "Embedding-Deployment-Name",
		"config.section_images":        "Bildgenerierung (optional)",
		"config.image_endpoint":        "Bild-Endpoint-URL",
		"config.image_deployment":      "Bild-Deployment-Name",
		"config.image_optional":        "(leer = Bildmodus deaktiviert)",
		"config.image_api_version":     "Bild-API-Version",
		"config.image_version_hint":    "(optional, v1-Endpoints nutzen preview)",
		"config.image_size":            "Größe",
		"config.image_quality":         "Qualität",
		"config.image_format":          "Dateiformat",
		"config.image_key":             "Bild-API-Key (Secret):",
		"config.image_key_own":         "über Umgebungsvariable AZURE_IMAGE_API_KEY gesetzt ✓",
		"config.image_key_inherit":     "von AZURE_API_KEY übernommen ✓",
		"config.image_key_missing":     "nicht gesetzt — AZURE_IMAGE_API_KEY oder AZURE_API_KEY setzen",
		"config.image_params_hint":     "Größe, Qualität und Dateiformat werden im Chat gewählt, sobald der Bildmodus aktiv ist.",
		"config.section_search":        "Web-Suche (optional)",
		"config.search_provider":       "Anbieter",
		"config.search_off":            "Aus",
		"config.search_searxng":        "SearXNG (selbst gehostet)",
		"config.search_max_results":    "Anzahl Treffer",
		"config.searxng_endpoint":      "SearXNG-Endpoint",
		"config.searxng_only":          "(nur für SearXNG)",
		"config.search_auto":           "Automatisch (Tool-Calling) — das Modell entscheidet selbst, wann es das Web durchsucht",
		"config.search_auto_hint":      "(erfordert Tool-Calling-Unterstützung des Routers)",
		"config.search_key":            "Such-API-Key (Secret):",
		"config.search_key_set":        "über Umgebungsvariable SEARCH_API_KEY gesetzt ✓",
		"config.search_key_missing":    "nicht gesetzt — für Tavily/Brave die Umgebungsvariable SEARCH_API_KEY setzen (SearXNG benötigt keinen Key)",
		"config.section_behavior":      "Verhalten",
		"config.system_prompt":         "System-Prompt",
		"config.temperature":           "Temperatur",
		"config.log_level":             "Log-Level",
		"config.log_level_hint":        "(gilt sofort, siehe Log-Seite)",
		"config.chat_key":              "Chat-API-Key (Secret):",
		"config.chat_key_set":          "über Umgebungsvariable AZURE_API_KEY gesetzt ✓",
		"config.chat_key_missing":      "nicht gesetzt — Umgebungsvariable AZURE_API_KEY beim Containerstart angeben",
		"config.embedding_key":         "Embedding-API-Key (Secret):",
		"config.embedding_key_own":     "eigener Key über AZURE_EMBEDDING_API_KEY gesetzt ✓",
		"config.embedding_key_inherit": "nutzt AZURE_API_KEY (kein eigener Key gesetzt) ✓",
		"config.embedding_key_missing": "nicht gesetzt — bei getrennter Ressource AZURE_EMBEDDING_API_KEY setzen",
		"config.saved":                 "Gespeichert ✓",
		"config.section_readiness":     "Bereitschaft & Verbindung",
		"config.readiness_hint":        "Dokument-Uploads sind erst möglich, wenn Speicher und Embedding-Endpoint verifiziert sind. Bitte zuerst speichern, dann testen.",
		"config.uploads_allowed":       "Uploads freigegeben ✓",
		"config.uploads_blocked":       "Uploads gesperrt – noch nicht verifiziert",
		"config.verify":                "Verbindung testen",
		"config.verify_running":        "Prüfe Speicher und Endpoints…",
		"config.verified_last":         "Zuletzt verifiziert ✓",
		"verify.uploads_ready":         "Speicher & Embedding bereit – Uploads freigegeben ✓",
		"verify.uploads_blocked":       "Uploads bleiben gesperrt, bis Speicher und Embedding grün sind",

		// ---- readiness checks ----
		"check.storage":            "Speicher (Datenpfad)",
		"check.storage_ready":      "bereit",
		"check.chat_endpoint":      "Chat-Endpoint",
		"check.embedding_endpoint": "Embedding-Endpoint",
		"check.web_search":         "Web-Suche",
		"check.reachable":          "erreichbar",
		"check.provider_reachable": "%s erreichbar",

		// ---- uploads ----
		"upload.blocked":           "Upload gesperrt – bitte zuerst in den Einstellungen die Verbindung testen (Speicher & Embedding-Endpoint müssen bereit sein).",
		"upload.embedding_missing": "Embedding nicht konfiguriert – bitte zuerst Einstellungen ausfüllen.",
		"upload.no_file":           "Keine Datei empfangen.",
		"upload.too_large":         "%q (zu groß)",
		"upload.read_error":        "%q (Lesefehler)",
		"upload.item_failed":       "%q (%s)",
		"upload.added_one":         "1 Dokument hinzugefügt.",
		"upload.added_many":        "%d Dokumente hinzugefügt.",
		"upload.partial":           "%d hinzugefügt. Fehlgeschlagen: %s",
		"upload.failed":            "Verarbeitung fehlgeschlagen: %s",

		// ---- statistics page ----
		"stats.title":            "Statistik",
		"stats.heading":          "Token-Statistik",
		"stats.back":             "← Zurück",
		"stats.total_tokens":     "Tokens gesamt",
		"stats.chat_tokens":      "Chat-Tokens",
		"stats.chat_sub":         "%s Eingabe / %s Antwort",
		"stats.embedding_tokens": "Embedding-Tokens",
		"stats.image_tokens":     "Bild-Tokens",
		"stats.image_sub":        "%s Anfragen",
		"stats.storage":          "Belegter Speicher",
		"stats.documents":        "Dokumente",
		"stats.chart":            "Tokens pro Tag",

		// ---- log page ----
		"logs.title":             "Logs",
		"logs.heading":           "Log-Ausgabe",
		"logs.level":             "Level: %s",
		"logs.copy":              "Kopieren",
		"logs.copied":            "Kopiert ✓",
		"logs.clear":             "Leeren",
		"logs.follow":            "Mitlaufen",
		"logs.empty":             "Noch keine Log-Einträge.",
		"logs.hint":              "Die letzten 2000 Zeilen seit dem Start werden im Speicher gehalten. Für mehr Details in den Einstellungen auf debug umstellen.",
		"stats.chat_requests":    "Chat-Anfragen",
		"stats.history":          "Verlauf (letzte 30 Tage)",
		"stats.day":              "Tag",
		"stats.chat":             "Chat",
		"stats.embedding":        "Embedding",
		"stats.total":            "Gesamt",
		"stats.requests":         "Anfragen",
		"stats.no_usage":         "Noch keine Nutzung erfasst.",
		"stats.by_model":         "Nach Modell",
		"stats.model":            "Modell",
		"stats.no_chat_requests": "Noch keine Chat-Anfragen erfasst.",
		"stats.note":             "Die Statistik wird dauerhaft im Datenpfad gespeichert und überlebt Neustarts.",
		"stats.auto_router":      "(Auto/Router)",

		// ---- browser-side strings (exposed as window.AIUI_I18N) ----
		"js.upload_start": "Lade Dokumente hoch…",
		"js.processing":   "Verarbeite {n} von {total}: {name}",
		"js.processing_n": "Verarbeite {n} von {total}…",
		"js.summary":      "{done} von {total} verarbeitet",
		"js.failed":       "({failed} fehlgeschlagen)",

		// ---- prompts sent to the model ----
		"prompt.default_system": "Du bist ein hilfreicher Assistent. Antworte präzise und nutze den bereitgestellten Kontext, wenn er relevant ist.",
		"prompt.title_system":   "Du erstellst extrem kurze, prägnante Titel für Chat-Unterhaltungen.",
		"prompt.title_user":     "Erstelle einen sehr kurzen, prägnanten Titel (höchstens 6 Wörter, keine Anführungszeichen, kein abschließendes Satzzeichen) für diese Unterhaltung.\n\nFrage: %s\n\nAntwort: %s",
		"prompt.rag_intro":      "\n\nNutze den folgenden Kontext aus hochgeladenen Dokumenten, sofern er für die Frage relevant ist. Wenn er nicht passt, ignoriere ihn.\n\n",
		"prompt.rag_item":       "[Kontext %d]\n%s\n\n",
		"prompt.web_intro":      "\n\nNutze die folgenden aktuellen Web-Ergebnisse, sofern sie für die Frage relevant sind. Zitiere die Quellen mit ihrer URL.\n\n",
		"prompt.web_item":       "[Web %d] %s\nQuelle: %s\n%s\n\n",
		"prompt.tool_desc":      "Durchsucht das Web nach aktuellen Informationen. Nutze dieses Werkzeug, wenn die Frage aktuelle Ereignisse, Nachrichten, Preise, Zahlen oder Fakten betrifft, die sich seit deinem Trainingsstand geändert haben könnten.",
		"prompt.tool_query":     "Die Suchanfrage in natürlicher Sprache",
		"tool.unknown":          "Unbekanntes Werkzeug: %s",
		"tool.empty_query":      "Leere Suchanfrage.",
		"tool.search_failed":    "Websuche fehlgeschlagen: %s",
		"tool.no_results":       "Keine Web-Ergebnisse gefunden.",
		"tool.results_intro":    "Web-Ergebnisse (zitiere relevante Quellen mit ihrer URL):\n\n",
		"tool.result_item":      "[%d] %s\nURL: %s\n%s\n\n",
	},
}
