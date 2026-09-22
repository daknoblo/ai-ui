export async function verifySidebarResize(page, base, index, mobile) {
  await page.goto(`${base}/chat/${index.chats.chat}`, { waitUntil: 'networkidle' });
  const sidebar = page.locator('#sidebar');
  const handle = page.locator('#sidebar-resize');
  if (mobile) {
    await page.evaluate(() => localStorage.setItem('ai-ui-sidebar-width', '500'));
    await page.reload({ waitUntil: 'networkidle' });
    await page.locator('#nav-toggle').click();
    if (await handle.isVisible() || (await sidebar.boundingBox()).width > 280) {
      throw new Error('Desktop resize preference changed the mobile drawer');
    }
    return;
  }
  async function assertWidth(expected) {
    await page.waitForFunction(value => {
      const sidebar = document.getElementById('sidebar');
      const handle = document.getElementById('sidebar-resize');
      return Math.abs(sidebar.getBoundingClientRect().width - value) <= 1 &&
        Math.abs(Number(handle.getAttribute('aria-valuenow')) - value) <= 1;
    }, expected);
    const width = (await sidebar.boundingBox()).width;
    const value = Number(await handle.getAttribute('aria-valuenow'));
    if (Math.abs(width - expected) > 1 || Math.abs(value - expected) > 1) {
      throw new Error(`Sidebar width/ARIA value = ${width}/${value}, expected ${expected}`);
    }
    if (await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)) {
      throw new Error('Sidebar resizing overflowed the viewport');
    }
  }
  async function dragBy(delta, cancel = false) {
    const box = await handle.boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + 200);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + delta, box.y + 230, { steps: 8 });
    if (cancel) await page.keyboard.press('Escape');
    await page.mouse.up();
    if (await page.locator('body.sidebar-resizing').count()) throw new Error('Resize drag remained active');
  }
  await assertWidth(270);
  const content = await page.locator('#messages').innerHTML();
  await dragBy(150);
  await assertWidth(420);
  if (await page.locator('#messages').innerHTML() !== content) throw new Error('Resizing replaced the conversation');
  await page.reload({ waitUntil: 'networkidle' });
  await assertWidth(420);
  await dragBy(-100);
  await assertWidth(320);
  await dragBy(100, true);
  await assertWidth(320);
  await page.locator('.new-chat-form .btn-new').click();
  await page.waitForFunction(() => !!document.querySelector('#chat-form') && !document.querySelector('#messages .msg'));
  await assertWidth(320);
  await handle.focus();
  await page.keyboard.press('ArrowRight');
  await assertWidth(330);
  await page.keyboard.press('Home');
  await assertWidth(240);
  await dragBy(-150);
  await assertWidth(240);
  await handle.focus();
  await page.keyboard.press('End');
  await assertWidth(560);
  await dragBy(350);
  await assertWidth(560);
  await page.setViewportSize({ width: 800, height: 900 });
  await assertWidth(440);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('#nav-toggle').click();
  if (await handle.isVisible() || (await sidebar.boundingBox()).width > 280) {
    throw new Error('Resized desktop sidebar broke mobile navigation');
  }
  await page.keyboard.press('Escape');
  await page.setViewportSize({ width: 1440, height: 900 });
  await assertWidth(560);
  await handle.dblclick();
  await assertWidth(270);
  await page.reload({ waitUntil: 'networkidle' });
  await assertWidth(270);

  // Resize while a real response is streaming, without replacing its DOM.
  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  await page.locator('#messages [sse-connect]').waitFor();
  const connection = await page.locator('#messages [sse-connect]').getAttribute('sse-connect');
  await dragBy(90);
  await assertWidth(360);
  await page.waitForFunction(() => document.querySelector('#messages [sse-connect]')?.dataset.streamState === 'finished');
  if (await page.locator('#messages [sse-connect]').getAttribute('sse-connect') !== connection ||
      await page.locator('#messages .msg.assistant').count() !== 1) {
    throw new Error('Sidebar resizing restarted or replaced the answer');
  }
  await handle.focus();
  await page.keyboard.press('Enter');
  await assertWidth(270);
  await page.evaluate(() => {
    Storage.prototype.setItem = function () { throw new DOMException('Test storage denied', 'SecurityError'); };
  });
  await page.keyboard.press('ArrowRight');
  await assertWidth(280);
  if (!(await page.locator('#sidebar-resize-status').isVisible())) {
    throw new Error('Failed sidebar persistence must show an explicit notice');
  }
  const chat = new URL(page.url()).pathname.match(/^\/chat\/(\d+)$/);
  if (!chat || Object.values(index.chats).includes(Number(chat[1]))) throw new Error('Expected a disposable resize test chat');
  if (!(await page.request.delete(`${base}/chats/${chat[1]}`)).ok()) throw new Error('Unable to remove resize test chat');
}
