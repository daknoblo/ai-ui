// Deterministic streaming events exercise the real HTMX extension and scroll UI.
// Documentation screenshots use the real local demo stream, not this fixture.
export async function verifyChatScroll(page, base, index, mobile) {
  await page.addInitScript(() => {
    window.scrollTestSources = [];
    window.EventSource = class extends EventTarget {
      static CONNECTING = 0;
      static OPEN = 1;
      static CLOSED = 2;
      constructor(url) {
        super();
        this.url = url;
        this.readyState = 1;
        window.scrollTestSources.push(this);
        queueMicrotask(() => this.onopen?.(new Event('open')));
      }
      close() { this.readyState = 2; }
    };
  });

  await page.goto(`${base}/`, { waitUntil: 'networkidle' });
  const testChat = new URL(page.url()).pathname;
  await page.evaluate(() => document.fonts.ready);
  await settled(page);
  await assertFollowing(page, 'opening a conversation');
  await page.locator('#messages').evaluate((box, html) => {
    const message = document.createElement('div');
    message.className = 'msg assistant';
    const bubble = document.createElement('div');
    bubble.className = 'bubble';
    bubble.innerHTML = html;
    message.append(bubble);
    box.append(message);
  }, paragraphs(80));
  await settled(page);
  await assertFollowing(page, 'loading existing conversation content');
  await scrollUp(page, mobile);
  const historyPosition = await position(page);
  if (historyPosition.following !== '0') throw new Error('Scrolling up did not pause following');
  await openSidebar(page);
  await page.locator('.btn-config[hx-get="/config"]').click();
  await page.locator('.modal-close').waitFor();
  await page.locator('.modal-close').click();
  await settled(page);
  await assertPosition(page, historyPosition, 'opening and closing settings');

  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  await page.waitForFunction(() => window.scrollTestSources.length > 0);
  await emit(page, 'token', paragraphs(80));
  await assertFollowing(page, 'submitting a new message while reading history');
  await scrollUp(page, mobile);
  const readingPosition = await position(page);
  if (readingPosition.following !== '0') throw new Error('Scrolling during generation did not pause following');
  await assertIndicator(page, 'streaming');
  for (const count of [85, 90, 95, 100]) {
    await emit(page, 'token', paragraphs(count));
    await assertPosition(page, readingPosition, `streaming ${count} paragraphs`);
  }
  if ((await position(page)).height <= readingPosition.height) {
    throw new Error('The test did not append output outside the viewport');
  }

  // OOB/status updates elsewhere on the page must not move the conversation.
  await page.evaluate(() => {
    document.getElementById('conn-status').dispatchEvent(new CustomEvent('htmx:afterSwap', {
      bubbles: true, detail: { elt: document.getElementById('conn-status') },
    }));
  });
  await settled(page);
  await assertPosition(page, readingPosition, 'an unrelated HTMX swap');

  await page.locator('#scroll-to-latest').click();
  await settled(page);
  await assertFollowing(page, 'jumping to latest during generation');
  await emit(page, 'token', paragraphs(105));
  await assertFollowing(page, 'streaming after jumping to latest');
  await page.locator('#messages .msg.assistant .bubble').last().evaluate((bubble) => {
    const lateContent = document.createElement('div');
    lateContent.style.height = '250px';
    bubble.append(lateContent);
  });
  await settled(page);
  await assertFollowing(page, 'late-loading content resizing the response');

  await scrollUp(page, mobile);
  await page.locator('#messages').evaluate((box) => { box.scrollTop = box.scrollHeight; });
  await settled(page);
  await assertFollowing(page, 'manually returning to the bottom');
  await emit(page, 'token', paragraphs(115));
  await assertFollowing(page, 'streaming after manual resume');

  await page.locator('#messages').focus();
  await page.keyboard.press('Home');
  await waitForScrollToSettle(page);
  const finishedPosition = await position(page);
  if (finishedPosition.following !== '0') throw new Error('Keyboard scrolling did not pause following');
  await page.evaluate(() => {
    const source = window.scrollTestSources.at(-1);
    source.readyState = 0;
    source.onerror(new Event('error'));
  });
  await assertIndicator(page, 'reconnecting');
  await assertPosition(page, finishedPosition, 'a disconnected stream');
  await page.evaluate(() => {
    const source = window.scrollTestSources.at(-1);
    source.readyState = 1;
    source.onopen(new Event('open'));
  });
  await assertIndicator(page, 'streaming');
  await emit(page, 'token', paragraphs(120));
  await emit(page, 'done', '');
  await assertIndicator(page, 'finished');
  await assertPosition(page, finishedPosition, 'completion while reading earlier output');

  await openSidebar(page);
  await page.locator('.btn-new').click();
  await page.waitForFunction(() => !document.querySelector('#messages [sse-connect]'));
  await settled(page);
  await assertFollowing(page, 'HTMX navigation to a new conversation');
  if (!(await page.locator('#scroll-status').isHidden())) {
    throw new Error('The previous conversation completion indicator leaked into a new chat');
  }
  const previousSources = await page.evaluate(() => window.scrollTestSources.length);
  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  await page.waitForFunction(count => window.scrollTestSources.length > count, previousSources);
  await emit(page, 'token', paragraphs(1));
  if (await page.locator('#messages').evaluate(box => box.scrollTop !== 0)) {
    throw new Error('The short-response fixture unexpectedly overflowed');
  }
  await page.locator('#messages').focus();
  await page.keyboard.press('Home');
  await settled(page);
  await assertIndicator(page, 'streaming');
  await emit(page, 'token', paragraphs(40));
  if (await page.locator('#messages').evaluate(box => box.scrollTop > 2 || box.dataset.following !== '0')) {
    throw new Error('Upward intent before overflow failed to preserve the first response lines');
  }
  await emit(page, 'done', '');
  for (const path of [testChat, new URL(page.url()).pathname]) {
    const match = path.match(/^\/chat\/(\d+)$/);
    if (!match || Object.values(index.chats).includes(Number(match[1]))) {
      throw new Error(`Refusing to remove a non-test conversation: ${path}`);
    }
    const response = await page.request.delete(`${base}/chats/${match[1]}`);
    if (!response.ok()) throw new Error(`Could not remove test conversation: ${response.status()}`);
  }
}

