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
  await submit('/config', '.config-form button[type="submit"]');
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
  await submit('/config', '.config-form button[type="submit"]');
  await page.locator('.config-notice-err').first().waitFor();
  await open();
  if (await page.locator('input[name="enabled_embeddings"][value$="/docs-next"]:checked').count()) {
    throw new Error('Incompatible vector-space selection was saved');
  }
  for (const op of names) {
    for (const input of await page.locator(`input[name="enabled_${op}"]`).all()) await input.uncheck();
    await submit('/config', '.config-form button[type="submit"]');
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
    await submit('/config', '.config-form button[type="submit"]');
    if (await page.locator('.config-notice-err').count()) throw new Error(`Restoring ${op} was rejected`);
  }
  const overflow = await page.locator('.modal').evaluate(element => element.scrollWidth > element.clientWidth + 2);
  if (overflow) throw new Error('Deployment pool controls overflow on this viewport');
}
