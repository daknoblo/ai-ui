export async function verifyResponseRetry(page, base, index) {
  await page.goto(`${base}/`, { waitUntil: 'networkidle' });
  const path = new URL(page.url()).pathname;
  const match = path.match(/^\/chat\/(\d+)$/);
  if (!match || Object.values(index.chats).includes(Number(match[1]))) throw new Error('Expected a disposable retry test chat');
  const chatID = match[1];
  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  await page.waitForFunction(() => document.querySelector('#messages [sse-connect]')?.dataset.streamState === 'finished');
  await page.waitForFunction(() => !document.querySelector('#messages .htmx-settling, #messages .htmx-added'));
  const first = page.locator('#messages .msg.assistant').first();
  await verifyFooter(first);
  await verifyFooterEdgeCases(first);
  const original = await first.locator('.bubble').innerHTML();
  const requestURL = await first.locator('.response-retry').getAttribute('hx-post');
  if (!requestURL?.startsWith(`/chat/${chatID}/retry/`)) throw new Error('Response retry is not bound to its own turn');
  const firstInput = page.locator('#messages .msg.user').first();
  if (await firstInput.locator('.response-retry').getAttribute('hx-post') !== requestURL) {
    throw new Error('Input and corresponding answer must retry the same question');
  }
  await page.locator('#chat-form textarea').fill('Later question: ' + index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  await page.waitForFunction(() => document.querySelectorAll('#messages .msg.assistant').length === 2 &&
    Array.from(document.querySelectorAll('#messages [sse-connect]')).every(message => message.dataset.streamState === 'finished'));
  await page.locator('#chat-form textarea').fill('UNSENT-DRAFT');
  let retries = 0;
  let submittedOptions;
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === requestURL) {
      retries++;
      submittedOptions = new URLSearchParams(request.postData());
    }
  });
  page.once('dialog', dialog => dialog.dismiss());
  await first.locator('.response-retry').click();
  if (retries !== 0 || await page.locator('#messages .msg.assistant').count() !== 2) {
    throw new Error('Canceling retry confirmation still submitted a request');
  }
  page.once('dialog', dialog => dialog.accept());
  await first.locator('.response-retry').click();
  await page.waitForFunction(() => document.querySelectorAll('#messages .msg.assistant').length === 3);
  if (await page.locator('#messages .response-retry:not(:disabled)').count()) {
    throw new Error('A history Retry button remained enabled while another answer was running');
  }
  await page.waitForFunction(() => Array.from(document.querySelectorAll('#messages [sse-connect]'))
    .every(message => message.dataset.streamState === 'finished'));
  const outcome = {
    retries, questions: await page.locator('#messages .msg.user').count(),
    originalPreserved: await first.locator('.bubble').innerHTML() === original,
    newInput: await page.locator('#messages .msg.user .bubble').last().innerText(),
    draft: await page.locator('#chat-form textarea').inputValue(),
  };
  if (outcome.retries !== 1 || outcome.questions !== 3 || !outcome.originalPreserved ||
      outcome.newInput.trim() !== index.stream_prompt || outcome.draft !== 'UNSENT-DRAFT' ||
      submittedOptions.get('mode') !== 'chat' || submittedOptions.get('web') !== '0' ||
      submittedOptions.get('edit') !== '0' || submittedOptions.has('message')) {
    throw new Error(`Retry did not submit a fresh question with current options: ${JSON.stringify(outcome)}`);
  }
  page.once('dialog', dialog => dialog.accept());
  await firstInput.locator('.response-retry').click();
  await page.waitForFunction(() => document.querySelectorAll('#messages .msg.assistant').length === 4 &&
    Array.from(document.querySelectorAll('#messages [sse-connect]')).every(message => message.dataset.streamState === 'finished'));
  if ((await page.locator('#messages .msg.user .bubble').last().innerText()).trim() !== index.stream_prompt) {
    throw new Error('Retry from an earlier input used the wrong question');
  }
  await page.reload({ waitUntil: 'networkidle' });
  if (await page.locator('#messages .msg.assistant').count() !== 4 ||
      await page.locator('#messages .msg.user').count() !== 4 ||
      await first.locator('.response-retry').getAttribute('hx-post') !== requestURL ||
      await first.locator('.response-retry').isDisabled()) {
    throw new Error('Retry results or controls did not survive reload');
  }
  for (const message of await page.locator('#messages .msg').all()) {
    const retry = message.locator('.response-retry');
    if (await retry.count() !== 1 || await message.locator('.response-copy').count() !== 1) {
      throw new Error('Copy and Retry must exist on every historical input and output');
    }
    await retry.scrollIntoViewIfNeeded();
    if (!(await retry.isVisible()) || await retry.isDisabled()) throw new Error('Earlier Retry controls must remain usable');
  }
  const lastQuestionURL = await page.locator('#messages .msg.user .response-retry').last().getAttribute('hx-post');
  if (await page.locator('#messages .msg.assistant .response-retry').last().getAttribute('hx-post') !== lastQuestionURL) {
    throw new Error('A repeated answer lost its corresponding new user message after reload');
  }
  await verifyFooter(first);

  // Server errors must remain visible without losing existing outputs.
  await page.route(base + requestURL, route => route.fulfill({
    status: 503, contentType: 'text/plain', body: 'Temporary retry failure',
  }));
  page.once('dialog', dialog => dialog.accept());
  await first.locator('.response-retry').click();
  await first.locator('.response-retry-status').filter({ hasText: 'Temporary retry failure' }).waitFor();
  await page.waitForFunction(() => !document.querySelector('#messages .response-retry').disabled);
  await verifyFooter(first);
  if (await page.locator('#messages .msg.assistant').count() !== 4 ||
      await page.locator('#messages .msg.user').count() !== 4) {
    throw new Error('Failed retry submission inserted a response');
  }

  async function verifyFooter(response) {
    await page.mouse.move(0, 0);
    const { copy, retry } = await response.evaluate(message => ({
      copy: message.querySelector('.response-copy').getBoundingClientRect().toJSON(),
      retry: message.querySelector('.response-retry').getBoundingClientRect().toJSON(),
    }));
    if (!copy || !retry || retry.x < copy.x + copy.width || retry.x - copy.x - copy.width > 12 ||
        Math.abs(retry.y - copy.y) > 2 || !(await response.locator('.response-retry-label').innerText()).trim()) {
      throw new Error(`Retry must be visibly labeled directly beside Copy: ${JSON.stringify({ copy, retry })}`);
    }
    if (!(await response.locator('.response-copy-label').innerText()).trim()) {
      throw new Error('Copy must have a visible label matching the Retry button style');
    }
    const equalActions = await response.locator('.response-actions').evaluate(actions => {
      const copy = getComputedStyle(actions.querySelector('.response-copy'));
      const retry = getComputedStyle(actions.querySelector('.response-retry'));
      const properties = ['height', 'minWidth', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft',
        'gap', 'fontFamily', 'fontSize', 'fontWeight', 'borderTopWidth', 'borderTopStyle', 'borderTopColor',
        'borderRadius', 'backgroundColor', 'color'];
      return properties.every(key => copy[key] === retry[key]);
    });
    if (!equalActions) throw new Error('Copy and Retry have inconsistent button styles');
    if (await response.locator('.msg-role .msg-model').count()) {
      throw new Error('Model metadata must not remain beside the Assistant heading');
    }
    await response.locator('.msg-metadata .msg-usage').waitFor({ state: 'visible' });
    await response.locator('.msg-metadata .model-badge').waitFor({ state: 'visible' });
    const equalTypography = await response.locator('.msg-footer').evaluate(footer => {
      const usage = getComputedStyle(footer.querySelector('.msg-usage'));
      const model = getComputedStyle(footer.querySelector('.model-badge'));
      const actions = [...footer.querySelectorAll('.response-copy, .response-retry')].map(button => getComputedStyle(button));
      return ['fontFamily', 'fontSize', 'fontWeight', 'lineHeight', 'color']
        .every(key => [model, ...actions].every(style => usage[key] === style[key])) &&
        [model, ...actions].every(style => style.borderTopWidth === '0px');
    });
    if (!equalTypography) throw new Error('Footer metadata and actions must use the same borderless typography');
    await verifyFooterLayout(response);
  }

  async function verifyFooterLayout(response) {
    const layout = await response.locator('.msg-footer').evaluate(footer => {
      const metadata = footer.querySelector('.msg-metadata');
      const actions = footer.querySelector('.response-actions');
      const copy = footer.querySelector('.response-copy');
      const retry = footer.querySelector('.response-retry');
      const box = footer.getBoundingClientRect();
      const details = metadata.getBoundingClientRect();
      const left = copy.getBoundingClientRect();
      const right = retry.getBoundingClientRect();
      const visible = getComputedStyle(metadata).display !== 'none';
      const fits = visible && details.width + actions.getBoundingClientRect().width + 12 <= box.width + 1;
      const usage = metadata.querySelector('.msg-usage').getBoundingClientRect();
      const label = copy.querySelector('.response-copy-label').getBoundingClientRect();
      const sameRow = !fits || (Math.abs(details.y - left.y) <= 1 &&
        Math.abs(usage.y + usage.height / 2 - (label.y + label.height / 2)) <= 1);
      return {
        aligned: Math.abs(right.right - box.right) <= 1 && (!visible || Math.abs(details.left - box.left) <= 1),
        sameRow,
        contained: footer.scrollWidth <= footer.clientWidth && left.left >= box.left &&
          (!visible || right.y >= details.bottom - 1 || left.left >= details.right),
      };
    });
    if (!layout.aligned || !layout.sameRow || !layout.contained) {
      throw new Error(`Footer must align metadata left and actions right without overlap: ${JSON.stringify(layout)}`);
    }
  }

  async function verifyFooterEdgeCases(response) {
    const original = await response.locator('.msg-metadata').innerHTML();
    const viewport = page.viewportSize();
    await page.setViewportSize({ width: 320, height: viewport.height });
    await verifyFooter(response);
    await response.locator('.msg-metadata').evaluate(metadata => {
      metadata.querySelector('.msg-model').textContent = 'long-deployment-name-'.repeat(20);
    });
    // Long names may wrap; only test containment and right alignment here.
    const longName = await response.locator('.msg-footer').evaluate(footer => {
      const box = footer.getBoundingClientRect();
      const retry = footer.querySelector('.response-retry').getBoundingClientRect();
      return footer.scrollWidth <= footer.clientWidth && Math.abs(retry.right - box.right) <= 1;
    });
    if (!longName) throw new Error('Long model names overflowed the response footer');
    await response.locator('.msg-metadata').evaluate(metadata => {
      metadata.querySelector('.msg-model').textContent = '';
      metadata.querySelector('.msg-usage').textContent = '';
    });
    await verifyFooterLayout(response);
    await response.locator('.response-copy-status').evaluate(status => {
      status.textContent = 'Long clipboard notice '.repeat(15);
    });
    await verifyFooterLayout(response);
    await response.locator('.response-copy-status').evaluate(status => { status.textContent = ''; });
    await response.locator('.msg-metadata').evaluate((metadata, html) => { metadata.innerHTML = html; }, original);
    await page.setViewportSize(viewport);
  }
  await page.unroute(base + requestURL);
  const removed = await page.request.delete(`${base}/chats/${chatID}`);
  if (!removed.ok()) throw new Error('Unable to remove the retry test chat');
}
