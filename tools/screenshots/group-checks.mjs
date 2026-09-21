export async function verifyChatGroups(page, base, index, mobile) {
  await page.goto(`${base}/`, { waitUntil: 'networkidle' });
  const path = new URL(page.url()).pathname;
  const match = path.match(/^\/chat\/(\d+)$/);
  if (!match || Object.values(index.chats).includes(Number(match[1]))) throw new Error('Expected a disposable test chat');
  const chatID = match[1];
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await showSidebar(page);
  const chatButton = await page.locator('.new-chat-form .btn-new').boundingBox();
  const groupButton = await page.locator('#new-group').boundingBox();
  if (!chatButton || !groupButton || chatButton.x >= groupButton.x ||
      Math.abs(chatButton.y - groupButton.y) > 3 || groupButton.x + groupButton.width > (await page.locator('#sidebar').boundingBox()).x + (await page.locator('#sidebar').boundingBox()).width + 1) {
    throw new Error('New chat/group controls are not side by side inside the sidebar');
  }
  await page.evaluate(() => { window.originalMessages = document.getElementById('messages'); });
  const groupA = await createGroup(page, 'Training & Recovery', 'blue');
  const groupB = await createGroup(page, 'Work', 'green');
  await assertSameConversation(page, path);
  await moveViaDialog(page, chatID, groupA);
  await page.locator(`[data-group-id="${groupA}"] #chat-row-${chatID}`).waitFor();
  if (!(await page.locator(`#chat-row-${chatID}`).getAttribute('class')).includes('active')) {
    throw new Error('Moving the current chat lost its selected state');
  }
  await page.locator(`#group-toggle-${groupA}`).click();
  await page.locator(`#group-toggle-${groupA}[aria-expanded="false"]`).waitFor();
  await page.reload({ waitUntil: 'networkidle' });
  await showSidebar(page);
  if (await page.locator(`#group-toggle-${groupA}`).getAttribute('aria-expanded') !== 'false' ||
      !(await page.locator(`[data-group-id="${groupA}"]`).getAttribute('class')).includes('group-color-blue')) {
    throw new Error('Group color/collapse did not persist across reload');
  }
  await page.locator(`#group-toggle-${groupA}`).click();
  await page.locator(`#group-toggle-${groupA}[aria-expanded="true"]`).waitFor();
  await page.waitForFunction(() => !document.querySelector('#sidebar .htmx-request, #sidebar .htmx-settling'));

  // Native dialogs must retain usable focus and keep the mobile sidebar open.
  await page.locator(`#group-edit-${groupA}`).press('Enter');
  await page.locator('#group-dialog[open]').waitFor();
  await page.keyboard.press('Escape');
  await page.locator('#group-dialog').waitFor({ state: 'detached' });
  if (mobile && !(await page.locator('body').getAttribute('class') || '').includes('nav-open')) {
    throw new Error('Canceling the group dialog unexpectedly closed the mobile sidebar');
  }
  await page.locator(`#group-edit-${groupA}`).click();
  await page.locator('#group-title').fill('Renamed training');
  await page.locator('#group-color').selectOption('violet');
  await page.locator('.group-form button[type="submit"]').click();
  await page.locator('#group-dialog').waitFor({ state: 'detached' });
  if ((await page.locator(`#group-name-${groupA}`).textContent()) !== 'Renamed training' ||
      !(await page.locator(`[data-group-id="${groupA}"]`).getAttribute('class')).includes('group-color-violet')) {
    throw new Error('Group rename or color update failed');
  }
  await page.evaluate(() => { window.originalMessages = document.getElementById('messages'); });
  if (mobile) await page.keyboard.press('Escape');
  await page.locator('#chat-form textarea').fill(index.stream_prompt);
  await page.locator('#chat-form button[type="submit"]').click();
  const stream = page.locator('#messages [sse-connect]').last();
  await stream.waitFor();
  const streamURL = await stream.getAttribute('sse-connect');
  await showSidebar(page);
  if (mobile) {
    await moveViaDialog(page, chatID, groupB);
  } else {
    await page.locator(`#chat-row-${chatID}`).dragTo(page.locator(`#group-toggle-${groupB}`));
  }
  await page.locator(`[data-group-id="${groupB}"] #chat-row-${chatID}`).waitFor();
  await assertSameConversation(page, path);
  if (await stream.getAttribute('sse-connect') !== streamURL) throw new Error('Moving a chat replaced its stream');
  await page.waitForFunction(() => document.querySelector('#messages [sse-connect]')?.dataset.streamState === 'finished');
  // A title update must render the grouped sidebar, not the old flat one.
  await page.locator(`[data-group-id="${groupB}"] #chat-row-${chatID}`).waitFor();
  if (!(await stream.locator('.bubble').innerText()).trim()) throw new Error('The answer disappeared during movement');
  await page.reload({ waitUntil: 'networkidle' });
  await showSidebar(page);
  await page.locator(`[data-group-id="${groupB}"] #chat-row-${chatID}`).waitFor();
  await page.evaluate(() => { window.originalMessages = document.getElementById('messages'); });
  const answer = await page.locator('#messages .msg.assistant .bubble').last().innerText();
  await page.locator(`#group-edit-${groupB}`).click();
  await page.locator('#group-dialog[open]').waitFor();
  page.once('dialog', dialog => dialog.accept());
  await page.locator('#group-dialog [hx-delete]').click();
  await page.locator('#group-dialog').waitFor({ state: 'detached' });
  await page.locator(`[data-group-id="0"] #chat-row-${chatID}`).waitFor();
  await assertSameConversation(page, path);
  if (await page.locator('#messages .msg.assistant .bubble').last().innerText() !== answer) {
    throw new Error('Removing a group altered its chat contents');
  }
  if (!mobile) {
    await page.locator(`#chat-row-${chatID}`).dragTo(page.locator(`#group-toggle-${groupA}`));
    await page.locator(`[data-group-id="${groupA}"] #chat-row-${chatID}`).waitFor();
    await page.locator(`#chat-row-${chatID}`).dragTo(page.locator('[data-group-id="0"]'));
    await page.locator(`[data-group-id="0"] #chat-row-${chatID}`).waitFor();
  }
  if (errors.length) throw new Error(`Group UI errors: ${errors.join('; ')}`);
  for (const resource of [`/groups/${groupA}`, `/chats/${chatID}`]) {
    const removed = await page.request.delete(base + resource);
    if (!removed.ok()) throw new Error(`Unable to clean up test resource ${resource}: ${removed.status()}`);
  }
}