function paragraphs(count) {
  return Array.from({ length: count }, (_, i) =>
    `<p>Streaming paragraph ${i + 1}: this text remains readable while additional output arrives below it.</p>`,
  ).join('');
}

async function emit(page, type, data) {
  await page.evaluate(({ type, data }) => {
    window.scrollTestSources.at(-1).dispatchEvent(new MessageEvent(type, { data }));
  }, { type, data });
  await settled(page);
}

async function settled(page) {
  await page.evaluate(() => new Promise(resolve =>
    requestAnimationFrame(() => requestAnimationFrame(resolve)),
  ));
}

async function position(page) {
  return page.locator('#messages').evaluate((box) => {
    const rect = box.getBoundingClientRect();
    const visible = Array.from(box.querySelectorAll('.bubble p')).find((paragraph) => {
      const bounds = paragraph.getBoundingClientRect();
      return bounds.bottom > rect.top + 8 && bounds.top < rect.bottom;
    });
    return {
      top: box.scrollTop,
      height: box.scrollHeight,
      following: box.dataset.following,
      visibleText: visible?.textContent,
      visibleOffset: visible ? visible.getBoundingClientRect().top - rect.top : null,
    };
  });
}

async function assertPosition(page, before, action) {
  const after = await position(page);
  if (Math.abs(after.top - before.top) > 2 || after.visibleText !== before.visibleText ||
      Math.abs((after.visibleOffset ?? 0) - (before.visibleOffset ?? 0)) > 2) {
    throw new Error(`Reading position moved during ${action}: ${JSON.stringify({ before, after })}`);
  }
}

async function assertFollowing(page, action) {
  const result = await page.locator('#messages').evaluate(box => ({
    following: box.dataset.following,
    distance: box.scrollHeight - box.clientHeight - box.scrollTop,
  }));
  if (result.following !== '1' || result.distance > 2) {
    throw new Error(`Did not follow the latest output after ${action}: ${JSON.stringify(result)}`);
  }
}

async function assertIndicator(page, state) {
  await page.waitForFunction(expected => {
    const status = document.getElementById('scroll-status');
    return status && !status.hidden && status.dataset.state === expected;
  }, state);
  const correctText = await page.locator('#scroll-to-latest').evaluate((button, expected) =>
    button.querySelector('[role="status"]').textContent === button.dataset[expected], state);
  if (!correctText) throw new Error(`Missing localized ${state} scroll indicator`);
}

async function openSidebar(page) {
  const toggle = page.locator('#nav-toggle');
  if (await toggle.isVisible()) await toggle.click();
}

async function scrollUp(page, mobile) {
  const box = await page.locator('#messages').boundingBox();
  if (mobile) {
    const session = await page.context().newCDPSession(page);
    const x = Math.round(box.x + box.width / 2);
    const start = Math.round(box.y + box.height * 0.3);
    await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: start }] });
    for (let step = 1; step <= 5; step++) {
      await session.send('Input.dispatchTouchEvent', {
        type: 'touchMove', touchPoints: [{ x, y: Math.round(start + box.height * 0.08 * step) }],
      });
      await page.waitForTimeout(20);
    }
    await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await session.detach();
  } else {
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.wheel(0, -450);
  }
  await waitForScrollToSettle(page);
}

async function waitForScrollToSettle(page) {
  let previous = -1;
  for (let attempt = 0; attempt < 30; attempt++) {
    await page.waitForTimeout(100);
    const top = await page.locator('#messages').evaluate(element => element.scrollTop);
    if (Math.abs(top - previous) < 0.5) return;
    previous = top;
  }
  throw new Error('The reading gesture did not settle');
}
