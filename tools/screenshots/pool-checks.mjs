export async function saveSettings(page, expected = true) {
  const saved = await page.evaluate(() => window.AIUI_SETTINGS.flush());
  if (saved !== expected) throw new Error(`Autosave result ${saved}, expected ${expected}`);
}

export async function verifyDeploymentPools(page, base, index) {
  const open = async () => {
    await page.goto(`${base}/chat/${index.chats.chat}`, { waitUntil: 'networkidle' });
    // On phones the settings trigger belongs to the off-canvas navigation.
    await page.locator('.btn-config[hx-get="/config"]').evaluate(button => button.click());
    await page.locator('input[name="deployment_pools"]').waitFor({ state: 'attached' });
  };
  const submit = async (path, selector) => {
    await page.evaluate(() => {
      document.body.dataset.poolSettled = '0';
      const settled = event => {
        if (event.detail.target?.id !== 'modal-root') return;
        document.body.dataset.poolSettled = '1';
        document.body.removeEventListener('htmx:afterSettle', settled);
      };
      document.body.addEventListener('htmx:afterSettle', settled);
    });
    const response = page.waitForResponse(r => r.url() === base + path && r.request().method() === 'POST');
    await page.locator(selector).click();
    if (!(await response).ok()) throw new Error(`Pool request failed: ${path}`);
    await page.waitForFunction(() => document.body.dataset.poolSettled === '1');
  };
  await open();
  await submit('/config/deployments/refresh', '[hx-post="/config/deployments/refresh"]');
  const verify = page.waitForResponse(r => r.url() === base + '/verify' && r.request().method() === 'POST');
  await page.locator('button[hx-post="/verify"]').click();
  if (!(await verify).ok()) throw new Error('Connection check failed');
  await page.waitForFunction(() => !document.querySelector('#verify-results.htmx-request'));
  const timestamp = await page.locator('.check-timestamp').boundingBox();
  const heading = await page.locator('.readiness-heading').boundingBox();
  if (!timestamp || !heading || Math.abs(timestamp.x + timestamp.width - heading.x - heading.width) > 2) {
    throw new Error('Last check is not aligned at the right of the section heading');
  }
  const checkLayout = await page.locator('.check:not(.err)').evaluateAll(rows => rows.map(row => ({
    height: row.getBoundingClientRect().height,
    text: row.querySelector('summary').innerText,
  })));
  if (checkLayout.some(row => row.text.includes('/subscriptions/')) ||
      (page.viewportSize().width > 720 && checkLayout.some(row => row.height > 70))) {
    throw new Error('Default connection check rows are not compact');
  }
  const beforeDetails = await page.locator('.config-form').evaluate(form => new URLSearchParams(new FormData(form)).toString());
  if (await page.locator('#foundry-resource-id').isVisible() || !(await page.locator('.foundry-model-list').first().isVisible())) {
    throw new Error('Technical details should be collapsed while deployment cards are visible');
  }
  if (await page.locator('.config-form button[type="submit"]').count()) throw new Error('Save button remains');
  const languageY = (await page.locator('.settings-language').boundingBox()).y;
  if (languageY >= (await page.locator('#verify-results').boundingBox()).y) throw new Error('Language is not first');
  const catalogSummary = page.locator('.deployment-card-details > summary').first();
  await catalogSummary.focus();
  await page.keyboard.press('Enter');
  if (!(await page.locator('.deployment-card-details .foundry-model-meta').first().isVisible())) throw new Error('Card details are not keyboard accessible');
  await page.keyboard.press('Enter');
  await page.locator('#foundry-connections > summary').click();
  if (!(await page.locator('#foundry-resource-id').isVisible())) throw new Error('Full resource identifier is inaccessible');
  await page.locator('#foundry-connections > summary').click();
  const afterDetails = await page.locator('.config-form').evaluate(form => new URLSearchParams(new FormData(form)).toString());
  if (beforeDetails !== afterDetails) throw new Error('Expanding details changed staged checkbox selections');
  const visibleLabels = await page.locator('.deployment-toggles label').allInnerTexts();
  if (visibleLabels.some(label => label.includes('/subscriptions/'))) throw new Error('Checkbox labels expose verbose resource IDs');
  const info = page.locator('.check.info .check-body').first();
  if (await info.count()) {
    if (await info.getAttribute('open') !== null) throw new Error('Informational checks should be compact by default');
    await info.locator('summary').click();
    if (!(await info.locator('.check-detail').first().isVisible())) throw new Error('Probe limitations are inaccessible');
    await info.locator('summary').click();
  }
  const text = await page.locator('.config-form').innerText();
  if (!text.includes('polandcentral') || !text.includes('swedencentral')) {
    throw new Error('Multi-resource inventory must show Sweden and Poland');
  }
  const names = ['chat', 'vision', 'images', 'image_edits', 'embeddings'];
  const selected = {};
  for (const op of names) {
    const inputs = page.locator(`input[name="enabled_${op}"]`);
    selected[op] = await inputs.evaluateAll(items => items.filter(item => item.checked).map(item => item.value));
    if (!selected[op].length) throw new Error(`Legacy ${op} selection did not migrate`);
    const original = selected[op].find(id => !id.includes('-poland/deployments/'));
    if (!original) throw new Error(`Original ${op} resource is missing`);
    const replica = original.replace('/deployments/', '-poland/deployments/');
    const match = inputs.filter({ visible: true });
    if (!(await match.count())) throw new Error(`No accessible ${op} checkboxes`);
    await page.locator(`input[name="enabled_${op}"][value="${replica}"]`).check();
    if (!selected[op].includes(replica)) selected[op].push(replica);
  }
  await saveSettings(page);
  if (await page.locator('.config-notice-err').count()) throw new Error('Compatible replicas were rejected');
  await open();
  for (const op of names) {
    const checked = await page.locator(`input[name="enabled_${op}"]:checked`).evaluateAll(items => items.map(item => item.value));
    if (checked.length !== selected[op].length || selected[op].some(id => !checked.includes(id))) {
      throw new Error(`Enabled ${op} replicas were not persisted`);
    }
  }
  const modelChange = page.locator('input[name="enabled_embeddings"][value$="/accounts/local-demo/deployments/docs-next"]');
  await modelChange.check();
  await saveSettings(page, false);
  await page.locator('.settings-save-toast[data-state="error"]').waitFor();
  if (!(await modelChange.isChecked())) throw new Error('Rejected edit was silently discarded');
  await page.locator('[data-settings-discard]').click();
  await open();
  if (await page.locator('input[name="enabled_embeddings"][value$="/docs-next"]:checked').count()) {
    throw new Error('Incompatible vector-space selection was saved');
  }
  for (const op of names) {
    for (const input of await page.locator(`input[name="enabled_${op}"]`).all()) await input.uncheck();
    await saveSettings(page);
    if (await page.locator('.config-notice-err').count()) throw new Error(`Disabling ${op} was rejected`);
    await submit('/config/deployments/refresh', '[hx-post="/config/deployments/refresh"]');
    await open();
    if (await page.locator(`input[name="enabled_${op}"]:checked`).count()) {
      throw new Error(`Refresh or reload silently repopulated an explicitly empty ${op} pool`);
    }
    for (const other of names.filter(name => name !== op)) {
      const checked = await page.locator(`input[name="enabled_${other}"]:checked`).evaluateAll(items => items.map(item => item.value));
      if (checked.length !== selected[other].length || selected[other].some(id => !checked.includes(id))) {
        throw new Error(`Disabling ${op} changed the independent ${other} pool`);
      }
    }
    for (const id of selected[op]) await page.locator(`input[name="enabled_${op}"][value="${id}"]`).check();
    await saveSettings(page);
    if (await page.locator('.config-notice-err').count()) throw new Error(`Restoring ${op} was rejected`);
  }
  const overflow = await page.locator('.modal').evaluate(element => element.scrollWidth > element.clientWidth + 2);
  if (overflow) throw new Error('Deployment pool controls overflow on this viewport');
  const prompt = page.locator('[name="system_prompt"]');
  const originalPrompt = await prompt.inputValue();
  let active = 0, maxActive = 0, posts = 0;
  await page.route(`${base}/config`, async route => {
    if (route.request().method() !== 'POST') return route.continue();
    active++;
    maxActive = Math.max(maxActive, active);
    posts++;
    if (posts === 1) await new Promise(resolve => setTimeout(resolve, 400));
    const response = await route.fetch();
    active--;
    await route.fulfill({ response });
  });
  const firstSave = page.waitForRequest(r => r.url() === base + '/config' && r.method() === 'POST');
  await prompt.fill('First autosave');
  await page.evaluate(() => { window.AIUI_SETTINGS.flush(); });
  await firstSave;
  await prompt.fill('Latest autosave');
  await saveSettings(page);
  if (maxActive !== 1 || posts !== 2) throw new Error('Autosaves were not serialized and coalesced');
  await page.unroute(`${base}/config`);
  await open();
  if (await prompt.inputValue() !== 'Latest autosave') throw new Error('Later edits were lost while saving');
  await page.route(`${base}/config`, route => route.request().method() === 'POST'
    ? route.fulfill({ status: 503, contentType: 'text/plain', body: 'Unavailable' }) : route.continue());
  await prompt.fill('Pending after failure');
  await saveSettings(page, false);
  if (!(await page.locator('.settings-save-toast[data-state="error"]').isVisible())) throw new Error('Save failure has no red overlay');
  await page.unroute(`${base}/config`);
  await prompt.fill(originalPrompt);
  await saveSettings(page);
  if (!(await page.locator('.settings-save-toast[data-state="saved"]').isVisible())) throw new Error('Save success has no green overlay');
  await prompt.fill('Saved while closing');
  await page.locator('.modal-close[data-settings-close]').click();
  await page.locator('.settings-overlay').waitFor({ state: 'detached' });
  await open();
  if (await prompt.inputValue() !== 'Saved while closing') throw new Error('Closing discarded a pending save');
  await prompt.fill(originalPrompt);
  await saveSettings(page);
  const originalLanguage = await page.locator('[name="language"]').inputValue();
  for (const language of [originalLanguage === 'en' ? 'de' : 'en', originalLanguage]) {
    const loaded = page.waitForEvent('load');
    await page.locator('[name="language"]').selectOption(language);
    await loaded;
    await page.locator('.settings-overlay').waitFor();
    if (await page.locator('[name="language"]').inputValue() !== language ||
        await page.locator('html').getAttribute('lang') !== language) {
      throw new Error('Language autosave did not reload and reopen Settings in the new language');
    }
  }
}
