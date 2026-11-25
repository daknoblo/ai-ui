(() => {
  if (window.AIUI_SETTINGS) return;
  const controllers = new WeakMap();

  function initialize() {
    const form = document.querySelector('.config-form[data-autosave]');
    if (!form || controllers.has(form)) return;
    const overlay = form.closest('.settings-overlay');
    const toast = overlay.querySelector('.settings-save-toast');
    let version = 0, savedVersion = 0, attemptedVersion = 0, running = null, timer = null, hideTimer = null, reload = false;

    function status(state, message) {
      clearTimeout(hideTimer);
      form.dataset.saveState = state;
      toast.dataset.state = state;
      toast.hidden = false;
      toast.setAttribute('role', state === 'error' ? 'alert' : 'status');
      toast.querySelector('span').textContent = message;
      toast.querySelector('[data-settings-discard]').hidden = state !== 'error';
      if (state === 'saved') hideTimer = setTimeout(() => { toast.hidden = true; }, 2200);
    }

    async function save() {
      clearTimeout(timer);
      if (running) return running;
      if (version === savedVersion) return true;
      running = (async () => {
        while (savedVersion < version) {
          attemptedVersion = version;
          if (!form.checkValidity()) {
            status('error', form.dataset.invalid);
            return false;
          }
          const sending = version;
          const body = new URLSearchParams(new FormData(form));
          body.delete('confirm_reindex');
          body.delete('reindex_target');
          status('saving', form.dataset.saving);
          try {
            const response = await fetch(form.action, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Config-Autosave': '1' },
              body, signal: AbortSignal.timeout(30000),
            });
            const data = await response.json();
            if (!response.ok || data.saved !== true) {
              status('error', data.error || form.dataset.saveError);
              return false;
            }
            savedVersion = sending;
            reload ||= data.reload === true;
            if (version !== sending) continue;
            if (form.isConnected) {
              htmx.swap('#verify-results', data.checks, { swapStyle: 'innerHTML' });
              htmx.swap('#embedding-index', data.index, { swapStyle: 'outerHTML' });
            }
            status('saved', form.dataset.saved);
          } catch (error) {
            console.error('Configuration autosave failed', error);
            status('error', form.dataset.saveError);
            return false;
          }
        }
        if (reload && form.isConnected) {
          sessionStorage.setItem('ai-ui-settings-reopen', '1');
          location.reload();
        }
        return true;
      })();
      try { return await running; } finally {
        running = null;
        if (version > attemptedVersion && version > savedVersion) timer = setTimeout(save, 150);
      }
    }

    function changed(event) {
      const field = event.target;
      if (!field.name || field.disabled || field.readOnly || field.closest('#embedding-index')) return;
      version++;
      status('saving', form.dataset.saving);
      clearTimeout(timer);
      timer = setTimeout(save, field.matches('select, [type="checkbox"]') ? 150 : 600);
    }
    form.addEventListener('input', changed);
    form.addEventListener('change', changed);
    form.addEventListener('submit', event => { event.preventDefault(); save(); });
    controllers.set(form, { save, dirty: () => savedVersion < version, notifySaved: () => status('saved', form.dataset.saved) });
  }

  document.addEventListener('click', async event => {
    const overlay = event.target.closest('.settings-overlay');
    if (!overlay) return;
    const form = overlay.querySelector('.config-form');
    const controller = controllers.get(form);
    if (!controller) return;
    if (event.target.closest('[data-settings-discard]')) {
      event.preventDefault();
      overlay.remove();
      return;
    }
    const closing = event.target === overlay || event.target.closest('[data-settings-close]');
    const action = event.target.closest('button[hx-post]');
    if (!closing && (!action || !controller.dirty())) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (!(await controller.save())) return;
    if (closing) overlay.remove();
    else if (action.isConnected) action.click();
  }, true);

  window.addEventListener('beforeunload', event => {
    const form = document.querySelector('.config-form[data-autosave]');
    if (form && controllers.get(form)?.dirty()) {
      event.preventDefault();
      event.returnValue = '';
    }
  });
  document.body.addEventListener('htmx:afterSwap', initialize);
  window.addEventListener('load', () => {
    initialize();
    if (sessionStorage.getItem('ai-ui-settings-reopen') === '1') {
      sessionStorage.removeItem('ai-ui-settings-reopen');
      htmx.ajax('GET', '/config', { target: '#modal-root', swap: 'innerHTML' }).then(() => {
        initialize();
        controllers.get(document.querySelector('.config-form'))?.notifySaved();
      });
    }
  });
  window.AIUI_SETTINGS = {
    flush: () => {
      initialize();
      const form = document.querySelector('.config-form[data-autosave]');
      return controllers.get(form)?.save() ?? Promise.resolve(true);
    },
  };
})();
