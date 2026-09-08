// Captures the documentation screenshots from the demo instance.
//
// For every language it starts `cmd/demo` on a scratch data path, walks through
// the sections and writes one PNG per shot plus a manifest that the site
// generator (cmd/site) turns into the gallery.
//
//   node capture.mjs --bin=../../bin/ai-ui-demo --out=../../docs/screenshots
//
// The Foundry demo needs no credentials: inventory, identity and inference all
// use the local fixture in internal/demo.

import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright';

const args = Object.fromEntries(
  process.argv.slice(2).map((arg) => {
    const [key, value] = arg.replace(/^--/, '').split('=');
    return [key, value ?? true];
  }),
);

const outDir = resolve(args.out ?? 'docs/screenshots');
const binary = resolve(args.bin ?? 'bin/ai-ui-demo');
const port = Number(args.port ?? 8123);
const base = `http://127.0.0.1:${port}`;
const languages = String(args.langs ?? 'en,de').split(',');

// By default the Chromium that `playwright install` downloaded is used. On
// machines where that download is unavailable, --channel=chrome or
// PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH point at a locally installed browser.
const launchOptions = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
  ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
  : { channel: args.channel ?? process.env.PLAYWRIGHT_CHANNEL ?? undefined };

const DESKTOP = { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 };
const MOBILE = {
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 2,
  isMobile: true,
  hasTouch: true,
};

