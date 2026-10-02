// denma: a center's Advanced page (views/denma-center.html,
// cmd/denma_center.go). Saving reloads the center; the page reloads once
// it's back.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as u from '../utils.js';

function denmaCenter() {
  const form = { ...(window._denmaCenter || {}) };
  return {
    form,
    notify: (form.notify_emails || []).join('\n'),
    reloading: false,

    async onSubmit() {
      const data = {
        ...this.form,
        notify_emails: this.notify.split(/[\n,;]+/).map((e) => e.trim()).filter(Boolean),
      };
      const res = await api('center', '/denma/center', 'PUT', data);
      if (!res.reloading) {
        u.toast('Saved. The center is sending a campaign; the changes apply when it has finished.', 'success');
        return;
      }

      // Wait for the reloaded center, then show it.
      this.reloading = true;
      const start = Date.now();
      const poll = setInterval(async () => {
        try {
          const r = await fetch(`${urls.api}/health`);
          if ((r.ok && Date.now() - start > 1200) || Date.now() - start > 15000) {
            clearInterval(poll);
            u.reload({ message: 'Saved', variant: 'success' });
          }
        } catch { /* still reloading */ }
      }, 400);
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaCenter', denmaCenter);
}, { once: true });
