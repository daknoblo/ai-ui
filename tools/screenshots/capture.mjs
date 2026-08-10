// Captures the documentation screenshots from the demo instance.
//
// For every language it starts `cmd/demo` on a scratch data path, walks through
// the sections and writes one PNG per shot plus a manifest that the site
// generator (cmd/site) turns into the gallery.
//
//   node capture.mjs --bin=../../bin/ai-ui-demo --out=../../docs/screenshots
//
// The demo needs no credentials: it runs against the stub backend in
// internal/demo.

import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
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

// Endpoint values shown in the settings dialog. The demo really talks to a stub
// on 127.0.0.1 with a random port, which says nothing about the product, so the
// fields are filled with the values a real installation would hold. Nothing is
// saved: this is display only.
const SETTINGS_DISPLAY = {
  endpoint: 'https://my-foundry.services.ai.azure.com/openai/v1',
  embedding_endpoint: 'https://my-embeddings.openai.azure.com',
  image_endpoint: 'https://my-images.openai.azure.com',
  search_endpoint: 'https://searxng.internal.example',
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
        caption: 'Uploads are chunked, embedded and stored next to the chat; the attachments stay visible above the input.',
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
    id: 'settings',
    langs: ['en', 'de'],
    meta: {
      en: {
        title: 'Settings dialog',
        caption: 'Language, endpoints, deployments, image parameters, web search and log level - all in the UI. API keys stay in the environment.',
      },
      de: {
        title: 'Einstellungsdialog',
        caption: 'Sprache, Endpunkte, Deployments, Bildparameter, Websuche und Log-Level - alles in der Oberfläche. API-Schlüssel bleiben in der Umgebung.',
      },
    },
    capture: async (page, ctx) => {
      await open(page, `/chat/${ctx.index.chats.chat}`);
      await page.click('.btn-config[hx-get="/config"]');
      await page.waitForSelector('.modal-overlay .modal');
      for (const [name, value] of Object.entries(SETTINGS_DISPLAY)) {
        const field = page.locator(`.config-form input[name="${name}"]`);
        if ((await field.count()) > 0) {
          await field.fill(value);
        }
      }
      // Filling the fields scrolls the dialog; the shot should start at the top.
      await page.locator('.modal-overlay .modal').evaluate((modal) => modal.scrollTo(0, 0));
      await sleep(200);
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

/** Starts the demo binary on a scratch data path. */
function startDemo(dataDir, lang) {
  const proc = spawn(binary, ['-port', String(port), '-data', dataDir, '-lang', lang, '-reset'], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  proc.stderr.on('data', (chunk) => process.stderr.write(`[demo] ${chunk}`));
  return proc;
}

/** Waits until the demo answers on /healthz. */
async function waitForDemo() {
  for (let attempt = 0; attempt < 100; attempt++) {
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
    const dataDir = await mkdtemp(join(tmpdir(), `ai-ui-demo-${lang}-`));
    const demo = startDemo(dataDir, lang);
    try {
      await waitForDemo();
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
      demo.kill('SIGTERM');
      await once(demo, 'exit');
      await rm(dataDir, { recursive: true, force: true });
    }
  }

  await writeFile(join(outDir, 'manifest.json'), `${JSON.stringify({ shots: manifest }, null, 2)}\n`);
  process.stdout.write(`wrote ${manifest.length} screenshots to ${outDir}\n`);
}

await main();
