export async function enableFluxModels(page, base, index) {
  await page.goto(`${base}/chat/${index.chats.chat}`, { waitUntil: 'networkidle' });
  await page.locator('.btn-config[hx-get="/config"]').evaluate(button => button.click());
  await page.locator('.config-form').waitFor();
  const original = await page.locator('.config-form').evaluate(form => new URLSearchParams(new FormData(form)).toString());
  for (const op of ['images', 'image_edits']) {
    const replicas = page.locator(`input[name="enabled_${op}"][value$="/flux-pro"], input[name="enabled_${op}"][value$="/flux-flex"]`);
    if (await replicas.count() < 2) throw new Error('FLUX models missing from demo inventory');
    for (const replica of await replicas.all()) await replica.check();
  }
  const form = await page.locator('.config-form').evaluate(form => new URLSearchParams(new FormData(form)).toString());
  const saved = await page.request.post(`${base}/config`, { data: form, headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
  if (!saved.ok() || (await saved.text()).includes('config-notice-err')) throw new Error('Could not activate FLUX replicas');
  return async () => {
    const restored = await page.request.post(`${base}/config`, { data: original, headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
    if (!restored.ok()) throw new Error('Could not restore image pools');
  };
}

export async function selectImageModel(page, base, model) {
  const saved = page.waitForResponse(response => response.url().endsWith('/image-model') && response.request().method() === 'POST');
  await page.locator('#image-model-select').selectOption(model);
  if (!(await saved).ok()) throw new Error(`Could not save image model ${model}`);
}

async function verifyImageControlLayout(page, singleRow = false) {
  const layout = await page.locator('.composer-tools').evaluate((toolbar, singleRow) => {
    const picker = toolbar.querySelector('#image-model-form');
    const attach = toolbar.querySelector('.attach-opt');
    const reasoning = toolbar.querySelector('#reasoning-opt');
    const params = toolbar.querySelector('#image-params');
    const visible = node => node && node.getClientRects().length > 0;
    const center = node => {
      const box = node.getBoundingClientRect();
      return box.y + box.height / 2;
    };
    const controls = [...toolbar.querySelectorAll('select, input[type="number"]')].filter(visible);
    const boxes = controls.map(node => node.getBoundingClientRect());
    return {
      hasPicker: !!picker,
      hasHelp: !!document.querySelector('#image-model-help'),
      aligned: window.innerWidth <= 720 || [attach, reasoning].filter(visible).every(node => Math.abs(center(node) - center(picker)) < 2),
      matchingStyle: getComputedStyle(picker).fontSize === getComputedStyle(attach).fontSize,
      inlineParams: getComputedStyle(params).display === (params.hidden ? 'none' : 'contents'),
      singleRow: !singleRow || window.innerWidth <= 720 ||
        boxes.every(box => Math.abs(box.y + box.height / 2 - center(picker)) < 2),
      separatedGroups: [...toolbar.querySelectorAll('.image-param-group')].filter(visible).every(node =>
        getComputedStyle(node).borderInlineStartStyle === 'solid'),
      overlapping: boxes.some((box, i) => boxes.slice(i + 1).some(other =>
        box.left < other.right && box.right > other.left && box.top < other.bottom && box.bottom > other.top)),
      overflowing: toolbar.scrollWidth > toolbar.clientWidth + 2 || boxes.some(box => box.left < 0 || box.right > window.innerWidth),
    };
  }, singleRow);
  if (!layout.hasPicker || layout.hasHelp || !layout.aligned || !layout.matchingStyle ||
      !layout.inlineParams || !layout.singleRow || !layout.separatedGroups || layout.overlapping || layout.overflowing) {
    throw new Error(`Image control layout regression: ${JSON.stringify(layout)}`);
  }
}

async function inputStyle(page) {
  return page.locator('#chat-form textarea').evaluate(input => {
    input.blur();
    const style = getComputedStyle(input);
    return JSON.stringify([style.borderColor, style.borderWidth, style.boxShadow, style.backgroundColor]);
  });
}

async function verifyChatTabTitle(page) {
  if (await page.title() !== `AI-UI – ${await page.locator('#chat-title').innerText()}`) {
    throw new Error('Browser tab did not retain the application name and current chat title');
  }
}

export async function verifyFluxModels(page, base, index, language) {
  const restore = await enableFluxModels(page, base, index);
  let id;
  try {
    await page.goto(`${base}/`, { waitUntil: 'networkidle' });
    id = new URL(page.url()).pathname.split('/').at(-1);
    if (Object.values(index.chats).includes(Number(id))) throw new Error('Expected disposable FLUX chat');
    await verifyChatTabTitle(page);
    const picker = page.locator('#image-model-select');
    const options = await picker.locator('option').evaluateAll(items => items.map(item => item.value));
    if (!['', 'flux.2-pro', 'flux.2-flex', 'gpt-image-2'].every(model => options.includes(model)) ||
        options.length !== new Set(options).size) throw new Error('Image picker did not collapse replicas');
    const automatic = await picker.locator('option[value=""]').innerText();
    if (automatic !== (language === 'de' ? 'Automatisch' : 'Automatic')) throw new Error('Automatic option is not localized');
    const pickerBox = await picker.boundingBox();
    const inputBox = await page.locator('#chat-form textarea').boundingBox();
    if (!pickerBox || !inputBox || pickerBox.y + pickerBox.height > inputBox.y) throw new Error('Image picker is not above the input');
    await verifyImageControlLayout(page);
    await selectImageModel(page, base, 'flux.2-flex');
    if (await page.locator('.composer').getAttribute('data-mode') !== 'chat') throw new Error('Image selection changed chat mode');
    await page.reload({ waitUntil: 'networkidle' });
    if (await picker.inputValue() !== 'flux.2-flex') throw new Error('Image selection was not persisted per chat');
    const chatInputStyle = await inputStyle(page);
    const mode = page.waitForResponse(r => r.url().endsWith('/mode') && r.request().method() === 'POST');
    await page.locator('[data-mode="image"].mode-opt').click();
    if (!(await mode).ok()) throw new Error('Image mode could not be saved');
    if (await inputStyle(page) !== chatInputStyle) throw new Error('Image mode changed the input frame or background');
    if (!(await page.locator('[data-image-setting="flex"]').first().isVisible()) ||
        await page.locator('[data-image-setting="gpt"]').first().isVisible()) throw new Error('Flex showed incorrect parameters');
    await verifyImageControlLayout(page);
    await selectImageModel(page, base, 'flux.2-pro');
    if (await page.locator('[data-image-setting="flex"]').first().isVisible()) throw new Error('Pro exposed Flex-only settings');
    await verifyImageControlLayout(page, true);
    await selectImageModel(page, base, 'gpt-image-2');
    if (!(await page.locator('[data-image-setting="gpt"]').first().isVisible()) ||
        await page.locator('[data-image-setting="flux"]').isVisible()) throw new Error('GPT parameters were not preserved');
    await verifyImageControlLayout(page);
    await selectImageModel(page, base, '');
    if (!(await page.locator('[data-image-setting="flex"]').first().isVisible()) ||
        !(await page.locator('[data-image-setting="gpt"]').first().isVisible())) throw new Error('Automatic lost model-family options');
    await verifyImageControlLayout(page);
    await selectImageModel(page, base, 'flux.2-flex');
    const params = page.waitForResponse(r => r.url().endsWith('/image/params') && r.request().method() === 'POST');
    await page.locator('[name="flux_size"]').selectOption('2048x2048');
    if (!(await params).ok()) throw new Error('FLUX resolution was not saved');
    for (let step = 0; step < 2; step++) {
      await page.locator('#chat-form textarea').fill(step ? 'Refine the blue tree.' : 'Draw a blue tree.');
      const sent = page.waitForRequest(request => request.url().endsWith('/send') && request.method() === 'POST');
      await page.locator('#chat-form button[type="submit"]').click();
      const form = new URLSearchParams((await sent).postData());
      if (form.get('image_model') !== 'flux.2-flex' || form.get('flux_size') !== '2048x2048' ||
          form.get('edit') !== (step ? '1' : '0')) throw new Error('Image request lost the current model, parameters or latest-image edit');
      await page.waitForFunction(count => document.querySelectorAll('#messages .bubble img').length === count &&
        [...document.querySelectorAll('#messages [sse-connect]')].every(node => node.dataset.streamState === 'finished'), step + 1);
      await verifyChatTabTitle(page);
    }
    const urls = await page.locator('#messages .bubble img').evaluateAll(images => images.map(image => image.getAttribute('src')));
    const bytes = await Promise.all(urls.map(async url => (await page.request.get(base + url)).body()));
    await page.reload({ waitUntil: 'networkidle' });
    await verifyChatTabTitle(page);
    if (await inputStyle(page) !== chatInputStyle) throw new Error('Reloaded image mode changed input styling');
    for (let i = 0; i < urls.length; i++) {
      if (!(await (await page.request.get(base + urls[i])).body()).equals(bytes[i])) throw new Error('Reload changed durable image bytes');
    }
    if (await page.locator('.chat-main').evaluate(node => node.scrollWidth > node.clientWidth + 2)) {
      throw new Error('Image controls overflow the viewport');
    }
    const invalid = await page.request.post(`${base}/image/params`, { form: { flux_steps: '51' } });
    if (invalid.status() !== 400) throw new Error('Server accepted invalid FLUX steps');
    await restore();
    const stale = await page.request.post(`${base}/chat/${id}/send`, { form: { message: 'Do not send', mode: 'image' } });
    if (stale.status() !== 400) throw new Error('Unavailable saved image model silently fell back');
  } finally {
    await restore();
    if (id) await page.request.delete(`${base}/chats/${id}`);
  }
}
