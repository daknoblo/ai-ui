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
  const original = await first.locator('.bubble').innerHTML();
  const requestURL = await first.locator('.response-retry').getAttribute('hx-post');
  if (!requestURL?.startsWith(`/chat/${chatID}/retry/`)) throw new Error('Response retry is not bound to its own turn');
  let retries = 0;
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === requestURL) retries++;
  });
  page.once('dialog', dialog => dialog.dismiss());
  await first.locator('.response-retry').click();
  if (retries !== 0 || await page.locator('#messages .msg.assistant').count() !== 1) {
    throw new Error('Canceling retry confirmation still submitted a request');
  }
  page.once('dialog', dialog => dialog.accept());
  await first.locator('.response-retry').click();
  await page.waitForFunction(() => document.querySelectorAll('#messages .msg.assistant').length === 2);
  if (!(await first.locator('.response-retry').isDisabled())) {
    throw new Error('Retry button remained enabled while another answer was running');
  }
  await page.waitForFunction(() => Array.from(document.querySelectorAll('#messages [sse-connect]'))
    .every(message => message.dataset.streamState === 'finished'));
  const outcome = {
    retries, questions: await page.locator('#messages .msg.user').count(),
    originalPreserved: await first.locator('.bubble').innerHTML() === original,
    labeled: await page.locator('#messages .msg.assistant').last().locator('.retry-note').isVisible(),
  };
  if (outcome.retries !== 1 || outcome.questions !== 1 || !outcome.originalPreserved || !outcome.labeled) {
    throw new Error(`Retry replaced the old answer, duplicated the question or lost its label: ${JSON.stringify(outcome)}`);
  }
  await page.reload({ waitUntil: 'networkidle' });
  if (await page.locator('#messages .msg.assistant').count() !== 2 ||
      await first.locator('.response-retry').getAttribute('hx-post') !== requestURL ||
      await first.locator('.response-retry').isDisabled()) {
    throw new Error('Retry results or controls did not survive reload');
  }

  // Server errors must remain visible without losing existing outputs.
  await page.route(base + requestURL, route => route.fulfill({
    status: 503, contentType: 'text/plain', body: 'Temporary retry failure',
  }));
  page.once('dialog', dialog => dialog.accept());
  await first.locator('.response-retry').click();
  await first.locator('.response-retry-status').filter({ hasText: 'Temporary retry failure' }).waitFor();
  await page.waitForFunction(() => !document.querySelector('#messages .response-retry').disabled);
  if (await page.locator('#messages .msg.assistant').count() !== 2) {
    throw new Error('Failed retry submission inserted a response');
  }
  await page.unroute(base + requestURL);
  const removed = await page.request.delete(`${base}/chats/${chatID}`);
  if (!removed.ok()) throw new Error('Unable to remove the retry test chat');
}
