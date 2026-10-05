// denma: a center's Config page (views/denma-center.html,
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
    utm: (form.utm_domains || []).join('\n'),
    reloading: false,

    async onSubmit() {
      const data = {
        ...this.form,
        notify_emails: this.notify.split(/[\n,;]+/).map((e) => e.trim()).filter(Boolean),
        utm_domains: this.utm.split(/[\s,;]+/).map((e) => e.trim()).filter(Boolean),
        signup_target_list: this.form.signup_holding_list ? this.form.signup_target_list : 0,
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

// How long ago an ISO time was: "4 minutes ago".
function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  for (const [n, name] of [[86400, 'day'], [3600, 'hour'], [60, 'minute']]) {
    if (s >= n) {
      const v = Math.floor(s / n);
      return `${v} ${name}${v === 1 ? '' : 's'} ago`;
    }
  }
  return 'just now';
}

// The Config page's sign-up webhooks (cmd/denma_signup.go), saved as they're
// changed.
function denmaSignupHooks() {
  return {
    hooks: window._denmaSignupHooks || [],
    lists: window._denmaSignupLists || [],
    edit: { id: 0, name: '', list_ids: [] },
    ago,

    listNames(h) {
      const names = Object.fromEntries(this.lists.map((l) => [l.id, l.name]));
      return h.list_ids.map((id) => names[id] || `#${id}`).join(', ');
    },

    copy(h) {
      u.copyToClipboard(h.url);
    },

    // A plain HTML form for the first webhook (or a placeholder address).
    htmlForm() {
      const url = this.hooks.length ? this.hooks[0].url : 'https://…/signup/…';
      return `<form method="post" action="${url}">
  <input type="email" name="email" required placeholder="E-mail">
  <input type="text" name="name" placeholder="Name">
  <label><input type="checkbox" name="consent" value="yes" required> I'd like to receive your e-mails</label>
  <button type="submit">Subscribe</button>
</form>`;
    },

    onAdd() {
      this.edit = { id: 0, name: '', list_ids: [] };
      this.$refs.hookDialog.showModal();
      this.$nextTick(() => this.$refs.hookName.focus());
    },

    onEdit(h) {
      this.edit = { id: h.id, name: h.name, list_ids: [...h.list_ids] };
      this.$refs.hookDialog.showModal();
    },

    put(h) {
      const i = this.hooks.findIndex((x) => x.id === h.id);
      if (i < 0) {
        this.hooks.push(h);
      } else {
        this.hooks.splice(i, 1, h);
      }
    },

    async onSubmit() {
      const body = { name: this.edit.name, list_ids: this.edit.list_ids };
      const h = this.edit.id
        ? await api('hooks', `/denma/center/signup-hooks/${this.edit.id}`, 'PUT', body)
        : await api('hooks', '/denma/center/signup-hooks', 'POST', body);
      this.$refs.hookDialog.close();
      this.put(h);
      u.toast(this.edit.id ? `Saved ${h.name}.` : `Added ${h.name}. Give its address to the form.`, 'success');
    },

    async onNewKey(h) {
      if (!(await u.confirm(`Give ${h.name} a new address? The old one stops working at once, so the form has to be given the new one.`))) {
        return;
      }
      this.put(await api('hooks', `/denma/center/signup-hooks/${h.id}/key`, 'POST'));
      u.toast(`${h.name} has a new address.`, 'success');
    },

    async onDelete(h) {
      if (!(await u.confirm(`Delete ${h.name}? Its address stops working, so the form can't sign anyone up through it.`))) {
        return;
      }
      await api('hooks', `/denma/center/signup-hooks/${h.id}`, 'DELETE');
      this.hooks = this.hooks.filter((x) => x.id !== h.id);
      u.toast(`Deleted ${h.name}.`, 'success');
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaCenter', denmaCenter);
  Alpine.data('denmaSignupHooks', denmaSignupHooks);
}, { once: true });