/** The sections that are captured, in gallery order. */
const SHOTS = [
  {
    id: 'chat',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Chat with Markdown answers',
        caption: 'Conversations in the sidebar, model picker in the header, answers rendered as sanitized Markdown including tables and code.',
      },
      de: {
        title: 'Chat mit Markdown-Antworten',
        caption: 'Unterhaltungen in der Seitenleiste, Modellauswahl in der Kopfzeile, Antworten als bereinigtes Markdown inklusive Tabellen und Code.',
      },
    },
    capture: (page, ctx) => open(page, `/chat/${ctx.index.chats.chat}`),
  },
  {
    id: 'streaming',
    langs: ['en'],
    meta: {
      en: {
        title: 'Answers stream token by token',
        caption: 'The answer arrives over server-sent events; the model used and the token usage are shown once the stream ends.',
      },
    },
    capture: async (page, ctx) => {
      await open(page, '/');
      await page.fill('#chat-form textarea', ctx.index.stream_prompt);
      await page.press('#chat-form textarea', 'Enter');
      await page.waitForFunction(() => {
        const bubble = document.querySelector('#messages .msg.assistant .bubble');
        return bubble !== null && bubble.innerText.trim().length > 60;
      }, null, { timeout: 30_000 });
      await sleep(400);
    },
  },
  {
    id: 'documents',
    langs: ['en'],
    meta: {
      en: {
        title: 'Documents as chat context (RAG)',
        caption: 'PDF, Word, Excel, PowerPoint, text and code are parsed, chunked and embedded next to the chat; the attachments stay visible above the input.',
      },
    },
    capture: (page, ctx) => open(page, `/chat/${ctx.index.chats.documents}`),
  },
  {
    id: 'websearch',
    langs: ['en'],
    meta: {
      en: {
        title: 'Optional web search',
        caption: 'With the 🌐 toggle a request is enriched with current results from Tavily, Brave Search or a self-hosted SearXNG.',
      },
    },
    storage: { 'ai-ui-web': '1' },
    capture: (page, ctx) => open(page, `/chat/${ctx.index.chats.websearch}`),
  },
  {
    id: 'image',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Image generation and editing',
        caption: 'The 🖼 mode turns the next message into a generated image; an attached image is edited instead of created from scratch.',
      },
      de: {
        title: 'Bilder erzeugen und bearbeiten',
        caption: 'Der Modus 🖼 macht aus der nächsten Nachricht ein erzeugtes Bild; ein angehängtes Bild wird stattdessen bearbeitet.',
      },
    },
    capture: (page, ctx) => open(page, `/chat/${ctx.index.chats.image}`),
  },
  {
    id: 'chat-image',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Image requests in ordinary chat',
        caption: 'The chat model delegates an explicit image request to the configured image model. The result appears inline while the conversation stays in chat mode.',
      },
      de: {
        title: 'Bildaufträge direkt im Chat',
        caption: 'Das Chat-Modell übergibt einen ausdrücklichen Bildauftrag an das konfigurierte Bildmodell. Das Bild erscheint direkt in der Unterhaltung; der Chatmodus bleibt aktiv.',
      },
    },
    capture: async (page) => {
      await open(page, '/');
      const lang = await page.locator('html').getAttribute('lang');
      await page.fill('#chat-form textarea', lang === 'de'
        ? 'Erzeuge ein Bild von einem blauen Park.'
        : 'Generate an image of a blue park.');
      await page.locator('#chat-form button[type="submit"]').click();
      await page.locator('#messages .msg.assistant .bubble img').last().waitFor();
      if (await page.locator('.composer').getAttribute('data-mode') !== 'chat') {
        throw new Error('Automatic image generation must preserve chat mode');
      }
    },
  },
  {
    id: 'settings',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Foundry deployment inventory',
        caption: 'Refresh reads metadata for the configured resources. Canonical models and capabilities explain which deployment aliases are usable; the local demo needs no credentials.',
      },
      de: {
        title: 'Foundry-Deployment-Inventar',
        caption: 'Aktualisieren liest Metadaten der eingestellten Ressourcen. Kanonische Modelle und Fähigkeiten zeigen, welche Deployment-Aliase nutzbar sind; die lokale Demo braucht keine Zugangsdaten.',
      },
    },
    capture: async (page, ctx) => {
      await openSettings(page, ctx);
      await submitSettings(page, '[hx-post="/config/deployments/refresh"]', '/config/deployments/refresh');
      await scrollSettingsTo(page, '.foundry-resource');
    },
  },
  {
    id: 'image-resource',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'A separate resource for image models',
        caption: 'AZURE_IMAGE_RESOURCE_ID selects a separate ARM deployment inventory and endpoint using the same identity. Chat and embeddings remain on the primary resource.',
      },
      de: {
        title: 'Eine getrennte Ressource für Bildmodelle',
        caption: 'AZURE_IMAGE_RESOURCE_ID wählt ein eigenes ARM-Deployment-Inventar samt Endpoint mit derselben Identität. Chat und Embeddings bleiben auf der Hauptressource.',
      },
    },
    capture: async (page, ctx) => {
      await openSettings(page, ctx);
      await page.locator('#foundry-image-resource-id').waitFor();
      await scrollSettingsTo(page, '#foundry-image-resource');
    },
  },
  {
    id: 'model-defaults',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Defaults for each operation',
        caption: 'Chat, embedding, image and vision defaults use supported inventory entries. The active embedding profile and completed local rebuild are shown below.',
      },
      de: {
        title: 'Standard-Deployments je Aufgabe',
        caption: 'Chat, Embeddings, Bilder und Vision verwenden unterstützte Inventar-Einträge. Darunter stehen das aktive Embedding-Profil und der abgeschlossene lokale Neuaufbau.',
      },
    },
    capture: async (page, ctx) => {
      await openSettings(page, ctx);
      await page.locator('#embedding-index .config-saved').waitFor();
      await scrollSettingsTo(page, 'select[name="chat_deployment"]');
    },
  },
  {
    id: 'embedding-reindex',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Embedding changes require consent',
        caption: 'Saving a new embedding default does not relabel the old vectors. The staged rebuild requires explicit consent after showing corpus counts and an API cost warning.',
      },
      de: {
        title: 'Embedding-Wechsel mit Bestätigung',
        caption: 'Ein neuer Embedding-Standard ändert bestehende Vektoren nicht. Der getrennte Neuaufbau erfordert eine Bestätigung mit Dokumentanzahl und Hinweis auf API-Kosten.',
      },
    },
    capture: async (page, ctx) => {
      await openSettings(page, ctx);
      const embedding = page.locator('.config-form select[name="embedding_deployment"]');
      const current = await embedding.inputValue();
      const alternative = await embedding.locator('option').evaluateAll(
        (options, selected) => options.map((option) => option.value).find((value) => value && value !== selected),
        current,
      );
      if (!alternative) throw new Error('Foundry demo has no alternative embedding deployment');
      await embedding.selectOption(alternative);
      await submitSettings(page, '.config-form button[type="submit"]', '/config');
      await page.locator('#embedding-index input[name="confirm_reindex"]').waitFor();
      await scrollSettingsTo(page, 'select[name="chat_deployment"]');
    },
  },
  {
    id: 'stats',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Token statistics',
        caption: 'Persisted usage per day and per model, including embeddings, images and the size of the data path.',
      },
      de: {
        title: 'Token-Statistik',
        caption: 'Dauerhaft gespeicherter Verbrauch pro Tag und Modell, inklusive Embeddings, Bildern und Größe des Datenpfads.',
      },
    },
    capture: (page) => open(page, '/stats'),
  },
  {
    id: 'logs',
    langs: ['en'],
    meta: {
      en: {
        title: 'Live log',
        caption: 'The last log lines in the browser, with a level filter - handy behind a reverse proxy without shell access.',
      },
    },
    capture: (page) => open(page, '/logs'),
  },
  {
    id: 'mobile',
    langs: ['en'],
    device: MOBILE,
    meta: {
      en: {
        title: 'Phone layout',
        caption: 'On a narrow viewport the sidebar turns into an off-canvas drawer behind the ☰ button, so the conversation keeps the full width.',
      },
    },
    capture: (page, ctx) => open(page, `/chat/${ctx.index.chats.chat}`),
  },
];

/** Opens a page of the demo and waits until it has settled. */
async function open(page, path) {
  await page.goto(base + path, { waitUntil: 'networkidle' });
  await page.evaluate(() => document.fonts.ready);
}

