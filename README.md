# ai-ui

[![CI](https://github.com/daknoblo/ai-ui/actions/workflows/ci.yml/badge.svg)](https://github.com/daknoblo/ai-ui/actions/workflows/ci.yml)
[![Docs](https://github.com/daknoblo/ai-ui/actions/workflows/docs.yml/badge.svg)](https://daknoblo.github.io/ai-ui/)
[![Release](https://img.shields.io/github/v/release/daknoblo/ai-ui)](https://github.com/daknoblo/ai-ui/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/daknoblo/ai-ui)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![GHCR](https://img.shields.io/badge/ghcr.io-ai--ui-blue?logo=docker)](https://github.com/daknoblo/ai-ui/pkgs/container/ai-ui)

A small, self-hosted ChatGPT-like web interface written in Go with document
context (RAG), connected to Azure OpenAI-compatible deployments, with an
optional identity-backed Microsoft Foundry deployment inventory.

**Website with the full screenshot gallery:**
<https://daknoblo.github.io/ai-ui/>

## Screenshots

[![Chat with Markdown answers](docs/screenshots/en/chat.png)](https://daknoblo.github.io/ai-ui/#screenshots)

| Documents as context (RAG) | Image generation | Token statistics |
| -------------------------- | ---------------- | ---------------- |
| ![Documents as chat context](docs/screenshots/en/documents.png) | ![Image generation](docs/screenshots/en/image.png) | ![Token statistics](docs/screenshots/en/stats.png) |

All screenshots are generated automatically from the demo instance
([cmd/demo](cmd/demo)) - see [Demo & documentation](#demo--documentation).

## Features

- Chat interface with a sidebar, multiple conversations and history
- Answer streaming (token by token) via server-sent events
- Model picker in the top right of the chat window. In manual mode its list
  comes from `AZURE_MODELS`; in Foundry mode it comes from supported deployments
  in the resource inventory. Manual mode offers "Auto" for the configured chat
  default; Foundry mode selects deployments explicitly, including a model
  router when available. The selection survives switching chats
- Optional Foundry inventory with metadata-only **Refresh** and separate defaults
  for chat, embeddings, images and vision. Deployment aliases are mapped to
  canonical model metadata; unsupported deployments remain visible
- Reasoning effort selectable per chat next to the input field; the offered
  values follow the selected model
- Document upload as RAG context (embeddings + brute-force cosine search).
  **PDF** (layout aware: lines and columns survive), **Word** (`.docx`, including
  tables, headers, footers and footnotes - tracked deletions and field codes are
  left out), **Excel** (`.xlsx`, sheet by sheet as tab separated rows),
  **PowerPoint** (`.pptx`, slides plus speaker notes), **RTF**, HTML/XML,
  Markdown, CSV, JSON, YAML and source code. Anything else that is textual is
  recognized by its content, so a file without a useful name or type still works
- **Scanned PDFs are read automatically**: a PDF without a text layer has its
  pages transcribed by the vision model, one request per page, and the transcript
  is embedded like any other document. The upload notice says which files were
  read that way and how many pages, because a transcription can be imperfect in a
  way a parsed document is not
- Attach files next to the input field (📎) or drag and drop them into the
  chat window; attachments are shown as chips above the input. Documents reach
  the model as retrieved context sections, attached images are sent along with
  the message so a vision capable model can look at them. The names of all
  attachments are part of the prompt, so the model knows what it has
- **The model follows the attachment**: a picture attached while a text-only
  model is selected is answered by the first vision capable entry of
  `AZURE_MODELS` in manual mode, or the configured vision fallback in Foundry
  mode. The switch applies to that one answer, the picker stays where it is,
  and the model tag names whoever replied
- Optional web search (🌐) per request: pulls in current online results as
  context - provider agnostic (Tavily, Brave Search, SearXNG)
- Optional image generation (🖼): the toggle switches the next message from a
  chat answer to a generated image (Azure image models such as `gpt-image-2`);
  images are stored in the database and shown inline. In image mode an attached
  image turns the next prompt into an edit of that image. There the model picker
  offers the image deployments
- With an image model configured, explicit image requests in ordinary chat can
  call the image generator automatically. Follow-up edits use the latest image;
  ordinary answers stay with the chat model. These image calls incur usage
  charges; manual image mode remains available.
- Documents and images are bound to their chat and are removed together with it
  (including their embeddings)
- Settings dialog in the UI (language, deployment defaults, system prompt,
  temperature, default reasoning effort). Manual mode also exposes endpoints
  and API versions; Foundry mode shows the resource and discovered endpoint
  read-only
- Explicit, staged document reindexing when changing the embedding configuration
  in either mode, with progress and a cost warning; a failed rebuild leaves the
  previous index intact
- User interface available in **English and German**, switchable in the settings
- Readiness/connection check: uploads are only possible once storage and the
  embedding endpoint are verified; checked at start-up and periodically in the
  background, with a status indicator in the sidebar
- Credentials exclusively through environment variables: API keys in manual
  mode, or a tenant/client/client-secret identity in Foundry mode
- Persistence in SQLite under the mounted data path
- Single binary, single Docker image (distroless, non-root), designed to run
  behind a reverse proxy such as Traefik

## Architecture

- **Go 1.26** + `chi` router, `html/template` + **HTMX** (server rendered)
- **SQLite** (`modernc.org/sqlite`, CGO free) for chats, messages, documents
  and embeddings
- **goldmark** for Markdown rendering (raw HTML is escaped, never rendered)
- RAG: chunking → embeddings → cosine similarity (top-k)

## Configuration

| Variable        | Default  | Description                                   |
| --------------- | -------- | --------------------------------------------- |
| `AZURE_RESOURCE_ID` | – | Optional. Opts into Foundry inventory for the primary chat/vision/embedding account; see [Foundry inventory & identity](#foundry-inventory--identity-optional). |
| `AZURE_IMAGE_RESOURCE_ID` | primary resource | Optional in Foundry mode. Separate account for image generation/editing, using the same service principal. Requires `AZURE_RESOURCE_ID` and access to both accounts; see [Separate image resource](#separate-image-resource). |
| `AZURE_TENANT_ID` | – | Tenant ID of the service principal; required in Foundry mode. |
| `AZURE_CLIENT_ID` | – | Application/client ID of the service principal; required in Foundry mode. |
| `AZURE_CLIENT_SECRET` | – | **Secret.** Service-principal secret; required in Foundry mode, environment only. |
| `AZURE_API_KEY` | – | **Secret.** AI endpoint key in manual mode; not required or used for Foundry identity authentication. |
| `AZURE_EMBEDDING_API_KEY` | – | **Secret, manual mode.** Required for an embedding endpoint with a different scheme or host. Empty ⇒ `AZURE_API_KEY` is used only for the same origin. |
| `AZURE_IMAGE_API_KEY` | – | **Secret, optional, manual mode.** Dedicated image endpoint key. Empty ⇒ `AZURE_API_KEY` is used. |
| `SEARCH_API_KEY` | – | **Secret, optional.** API key for web search (Tavily or Brave). Not required for SearXNG. |
| `DATA_DIR`      | `/appdata` | Persistent data path. The SQLite database is stored directly in it, the UI settings in `<DATA_DIR>/appdata/config.json`. |
| `PORT`          | `8080`   | HTTP port.                                    |
| `HEALTHCHECK_INTERVAL` | `60s` | Interval of the periodic connection check (Go duration, e.g. `30s`, `2m`). `0` or `off` disables the periodic check (the start-up check still runs). |
| `TZ`            | –        | IANA time zone name. The binary bundles `time/tzdata`, so this works in the distroless image. |

Remaining settings are configured in the UI dialog and stored in
`<DATA_DIR>/appdata/config.json` (without secrets). The general AI endpoint and
the embeddings can use separate endpoints, deployments and API versions in
manual mode; empty embedding fields fall back to the values of the AI endpoint.
When `AZURE_RESOURCE_ID` is absent, this existing API-key/manual mode is retained.
Setting a resource ID without a valid identity does not fall back to API keys.

Two endpoint schemas are detected automatically: the classic Azure OpenAI format
(`https://<resource>.openai.azure.com`, deployment in the path, `api-version`
required) and the new OpenAI compatible **v1 format** of Azure AI Foundry,
recognizable by the `/openai/v1` path
(`https://<resource>.services.ai.azure.com/openai/v1`). With the v1 format the
deployment is passed as `model` in the request and `api-version` is optional.
The chat and embedding endpoints may use different schemas in manual mode.

### Foundry inventory & identity (optional)

For an installation **outside Azure**, provide these four environment settings
for an existing service principal and one existing Foundry/Azure OpenAI account:

`AZURE_RESOURCE_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
`AZURE_CLIENT_SECRET`. For a new installation, follow the
[service-principal setup](#service-principal-setup-and-operation) below.

Inject the real secret through your deployment environment or secret manager,
not through `config.json` or committed files. The resource ID only opts in; it
does **not** authenticate the application by itself. The endpoint is derived
from valid account metadata returned by Azure Resource Manager (ARM).
`AZURE_ENDPOINT` is an optional explicit OpenAI-compatible `/openai/v1` endpoint
override, for example when metadata does not provide a usable endpoint. Thus
the basic automatic-endpoint setup needs four settings. Add
`AZURE_IMAGE_RESOURCE_ID` when image deployments live in another account; no
additional identity or secret is needed. Endpoint overrides remain optional.
The distroless image remains a single static, non-root Go binary: it contains
no Azure CLI and requires no interactive login.

Have an administrator assign **Cognitive Services OpenAI User**, scoped to that
one resource, as a starting role. Verify that the identity has the required
account/deployment metadata read permissions and the data-plane permissions for
the operations you will use. Some model types or operations, including image
operations, may need additional operation-specific permissions; this role is
not a promise that every Foundry model or protocol is usable. Inventory visibility
alone does not prove inference permission. The app never grants roles.

Discovery reads ARM metadata for the configured account and its deployments.
With no separate image resource, it also reads
`/openai/v1/models?api-version=preview` on the same inference endpoint for
known GPT-Image model IDs. With a separate image resource, only actual ARM
deployments from that account populate the image picker; neither account's
Models API is queried. Discovery does not scan subscriptions, read account keys,
create deployments or provision resources. A data-plane model list alone is
insufficient to establish chat/embedding deployment capabilities.
The inventory classifies canonical model
name, version, format, provisioning state, SKU and available capabilities, not
guesses based on an arbitrary deployment alias. Unsupported models, native
Anthropic deployments and batch-only deployments remain visible with a reason,
but cannot be used by the pickers. The current client routes OpenAI-compatible
chat completions, embeddings, image generation and image edits; discovery is
not universal Foundry protocol support.

The inventory groups chat, embeddings, and image models and shows their
capabilities, version, format, and source. **Models API** image entries can be
available by name without appearing as ARM deployments. Provider aliases are
preferred over duplicate dated image variants, and an ARM deployment always
wins a name collision. Listing a model proves neither that it is deployed in
that resource nor that generation is permitted. An image deployment in another
resource requires `AZURE_IMAGE_RESOURCE_ID`, not a matching catalog model ID.
If the image catalog cannot be read, the ARM inventory still updates and any
previous image entries for the same resource/endpoint are retained with a warning.

The supported-model mapping includes GPT chat models, model router,
OpenAI text embeddings and GPT-Image generation/editing. DALL-E image options
and Cohere embedding request formats need separate adapters and are not
selectable in Foundry mode. New chat model names can use recognized Azure
chat/vision capability metadata when their model format is already supported.
Known model profiles cover deployments that omit those hints, including
GPT-5.6, GPT-6 Astra, GPT-chat-latest and Grok 4.3. Unsupported protocols,
Responses-only models and batch deployments remain excluded.

Automatic image requests use a function tool bound to the configured image
deployment, not a model chosen by the chat response. GPT-6 Astra and GPT-5.5/5.6
use the Responses API for tool-enabled turns; requests are stateless
(`store=false`), and encrypted reasoning items are retained only within the
current tool loop. Other supported chat models use Chat Completions tools.
The selected text model and conversation mode are not changed by image delegation.
The chat deployment must support function tools on its selected API. The manual
image button bypasses chat-model orchestration and remains available.

In **Settings**, use **Refresh**, select the chat, embedding, image and optional
vision defaults, **Save**, then run **Check again**. Refresh fetches metadata
only: it neither invokes models nor changes role defaults. A failed refresh
keeps the last successful inventory and displays the failure. Connection
verification is separate and can consume chat/embedding tokens.

Chat, vision and embeddings use the primary account. Image generation and
editing use `AZURE_IMAGE_RESOURCE_ID` when set, otherwise the primary account.
Endpoint overrides must match the discovered account for their operation.
Separate-resource API-key configurations remain available in manual mode.
`AZURE_MODELS` and
`AZURE_IMAGE_MODELS`, when supplied in Foundry mode, restrict the discovered
supported choices; they cannot turn an unsupported or undiscovered deployment
into a supported one.
The settings dialog warns when these environment filters hide compatible
deployments. Remove obsolete filters if you want to select from the complete
discovered inventory.

### Service-principal setup and operation

Open [Azure Cloud Shell](https://shell.azure.com) in **Bash** mode in the tenant
that owns your Foundry resource. Your user needs permission to create app
registrations/service principals and assign roles on that resource.
Replace the three placeholders and run:

```bash
az account set --subscription '<subscription-id>' &&
RESOURCE_ID="$(az cognitiveservices account show \
  --resource-group '<resource-group>' \
  --name '<foundry-account-name>' \
  --query id --output tsv)" &&
test -n "$RESOURCE_ID" &&
az ad sp create-for-rbac \
  --name "ai-ui-$(date -u +%Y%m%d-%H%M%S)" \
  --role 'Cognitive Services OpenAI User' \
  --scopes "$RESOURCE_ID" \
  --years 1 \
  --output json &&
printf 'AZURE_RESOURCE_ID=%s\n' "$RESOURCE_ID"
```

This creates an app/service principal with a timestamped name, grants access
only to that Foundry account, and creates a secret valid for **one year**
(subject to tenant policy). The account name is the Azure resource name, not
a Foundry project name. The `&&` chain stops on errors.

Copy the output into your container's environment settings:

| Output value | Container variable |
| --- | --- |
| Printed `AZURE_RESOURCE_ID` | `AZURE_RESOURCE_ID` |
| JSON `tenant` | `AZURE_TENANT_ID` |
| JSON `appId` | `AZURE_CLIENT_ID` |
| JSON `password` | `AZURE_CLIENT_SECRET` |

**Copy and securely store `password` immediately**; Azure does not let you read
its value again. Do not commit or share the output, or put the secret in Compose
YAML or `config.json`. If you change the generated name, use an unused name:
`create-for-rbac` can modify an existing app with a matching display name.
This is a one-time creation command, not a container startup or rotation command.

[docker-compose.example.yml](docker-compose.example.yml) already maps these four
variables. Start/recreate the container with the values, then open
**Settings > Refresh deployments**, select the models, **Save**, and
**Check again**. Allow a few minutes for new role assignments to propagate;
model verification and use can incur Azure charges.

Before expiry, add a new secret to the same app, update the container's
environment and recreate it. Remove the old secret only after testing the new
one; retain the data volume. See the
[Azure CLI service-principal guide](https://learn.microsoft.com/en-us/cli/azure/azure-cli-sp-tutorial-1)
for details.

### Separate image resource

If the image deployment belongs to a different Foundry/Azure OpenAI account,
set `AZURE_IMAGE_RESOURCE_ID` to that account's ARM resource ID. Keep
`AZURE_RESOURCE_ID` and the existing tenant/client/secret settings unchanged.
The accounts may be in different resource groups or subscriptions, but must be
accessible to the same service principal in the same Entra tenant. The app
does not search projects or subscriptions automatically.

In Cloud Shell **Bash**, signed into that tenant, grant the existing service
principal access to the image account. Replace the placeholders; the caller
needs permission to read the account/service principal and create role
assignments at the image resource scope:

```bash
IMAGE_SUBSCRIPTION='<image-subscription-id>' &&
IMAGE_RESOURCE_ID="$(az cognitiveservices account show \
  --subscription "$IMAGE_SUBSCRIPTION" \
  --resource-group '<image-resource-group>' \
  --name '<image-account-name>' --query id --output tsv)" &&
SP_OBJECT_ID="$(az ad sp show \
  --id '<existing-AZURE_CLIENT_ID>' --query id --output tsv)" &&
test -n "$IMAGE_RESOURCE_ID" && test -n "$SP_OBJECT_ID" &&
az role assignment create \
  --subscription "$IMAGE_SUBSCRIPTION" \
  --assignee-object-id "$SP_OBJECT_ID" \
  --assignee-principal-type ServicePrincipal \
  --role 'Cognitive Services OpenAI User' \
  --scope "$IMAGE_RESOURCE_ID" --output none &&
printf 'AZURE_IMAGE_RESOURCE_ID=%s\n' "$IMAGE_RESOURCE_ID"
```

This adds resource-scoped permissions only. It neither creates a service
principal nor creates/rotates a secret. A Cloud Shell login-cache error needs
to be resolved in that shell; it does not by itself mean the app's service
principal credentials are invalid.

Add the printed variable to the container environment and recreate it.
Leave `AZURE_IMAGE_ENDPOINT` unset to derive the endpoint from the image
account; remove an old override that points to the primary resource.
In Settings, choose **Refresh deployments**, select the actual image
**deployment name**, save, then **Check again**. Allow time for role assignments
to propagate. `AZURE_IMAGE_DEPLOYMENT` can optionally pin that deployment, and
`AZURE_IMAGE_MODELS` remains an optional allow-list for that image resource.

The image account has its own cached ARM inventory, read-only resource/endpoint
fields and connection-check row. Refresh attempts both accounts even if one
fails, retains each last successful cache separately, and never falls back to
the primary endpoint when the image account is unavailable. Metadata updates
are reflected immediately in the displayed inventory checks without silently
repeating inference probes. Chat/embedding readiness and the stored embedding
profile are not reset by an image-only inventory change; changing just the
image resource does **not** require rebuilding embeddings.

### Switching the embedding model

Every index records its endpoint, deployment, API version (for classic
endpoints) and vector dimensions. Foundry indexes additionally record the
resource and canonical model/version. This applies to both identity-backed
Foundry and manual/API-key configuration. Saving a different embedding default
does **not** relabel or mix existing vectors. A legacy corpus with no known
embedding profile also needs an explicit rebuild before it can be used safely.
An older classic-endpoint profile without a recorded API version likewise
requires rebuilding with an explicitly configured version.

Save the new embedding selection, review the document/chunk counts and cost
warning, then explicitly consent to **Rebuild embedding index** in Settings.
Rebuilding embeds the stored chunk texts into a staged index; it does not
reparse the original files or repeat OCR. It can process the whole corpus and
incur embedding API charges, so consider its size and your provider's current
pricing before confirming. No monetary estimate is guessed by the app.

Progress is shown in the dialog. Corpus mutations such as uploads and
document/chat deletion are temporarily blocked while the rebuild runs. Only a
complete successful rebuild becomes active; failure leaves the previous index
intact. The old profile remains separate from the newly selected default until
the switch succeeds, rather than silently querying old vectors with a new model.

Manual embeddings can inherit the chat API key only when the endpoints have
the same scheme and host. A separate origin requires
`AZURE_EMBEDDING_API_KEY`. Previously configured embedding endpoints remain
authorized for the old index during the current process. After a restart,
only the current configuration authorizes a destination: a persisted profile
alone cannot send the current key to an old endpoint. If those settings are
incompatible, retrieval fails explicitly until you restore the matching
configuration or complete a rebuild. Switching between manual and Foundry
authentication also requires rebuilding; existing vectors are never relabeled.

The **Rebuild options** section also permits an explicitly confirmed rebuild
of the current profile, for example to repair an index or regenerate changed
vector dimensions. Out-of-band deployment changes are detected when inventory
metadata is refreshed; refresh after changing deployments in Azure.

### Several deployments, one configuration (manual mode)

All deployments of a resource share its endpoint and API key, so `AZURE_MODELS`
is all it takes to offer several models: list the deployment names, and the
picker in the top right switches between them. With the v1 schema the selected
name is sent as `model`, with the classic schema it becomes the deployment in
the request path - in both cases the request goes to that deployment. The entry
of `AZURE_DEPLOYMENT` answers "Auto (router)" and is the fallback.

Embeddings and image generation are separate APIs and therefore keep their own
deployment setting, but they can live in the same resource: leave their endpoint
empty and only name the deployment.

### Language

The interface language (English or German) is selected in the settings dialog
and applies to the whole application, including the prompts used for the
automatic chat titles and the document/web context. The page reloads once after
the language has been changed. New installations default to English.

### Temperature & reasoning effort

The temperature lives in the settings dialog under **Behavior** and applies to
every chat request. The **reasoning effort** (the `reasoning_effort` parameter of
reasoning models) is chosen **per chat** next to the input field; the settings
dialog only holds the default for new chats.

The offered values follow the model selected in the top right and are updated
when it changes: `none`, `low`, `medium`, `high`, `xhigh` for GPT-5.1 and newer,
`minimal`, `low`, `medium`, `high` for GPT-5, `low`, `medium`, `high` for the
o-series. Models without reasoning hide the field, and `auto` omits the parameter
so the model keeps its own default. A value the answering model does not accept -
which cannot be ruled out behind a model router - is dropped automatically and
the request is repeated without it, the same way an unsupported temperature is.

### Pinning the endpoint via environment variables (optional)

In manual mode the endpoint settings can be provided entirely through
environment variables instead of the UI dialog. When one of these variables is set, its value takes
precedence over `config.json` and the matching field in the settings dialog is
shown but disabled (not editable through the UI):

The naming scheme is consistent: the **general AI endpoint** uses the base names
`AZURE_*`, the **embeddings** consistently use `AZURE_EMBEDDING_*`.

General AI endpoint:

| Variable        | Setting                                       |
| --------------- | --------------------------------------------- |
| `AZURE_ENDPOINT` | Endpoint URL of the AI endpoint. In Foundry mode, an optional explicit override of the discovered endpoint. |
| `AZURE_DEPLOYMENT` | Deployment name of the chat model.         |
| `AZURE_MODELS` | Selectable models (comma or newline separated), e.g. `model-router,gpt-5.1,o4-mini`. In manual mode this is the source of the list, shown read-only; its entries are **deployment names of the same resource**. In Foundry mode it is an optional allow-list intersected with supported inventory entries. |
| `AZURE_API_VERSION` | API version of the AI endpoint. Only used by the classic schema; with a `/openai/v1` endpoint the client picks it and the field disappears from the settings dialog. |

Embeddings (fall back to the AI endpoint when empty):

| Variable        | Setting                                       |
| --------------- | --------------------------------------------- |
| `AZURE_EMBEDDING_ENDPOINT` | Embedding endpoint URL.               |
| `AZURE_EMBEDDING_DEPLOYMENT` | Deployment name of the embedding model. |
| `AZURE_EMBEDDING_API_VERSION` | Embedding API version.              |

Image generation (fall back to the AI endpoint when empty):

| Variable        | Setting                                       |
| --------------- | --------------------------------------------- |
| `AZURE_IMAGE_ENDPOINT` | Image endpoint URL, e.g. `https://my-resource.services.ai.azure.com/openai/v1`. |
| `AZURE_IMAGE_DEPLOYMENT` | Deployment name of the image model, e.g. `gpt-image-2`. |
| `AZURE_IMAGE_MODELS` | Selectable image deployments (comma or newline separated). In manual mode, empty ⇒ only `AZURE_IMAGE_DEPLOYMENT`; in Foundry mode, empty ⇒ supported inventory entries, otherwise an allow-list. In image mode the picker offers these instead of the chat models. |
| `AZURE_IMAGE_API_VERSION` | Image API version.                    |

The key is `AZURE_IMAGE_API_KEY`; when it is empty `AZURE_API_KEY` is used.
**A dedicated key is required as soon as the image endpoint belongs to a
different resource** - the chat key is rejected there with HTTP 401. The
settings dialog points that out, and the connection check probes the image
deployments as well with an intentionally incomplete validation request rather
than generating an image.
Size, quality and file format are chosen in the settings dialog.

The matching secrets are `AZURE_API_KEY` and `AZURE_EMBEDDING_API_KEY`
respectively (see the table above).

Variables that are not set stay editable in the UI. Empty values count as
"not set" and do not lock anything.

### Readiness & connection check

The connection check sits at the top of the settings dialog and runs at startup.
In manual mode it also runs in the dialog after configuration changes; in
Foundry mode save the role selections and explicitly choose **Check again**.
It probes storage (data path writable), chat and embeddings, and the selectable
chat deployments individually. A typo in a manual `AZURE_MODELS` list or an
inference permission error therefore appears before that model is picked.
**Refresh** in Foundry mode is a separate metadata-only operation.
Checks use the saved settings and name the model being tested. Unconfigured
optional features and checks not run by the periodic monitor are shown as
skipped, not as failures. Cached inventory checks are labeled as metadata.
The vision check verifies the chat route, not image understanding. The image
check sends no prompt: only the expected missing-prompt response is accepted,
and the result explicitly says that generation is untested. An arbitrary 400,
such as `unknown_model`, is an error, not a green check. Use a real image request
when you want to verify generation, bearing in mind the associated charges.

Document uploads are only enabled once storage and the embedding endpoint are
green, because a document is chunked and embedded on the way in. Attaching an
image needs neither: it is stored as is and travels with the message. A scanned
PDF additionally needs a supported vision deployment (a vision-capable
`AZURE_MODELS` entry in manual mode, or the selected chat/vision role in Foundry
mode); without one the upload fails with that reason instead of storing an
empty document. A
background check (`HEALTHCHECK_INTERVAL`) monitors the connection continuously -
without the per-deployment probes - and reports failures through the sidebar
status and the log.

Office imports preserve missing Excel columns and workbook tab order, associate
PowerPoint notes with their actual slides, and retain text in nested Word
tables. RTF imports decode UTF-16 surrogate pairs and scoped Unicode fallbacks.
Malformed selected Office parts and resource-limit violations fail the upload
instead of silently returning an incomplete extract. Each Office archive is
limited to 64 MiB per part, 128 MiB total inflated data, 256 MiB processing work,
2,000 entries/part reads, two million XML tokens, nesting depth 256 and 8 MiB
extracted text.

Parser improvements apply to newly uploaded documents. To replace an old
incorrect extract, remove and upload that document again; rebuilding embeddings
alone reuses the already stored text.

### Web search (optional)

Pick a provider in the settings dialog under **Web search**:

- **Tavily** – optimized for LLM/RAG, returns already extracted content
  (requires `SEARCH_API_KEY`).
- **Brave Search** – REST API (requires `SEARCH_API_KEY`).
- **SearXNG** – self-hosted meta search; only the base URL is needed, no key.

When a provider is configured, a 🌐 toggle appears next to the chat input. While
it is active, the message is enriched with current web results; the state
survives switching chats. The search API key is - like the Azure keys - read
exclusively from the `SEARCH_API_KEY` environment variable and never stored in
`config.json`.

The SearXNG base URL is fetched by the server, so it is validated: only
`http`/`https` URLs are accepted, and connections to loopback and link-local
addresses (including the cloud metadata service `169.254.169.254`) are refused.
Private LAN ranges stay reachable because that is where a self-hosted instance
usually lives.

## Security

- Single static binary on a distroless base image, running as non-root
  (UID/GID `65532`); the container image is signed with cosign and scanned with
  Trivy for both `linux/amd64` and `linux/arm64`
- Releases run the same validation workflow as CI. Images are first pushed by
  digest without changing public tags; `latest`, `stable` and version/SHA tags
  are promoted only after both architecture scans and keyless signing succeed.
  HIGH/CRITICAL findings (including unfixed ones) or failed checks block
  promotion, and scan reports remain available in GitHub Code Scanning.
- Secrets are read from environment variables only and are never written to
  `config.json`
- All SQL statements are parameterized; no user input is concatenated into
  queries
- Model and document content is rendered as sanitized Markdown - raw HTML is
  escaped, never injected
- Defensive response headers on every request: `Content-Security-Policy`,
  `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`,
  `Cross-Origin-Opener-Policy` and `Permissions-Policy`
- Uploads are limited to 25 MiB per file and 150 MiB per request; the extracted
  text is capped, the OOXML formats (`.docx`, `.xlsx`, `.pptx`) are bounded
  against decompression bombs, and the PDF reader is fed untrusted input behind
  a panic guard
- The app is meant to run inside a trusted network or behind a reverse
  proxy/VPN. It has no user accounts and should not be exposed to the internet
  unprotected

## Running locally

Sending a message creates a durable generation tied to that exact message.
SSE connections observe or replay it; reconnecting or reloading does not start
another provider request. Each chat accepts one active generation at a time,
with at most four chat/image generations across the application. Additional
submissions receive an explicit busy response without storing a message or
starting a provider request; they are not automatically queued or retried.
Closing the browser does not cancel that generation, which has a ten-minute
timeout. Application shutdown cancels and joins active workers; after a restart,
interrupted operations are marked as such and are never automatically retried
because the provider may already have processed them. Optional title generation
has a separate fifteen-second timeout.

Static asset URLs carry content versions. Opening settings also updates the
page's stylesheet, so a page left open during a container update does not render
new controls with old CSS.

```sh
export AZURE_API_KEY=your-key
DATA_DIR=./data PORT=8080 go run .
# http://localhost:8080
```

## Docker

```sh
docker build -t ai-ui .
docker run --rm -p 8080:8080 \
  -e AZURE_API_KEY=your-key \
  -v ai-ui-data:/appdata \
  ai-ui
```

### Data path permissions (non-root)

The container runs as a non-root user (**UID/GID `65532`**) and stores all data
(chats, documents, embeddings, configuration) under `/appdata`. A Docker managed
**named volume** is used as persistent storage:

```yaml
services:
  ai-ui:
    image: ghcr.io/daknoblo/ai-ui:latest
    volumes:
      - ai-ui-data:/appdata
volumes:
  ai-ui-data:
```

A freshly created named volume inherits its ownership from the image (`65532`)
and therefore works out of the box - including in Dockge/Portainer as a normal
user, without any manual permission handling. Docker manages the volume; no
changes on the host are required.

## Deployment

[docker-compose.example.yml](docker-compose.example.yml) contains **one**
`ai-ui` container with a named volume and the published port `8080`; Traefik
labels are included but commented out. The project is designed for exactly one
container - how many instances of it you run is up to you (e.g. several services
in a single stack). The image is built and published to
`ghcr.io/daknoblo/ai-ui` by GitHub Actions: `latest` and `stable` from `main`,
and the version tags (`1.1.0`, `1.1`) when a release is published. Note that the
image tag carries no `v` prefix even though the git tag does.

## Development

```sh
gofmt -l .                  # must print nothing
go vet ./...
golangci-lint run ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

User interface strings live in [internal/i18n](internal/i18n/i18n.go). Every key
must exist in all supported languages; a test enforces that.

## Demo & documentation

The repository contains a demo instance that needs neither an API key nor any
Azure resources: [internal/demo](internal/demo) provides a stub of the
Azure-compatible endpoints (chat streaming, embeddings, images) and seeds the
database with conversations, documents, a generated image and token statistics.
The legacy manual demo remains available. `-foundry` opts into the new inventory
using a fake identity and loopback-only v1 endpoints, never an Azure credential
or resource. Aliases such as `chat-primary`, `docs-primary` and `canvas` map to
canonical GPT-4o, text-embedding-3-large and gpt-image-1 metadata; native and
batch-only examples are visible but unusable. Refresh, role defaults and
reindexing operate on the local fixture.

```sh
go run ./cmd/demo -data ./data/demo                        # Manual demo
go run ./cmd/demo -foundry -data ./data/demo-foundry        # Foundry demo
go run ./cmd/demo -foundry -data ./data/demo-foundry-de -lang de
go run ./cmd/demo -foundry -separate-images -data ./data/demo-images
# http://localhost:8080
```

Use separate data paths for the two modes, or `-reset` to discard a previous
demo fixture. Embeddings are deterministic hashed demo vectors, not real model
output. The Foundry fixture initializes its profile through the same staged
reindex workflow over stored texts, rather than relabeling legacy vectors. A
second local embedding alias lets you try the consent and rebuild flow without
API costs. Web search remains a display-only placeholder in the demo.

The `-separate-images` option adds a second loopback backend and a separate
image account with actual GPT-Image 2/1.5 deployment metadata, using the same
fake identity. The original primary resource and embedding profile stay intact.

The demo is also the source of the screenshots. The capture script starts it
with `-foundry -separate-images`, exercises both local inventories, role defaults and reindex consent
views, and captures those sections with Playwright. The resource and endpoint
remain truthful read-only demo values; it does not replace them with real Azure
URLs or contact real services. Screenshots are written to `docs/screenshots`,
together with a manifest describing every shot:

```sh
CGO_ENABLED=0 go build -o bin/ai-ui-demo ./cmd/demo
cd tools/screenshots && npm ci && npx playwright install chromium
node capture.mjs --bin=../../bin/ai-ui-demo --out=../../docs/screenshots
```

[cmd/site](cmd/site) turns this README and the screenshots into the static
website that is published on GitHub Pages:

```sh
go run ./cmd/site -out site   # open site/index.html
```

The `Docs` workflow runs all three steps on every push to `main` that touches
the application, the templates or this README: it recaptures the screenshots,
commits them when they changed and deploys the regenerated website. New features
therefore appear in the documentation without a manual screenshot session.

## License

Released under the [MIT License](LICENSE).