export async function createGroup(page, name, color) {
  await page.locator('#new-group').click();
  await page.locator('#group-dialog[open]').waitFor();
  await page.locator('#group-title').fill(name);
  await page.locator('#group-color').selectOption(color);
  await page.locator('.group-form button[type="submit"]').click();
  await page.locator('#group-dialog').waitFor({ state: 'detached' });
  const group = page.locator('.chat-group').filter({ has: page.locator('.group-name', { hasText: name }) }).last();
  await group.waitFor();
  return group.getAttribute('data-group-id');
}

async function moveViaDialog(page, chatID, groupID) {
  await page.locator(`#chat-move-${chatID}`).click();
  await page.locator('#group-dialog[open]').waitFor();
  await page.locator('#move-group').selectOption(groupID);
  await page.locator('.group-form button[type="submit"]').click();
  await page.locator('#group-dialog').waitFor({ state: 'detached' });
}

async function showSidebar(page) {
  if (await page.locator('#nav-toggle').isVisible() &&
      !(await page.locator('body').getAttribute('class') || '').includes('nav-open')) {
    await page.locator('#nav-toggle').click();
  }
}

async function assertSameConversation(page, path) {
  if (new URL(page.url()).pathname !== path ||
      !(await page.evaluate(() => document.getElementById('messages') === window.originalMessages))) {
    throw new Error('A sidebar-only action navigated away or replaced the active conversation');
  }
}
