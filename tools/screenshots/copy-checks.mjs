// Verify clipboard formats without reading or replacing the user's clipboard.
export async function verifyResponseCopy(page, base, index) {
  await page.addInitScript(() => {
    window.copyWrites = [];
    window.copyMode = 'rich';
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
      async write(items) {
        if (window.copyMode !== 'rich') throw new DOMException('Test clipboard unavailable', 'NotAllowedError');
        const item = items[0];
        const html = await (await item.getType('text/html')).text();
        const text = await (await item.getType('text/plain')).text();
        window.copyWrites.push({ html, text, mode: 'rich' });
      },
      async writeText(text) {
        if (window.copyMode !== 'plain') throw new DOMException('Test clipboard denied', 'NotAllowedError');
        window.copyWrites.push({ text, mode: 'plain' });
      },
    } });
    const originalExec = document.execCommand.bind(document);
    document.execCommand = function (command, ...args) {
      if (command !== 'copy') return originalExec(command, ...args);
      if (window.copyMode !== 'legacy') return false;
      const data = new DataTransfer();
      const event = new ClipboardEvent('copy', { bubbles: true, cancelable: true, clipboardData: data });
      document.dispatchEvent(event);
      window.copyWrites.push({ html: data.getData('text/html'), text: data.getData('text/plain'), mode: 'legacy' });
      return event.defaultPrevented;
    };
  });
  await page.goto(`${base}/chat/${index.chats.chat}`, { waitUntil: 'networkidle' });
  const response = page.locator('#messages .msg.assistant').first();
  const copy = response.locator('.response-copy');
  await copy.waitFor({ state: 'visible' });
  const history = page.locator('#messages .msg');
  for (const message of await history.all()) {
    if (await message.locator('.response-copy').count() !== 1 || await message.locator('.response-retry').count() !== 1) {
      throw new Error('Every input and output in the history must have both controls');
    }
    await message.locator('.response-copy').scrollIntoViewIfNeeded();
    if (!(await message.locator('.response-copy').isVisible())) throw new Error('Earlier copy control is hidden');
  }
  const input = page.locator('#messages .msg.user').first();
  await input.locator('.bubble').evaluate(bubble => {
    bubble.innerHTML = '<p>USER-ONLY <strong>formatted input</strong> FINAL-INPUT</p>';
  });
  await input.locator('.response-copy').click();
  await assertStatus(input, 'copySuccess', 'success');
  const inputCopy = await page.evaluate(() => window.copyWrites.at(-1));
  if (inputCopy.text !== 'USER-ONLY formatted input FINAL-INPUT' || !inputCopy.html.includes('<strong>formatted input</strong>')) {
    throw new Error('Input copy did not copy only its own formatted content');
  }
  await page.evaluate(() => { window.copyWrites = []; });
  await response.locator('.bubble').evaluate(bubble => {
    bubble.innerHTML = '<h2>Exercise overview</h2><p><strong>Bold</strong> and <em>italic</em> text with <a href="/docs/example">a link</a>.</p>' +
      '<ol start="3"><li>First instruction</li><li>Second instruction</li></ol>' +
      '<table><thead><tr><th>Exercise</th><th>Repetitions</th></tr></thead><tbody><tr><td>Heel raise</td><td>12</td></tr></tbody></table>' +
      '<pre><code>  first line\n    indented line\nlast line</code></pre>' +
      '<p>' + 'Long complete response. '.repeat(120) + 'FINAL-MARKER</p>' +
      '<img alt="Illustration description" src="/images/999999">' +
      '<a href="javascript:alert(1)" data-secret="not-copied">Unsafe destination</a>';
  });
  await copy.click();
  await page.waitForFunction(() => window.copyWrites.length === 1);
  const rich = await page.evaluate(() => window.copyWrites[0]);
  for (const snippet of ['<h2>', '<strong>', '<em>', '<ol start="3">', '<table', '<th', '<pre', '<code', 'FINAL-MARKER']) {
    if (!rich.html.includes(snippet)) throw new Error(`Formatted clipboard lost ${snippet}`);
  }
  for (const snippet of ['3. First instruction', '4. Second instruction', 'Exercise\tRepetitions',
    'Heel raise\t12', '  first line\n    indented line\nlast line', 'FINAL-MARKER', 'Illustration description']) {
    if (!rich.text.includes(snippet)) throw new Error(`Plain clipboard lost ${snippet}`);
  }
  if (!rich.html.includes(`${base}/docs/example`) ||
      /javascript:|data-secret|response-copy|msg-model|<img/.test(rich.html)) {
    throw new Error('Clipboard contains unsafe URLs, image bytes or application UI');
  }
  await assertStatus(response, 'copySuccess', 'success');

  // Also exercise user-initiated copy-event support on HTTP-only installations.
  await page.evaluate(() => { window.copyMode = 'legacy'; });
  await copy.click();
  await page.waitForFunction(() => window.copyWrites.length === 2);
  const legacy = await page.evaluate(() => window.copyWrites[1]);
  if (legacy.mode !== 'legacy' || legacy.html !== rich.html || legacy.text !== rich.text) {
    throw new Error('Copy-event fallback did not preserve both formats');
  }
  await assertStatus(response, 'copySuccess', 'success');

  await page.evaluate(() => { window.copyMode = 'plain'; });
  await copy.click();
  await page.waitForFunction(() => window.copyWrites.length === 3);
  if (await page.evaluate(() => window.copyWrites[2].text) !== rich.text) {
    throw new Error('Plain-only fallback truncated the answer');
  }
  await assertStatus(response, 'copyPlain', 'success');

  await page.evaluate(() => { window.copyMode = 'denied'; });
  await copy.click();
  await assertStatus(response, 'copyFailed', 'error');
  if (await copy.isDisabled()) throw new Error('A denied clipboard left the control disabled');

  // Live replies must not expose a misleading "copy complete" action early.
  await page.goto(`${base}/`, { waitUntil: 'networkidle' });
  const testChat = new URL(page.url()).pathname;
  await page.evaluate(() => { window.copyMode = 'rich'; });
  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  const streamed = page.locator('#messages [sse-connect]').last();
  await streamed.waitFor();
  if (await streamed.locator('.response-copy').isVisible()) {
    throw new Error('Copy complete response was visible before streaming finished');
  }
  const liveInput = page.locator('#messages .msg.user').last();
  await liveInput.locator('.response-copy').click();
  await assertStatus(liveInput, 'copySuccess', 'success');
  if ((await page.evaluate(() => window.copyWrites.at(-1).text)) !== index.stream_prompt) {
    throw new Error('New inputs must be copyable while their answer is streaming');
  }
  await streamed.locator('.response-copy').waitFor({ state: 'visible', timeout: 30_000 });
  await streamed.locator('.response-copy').click();
  await assertStatus(streamed, 'copySuccess', 'success');
  const streamedText = await streamed.locator('.bubble').innerText();
  const lastCopy = await page.evaluate(() => window.copyWrites.at(-1));
  if (!lastCopy.text.includes(streamedText.trim().split('\n').at(-1))) {
    throw new Error('The last streamed lines are missing from the copied response');
  }
  await page.reload({ waitUntil: 'networkidle' });
  if (await page.locator('#messages .response-copy').count() !== await page.locator('#messages .msg').count()) {
    throw new Error('Persisted inputs or replies lost their copy controls on reload');
  }
  const match = testChat.match(/^\/chat\/(\d+)$/);
  if (!match || Object.values(index.chats).includes(Number(match[1]))) {
    throw new Error('Refusing to delete a non-test conversation');
  }
  const removed = await page.request.delete(`${base}/chats/${match[1]}`);
  if (!removed.ok()) throw new Error(`Could not clean up the clipboard test chat: ${removed.status()}`);
}

async function assertStatus(response, key, state) {
  const expected = await response.locator('.response-copy').evaluate((button, name) => button.dataset[name], key);
  await response.locator(`.response-copy-status[data-state="${state}"]`).filter({ hasText: expected }).waitFor();
  await response.locator('.response-copy-status').evaluate((element, args) => {
    const button = element.parentElement.querySelector('.response-copy');
    if (element.dataset.state !== args.state || element.textContent !== button.dataset[args.key]) {
      throw new Error(`Incorrect clipboard status: ${element.dataset.state} / ${element.textContent}`);
    }
  }, { key, state });
}