async function openSettings(page, ctx) {
  if (!ctx.index.foundry) throw new Error('Settings screenshots require the Foundry demo fixture');
  await open(page, `/chat/${ctx.index.chats.chat}`);
  await page.click('.btn-config[hx-get="/config"]');
  await page.locator('.foundry-inventory').waitFor();
  const resource = page.locator('#foundry-resource-id');
  const endpoint = page.locator('#foundry-endpoint');
  if (!(await resource.inputValue()).includes('/providers/Microsoft.CognitiveServices/accounts/')) {
    throw new Error('Foundry resource ID must be visible');
  }
  if (await resource.isEditable() || await endpoint.isEditable()) {
    throw new Error('Discovered connection fields must be read-only');
  }
  if (!(await endpoint.inputValue()).startsWith('http://127.0.0.1:')) {
    throw new Error('Settings demo must keep its real loopback endpoint');
  }
  for (const name of ['chat_deployment', 'embedding_deployment', 'image_deployment', 'vision_deployment']) {
    const field = page.locator(`.config-form select[name="${name}"]`);
    await field.waitFor();
    if (!(await field.inputValue())) throw new Error(`Foundry demo default is missing: ${name}`);
  }
  // Foundry renders connection metadata read-only, not as per-role URL inputs.
  if (await page.locator('.config-form input[name="endpoint"], .config-form input[name="embedding_endpoint"], .config-form input[name="image_endpoint"]').count()) {
    throw new Error('Foundry settings unexpectedly contain editable inference endpoints');
  }
  await page.locator('#embedding-index').waitFor();
}

async function submitSettings(page, selector, path) {
  const responsePromise = page.waitForResponse(
    (response) => response.url() === base + path && response.request().method() === 'POST',
  );
  await page.locator(selector).click();
  const response = await responsePromise;
  if (!response.ok()) throw new Error(`Settings request failed: ${path} (${response.status()})`);
  await page.waitForFunction(() => !document.querySelector('#modal-root .htmx-request'));
  if (await page.locator('.config-notice-err').count()) {
    throw new Error(`Settings request reported an error: ${path}`);
  }
}

async function scrollSettingsTo(page, selector) {
  await page.locator(`.config-form ${selector}`).first().evaluate((element) => {
    const anchor = element.closest('label') ?? element;
    const modal = element.closest('.modal');
    modal.scrollTop += anchor.getBoundingClientRect().top - modal.getBoundingClientRect().top - 90;
  });
  await sleep(200);
}

/** Starts the demo binary on a scratch data path. */
function startDemo(dataDir, lang) {
  const proc = spawn(binary, ['-port', String(port), '-data', dataDir, '-lang', lang, '-reset', '-foundry', '-separate-images'], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  proc.stderr.on('data', (chunk) => process.stderr.write(`[demo] ${chunk}`));
  return proc;
}

/** Waits until the demo answers on /healthz. */
async function waitForDemo(proc) {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (proc.exitCode !== null || proc.signalCode !== null) {
      throw new Error(`demo exited before startup (${proc.exitCode ?? proc.signalCode})`);
    }
    try {
      const response = await fetch(`${base}/healthz`);
      if (response.ok) return;
    } catch {
      // not up yet
    }
    await sleep(100);
  }
  throw new Error(`demo did not start on ${base}`);
}

async function main() {
  await rm(outDir, { recursive: true, force: true });
  const manifest = [];

  for (const lang of languages) {
    const dataDir = await mkdtemp(resolve(`.ai-ui-demo-${lang}-`));
    const demo = startDemo(dataDir, lang);
    try {
      await waitForDemo(demo);
      const index = JSON.parse(await readFile(join(dataDir, 'demo-index.json'), 'utf8'));
      const browser = await chromium.launch(launchOptions);
      try {
        await mkdir(join(outDir, lang), { recursive: true });
        for (const shot of SHOTS.filter((s) => s.langs.includes(lang))) {
          const context = await browser.newContext({
            ...(shot.device ?? DESKTOP),
            colorScheme: 'dark',
            reducedMotion: 'reduce',
            locale: lang === 'de' ? 'de-DE' : 'en-US',
          });
          if (shot.storage) {
            const entries = Object.entries(shot.storage);
            await context.addInitScript((items) => {
              for (const [key, value] of items) window.localStorage.setItem(key, value);
            }, entries);
          }
          const page = await context.newPage();
          await shot.capture(page, { index, lang });
          const file = `${lang}/${shot.id}.png`;
          await page.screenshot({ path: join(outDir, file), animations: 'disabled' });
          const viewport = page.viewportSize();
          manifest.push({
            id: shot.id,
            lang,
            file,
            width: viewport.width,
            height: viewport.height,
            ...shot.meta[lang],
          });
          await context.close();
          process.stdout.write(`captured ${file}\n`);
        }
      } finally {
        await browser.close();
      }
    } finally {
      if (demo.exitCode === null && demo.signalCode === null) {
        demo.kill('SIGTERM');
        await once(demo, 'exit');
      }
      await rm(dataDir, { recursive: true, force: true });
    }
  }

  await writeFile(join(outDir, 'manifest.json'), `${JSON.stringify({ shots: manifest }, null, 2)}\n`);
  process.stdout.write(`wrote ${manifest.length} screenshots to ${outDir}\n`);
}

await main();
