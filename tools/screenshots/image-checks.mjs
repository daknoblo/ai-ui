export async function verifyImageRefinements(page, base, index) {
  await page.goto(`${base}/`, { waitUntil: 'networkidle' });
  const match = new URL(page.url()).pathname.match(/^\/chat\/(\d+)$/);
  if (!match || Object.values(index.chats).includes(Number(match[1]))) {
    throw new Error('Expected a disposable image refinement chat');
  }
  const chatID = match[1];
  const modeSaved = page.waitForResponse(response =>
    response.url() === `${base}/chat/${chatID}/mode` && response.request().method() === 'POST');
  await page.locator('.mode-opt[data-mode="image"]').click();
  if (!(await modeSaved).ok()) throw new Error('Could not enable image mode');
  const images = [];
  for (let step = 0; step < 4; step++) {
    const expectedEdit = step === 0 ? '0' : '1';
    if (await page.locator('#edit-flag').inputValue() !== expectedEdit) {
      throw new Error(`Image step ${step + 1} did not select the expected generation/edit mode`);
    }
    await page.locator('#chat-form textarea').fill(step === 0
      ? 'Generate a blue park illustration.'
      : `Refine the current illustration, step ${step + 1}.`);
    const sent = page.waitForRequest(request =>
      request.url() === `${base}/chat/${chatID}/send` && request.method() === 'POST');
    await page.locator('#chat-form button[type="submit"]').click();
    const form = new URLSearchParams((await sent).postData());
    if (form.get('mode') !== 'image' || form.get('edit') !== expectedEdit) {
      throw new Error(`Image step ${step + 1} submitted incorrect composer flags`);
    }
    await page.waitForFunction(count =>
      document.querySelectorAll('#messages .msg.assistant').length === count &&
      [...document.querySelectorAll('#messages [sse-connect]')].every(message => message.dataset.streamState === 'finished'),
    step + 1);
    await page.waitForFunction(count => {
      const pictures = [...document.querySelectorAll('#messages .bubble img')];
      return pictures.length === count && pictures.every(image => image.complete && image.naturalWidth > 0);
    }, step + 1);
    const current = await page.locator('#messages .bubble img').evaluateAll(pictures => pictures.map(image => image.getAttribute('src')));
    if (current.some((src, i) => i < images.length && src !== images[i]) ||
        !/^\/images\/\d+$/.test(current.at(-1)) || images.includes(current.at(-1))) {
      throw new Error('Image refinement replaced an earlier result or reused its URL');
    }
    images.push(current.at(-1));
    if (await page.locator('#edit-flag').inputValue() !== '1' || !(await page.locator('#edit-check').isChecked())) {
      throw new Error('A successful generation did not enable editing for the next message');
    }
    if (step === 1) {
      await page.reload({ waitUntil: 'networkidle' });
      if (await page.locator('.composer').getAttribute('data-mode') !== 'image' ||
          await page.locator('#messages .bubble img').count() !== images.length) {
        throw new Error('Reload lost generated images or the current image mode');
      }
    }
  }
  const removed = await page.request.delete(`${base}/chats/${chatID}`);
  if (!removed.ok()) throw new Error('Could not remove the image refinement test chat');
}
