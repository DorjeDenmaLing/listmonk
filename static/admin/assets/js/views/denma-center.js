// denma: a center's Config pages, General (views/denma-center.html),
// Webhooks (views/denma-webhooks.html), cmd/denma_center.go, and API
// (views/denma-api.html, cmd/denma_api.go). Saving General reloads the
// center; the page reloads once it's back.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as u from '../utils.js';
import { denmaTagAutocomplete } from '../denma-tags-ui.js';

// Every time zone the browser knows (with the center's), for its sending
// hours (cmd/denma_sendwindow.go).
function timeZones(cur) {
  const all = (Intl.supportedValuesOf && Intl.supportedValuesOf('timeZone')) || [];
  return cur && !all.includes(cur) ? [cur, ...all] : all;
}

function denmaCenter() {
  const form = { ...(window._denmaCenter || {}) };
  form.time_zone = form.time_zone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  form.send_window = {
    on: false, days: [0, 1, 2, 3, 4, 5, 6], from: '08:00', to: '21:00', ...(form.send_window || {}),
  };
  form.send_window.days = form.send_window.days || [];
  return {
    form,
    timeZones: timeZones(form.time_zone),
    weekDays: [[1, 'Mon'], [2, 'Tue'], [3, 'Wed'], [4, 'Thu'], [5, 'Fri'], [6, 'Sat'], [0, 'Sun']],
    notify: (form.notify_emails || []).join('\n'),
    utm: (form.utm_domains || []).join('\n'),
    reloading: false,
    mediaField: null, // the field the Media picker fills: logo_url or favicon_url

    init() {
      // What's chosen in the Media picker (partials/media-picker.html, as
      // the campaign page's).
      window.addEventListener('message', (e) => {
        if (e.origin !== window.location.origin || e.source !== this.$refs.mediaFrame?.contentWindow) {
          return;
        }
        if (e.data?.type === 'media-select' && this.mediaField) {
          this.form[this.mediaField] = e.data.media.url;
          this.$refs.mediaDialog.close();
        }
      });
    },

    openMedia(field) {
      this.mediaField = field;
      this.$refs.mediaFrame.src = `${urls.admin}/campaigns/media/fragment?t=${Date.now()}`;
      this.$refs.mediaDialog.showModal();
    },

    onMediaClose() {
      this.mediaField = null;
    },

    async onSubmit() {
      const data = {
        ...this.form,
        notify_emails: this.notify.split(/[\n,;]+/).map((e) => e.trim()).filter(Boolean),
        utm_domains: this.utm.split(/[\s,;]+/).map((e) => e.trim()).filter(Boolean),
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

// Config -> Webhooks: the sign-up webhooks (cmd/denma_signup.go), saved as
// they're changed: Form webhooks (the center's admins') and Server webhooks
// (the superadmins'). A new address or secret is shown once.
const denmaNewHookForm = (kind) => ({
  id: 0, kind, name: '', list_ids: [], tags: [], request_tags: [],
  per_hour: window._denmaSignupPerHour || 50, create_tags: false, allowed_from: '',
});

function denmaSignupHooks() {
  return {
    hooks: window._denmaSignupHooks || [],
    lists: window._denmaSignupLists || [],
    isSuper: !!window._denmaSignupSuper,
    edit: denmaNewHookForm('form'),
    rotate: { id: 0, name: '', overlap_hours: 0 },
    token: '',
    tokenKind: 'form',
    ago,
    denmaTagAutocomplete,

    // A Form webhook's lists are double opt-in; a Server webhook's any.
    listsFor(h) {
      return h.kind === 'server' ? this.lists : this.lists.filter((l) => l.optin === 'double');
    },

    listNames(h) {
      const names = Object.fromEntries(this.lists.map((l) => [l.id, l.name]));
      return h.list_ids.map((id) => names[id] || `#${id}`).join(', ');
    },

    kindText(h) {
      return h.kind === 'server' ? 'Server webhook: vouches for consent' : 'Form webhook: double opt-in';
    },

    requestTagsText(h) {
      const given = h.request_tags.join(', ');
      if (h.create_tags) {
        return given ? `A request may give ${given}, and new tags` : 'A request may give new tags';
      }
      return `A request may give ${given}${h.kind === 'form' ? ' (to someone new)' : ''}`;
    },

    canChange(h) {
      return h.kind !== 'server' || this.isSuper;
    },

    dialogTitle() {
      const what = this.edit.kind === 'server' ? 'Server webhook' : 'Form webhook';
      return this.edit.id ? `Edit ${what}` : `Add ${what}`;
    },

    canSave() {
      const e = this.edit;
      return e.name.trim() && e.list_ids.length && (e.kind !== 'form' || e.per_hour > 0);
    },

    copy(text) {
      u.copyToClipboard(text);
    },

    // A plain HTML form, with a placeholder address.
    htmlForm() {
      return `<form method="post" action="https://…/signup/…">
  <input type="email" name="email" required placeholder="E-mail">
  <input type="text" name="name" placeholder="Name">
  <label><input type="checkbox" name="consent" value="yes" required> I'd like to receive your e-mails</label>
  <button type="submit">Subscribe</button>
</form>`;
    },

    onAdd(kind) {
      this.edit = denmaNewHookForm(kind);
      this.$refs.hookDialog.showModal();
      this.$nextTick(() => this.$refs.hookName.focus());
    },

    onEdit(h) {
      this.edit = {
        id: h.id,
        kind: h.kind,
        name: h.name,
        list_ids: [...h.list_ids],
        tags: [...h.tags],
        request_tags: [...h.request_tags],
        per_hour: h.per_hour,
        create_tags: h.create_tags,
        allowed_from: h.allowed_from.join('\n'),
      };
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

    // A new address or secret, shown once.
    showToken(h) {
      this.token = h.url || h.secret || '';
      this.tokenKind = h.kind;
      delete h.url;
      delete h.secret;
      if (this.token) {
        this.$refs.tokenDialog.showModal();
      }
    },

    async onSubmit() {
      const e = this.edit;
      const body = {
        name: e.name,
        list_ids: e.list_ids,
        tags: e.tags.map(String),
        request_tags: e.request_tags.map(String),
        per_hour: e.per_hour,
        create_tags: e.create_tags,
        allowed_from: e.allowed_from.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean),
      };
      const h = e.id
        ? await api('hooks', `/denma/center/signup-hooks/${e.id}`, 'PUT', body)
        : await api('hooks', `/denma/center/signup-hooks?kind=${e.kind}`, 'POST', body);
      this.$refs.hookDialog.close();
      if (e.id) {
        u.toast(`Saved ${h.name}.`, 'success');
      } else {
        this.showToken(h);
      }
      this.put(h);
    },

    async onNewKey(h) {
      if (h.kind === 'server') {
        this.rotate = { id: h.id, name: h.name, overlap_hours: 0 };
        this.$refs.rotateDialog.showModal();
        return;
      }
      if (!(await u.confirm(`Give ${h.name} a new address? The old one stops working at once, so the form has to be given the new one.`))) {
        return;
      }
      const nh = await api('hooks', `/denma/center/signup-hooks/${h.id}/key`, 'POST');
      this.showToken(nh);
      this.put(nh);
    },

    async onRotate() {
      const nh = await api('hooks', `/denma/center/signup-hooks/${this.rotate.id}/key`, 'POST', { overlap_hours: this.rotate.overlap_hours });
      this.$refs.rotateDialog.close();
      this.showToken(nh);
      this.put(nh);
    },

    async onDelete(h) {
      const what = h.kind === 'server' ? 'Its secret stops working, so the system using it can\'t sign anyone up through it.'
        : 'Its address stops working, so the form can\'t sign anyone up through it.';
      if (!(await u.confirm(`Delete ${h.name}? ${what}`))) {
        return;
      }
      await api('hooks', `/denma/center/signup-hooks/${h.id}`, 'DELETE');
      this.hooks = this.hooks.filter((x) => x.id !== h.id);
      u.toast(`Deleted ${h.name}.`, 'success');
    },
  };
}

// A new key's settings, on Config -> API.
const denmaNewKeyForm = () => ({
		id: 0, name: '', can: [], allowed_from: '', link_domains: '', webhook_url: '', sender: '', daily_limit: 1000, expires_on: '',
});

// A date as YYYY-MM-DD (UTC).
const denmaYMD = (d) => d.toISOString().slice(0, 10);

// Config -> API: the sending API's keys (cmd/denma_api.go), saved as
// they're changed. A new key is shown once. A deleted key stays listed,
// to be restored, for a few days.
function denmaAPIKeys() {
  return {
				keys: window._denmaAPIKeys || [],
				ownDomains: window._denmaAPIDomains || [],
    centerSender: window._denmaAPISender || '',
    keepDays: window._denmaAPIKeepDays || 7,
    edit: denmaNewKeyForm(),
    rotate: { id: 0, name: '', overlap_hours: 0 },
    del: { id: 0, name: '', leaked: false },
				newKey: '',
				newSecret: '',
				ago,

    when(iso) {
      return new Date(iso).toLocaleString();
    },

    tomorrow() {
      return denmaYMD(new Date(Date.now() + 86400000));
    },

    // Whether a key (or the form) can send e-mail.
    sends(k) {
      return k.can.includes('messages') || k.can.includes('campaigns');
    },

    sendText(k) {
      return `Sends from ${k.sender || this.centerSender}, at most ${k.daily_limit.toLocaleString()} e-mails a day`
        + (k.sent_24h !== undefined ? ` (${k.sent_24h.toLocaleString()} in the last 24 hours)` : '');
    },

    // Who made it, and when it expires.
    madeText(k) {
      let s = `Made ${new Date(k.created_at).toLocaleDateString()}${k.created_by ? ` by ${k.created_by}` : ''}`;
      if (k.expires_at) {
        const at = new Date(k.expires_at);
        s += at < new Date() ? '; expired' : `; expires ${at.toLocaleDateString()}`;
      }
      return s;
    },

    deletedText(k) {
      return `Deleted ${ago(k.deleted_at)}${k.deleted_by ? ` by ${k.deleted_by}` : ''}${k.leaked ? ', as leaked' : ''}. `
        + `It can be restored for ${this.keepDays} days from then.`;
    },

// What a key can do, in words.
    canText(k) {
      const can = [];
      if (k.can.includes('messages')) {
        can.push('send transactional e-mails');
      }
      if (k.can.includes('campaigns')) {
        can.push('send campaigns');
      }
if (k.can.includes('events')) {
        can.push('read the events');
      }
      const s = can.join(', ');
      return can.length ? `Can ${s}` : 'Can do nothing';
    },

    canSave() {
      const e = this.edit;
						return e.name.trim() && e.can.length && (!this.sends(e) || e.daily_limit > 0);
    },

    webhookStatus(k) {
      if (k.webhook_error) {
        const next = k.webhook_next_at ? `; trying again ${new Date(k.webhook_next_at).toLocaleTimeString()}` : '';
        return `Failing (${k.webhook_failures} in a row): ${k.webhook_error}${next}`;
      }
      return k.webhook_ok_at ? `Delivered ${ago(k.webhook_ok_at)}` : 'Nothing to deliver yet';
    },

    copy(text) {
      u.copyToClipboard(text);
    },

    put(k) {
      const i = this.keys.findIndex((x) => x.id === k.id);
      if (i < 0) {
        this.keys.push(k);
      } else {
        this.keys.splice(i, 1, k);
      }
    },

				// A new key, or its notifications' signing secret: shown once.
				showKey(k) {
						this.newKey = k.key || '';
						this.newSecret = k.webhook_url ? (k.webhook_secret || '') : '';
						delete k.key;
						delete k.webhook_secret;
						if (this.newKey || this.newSecret) {
								this.$refs.newKeyDialog.showModal();
						}
				},

    onAdd() {
      this.edit = denmaNewKeyForm();
      this.$refs.keyDialog.showModal();
      this.$nextTick(() => this.$refs.keyName.focus());
    },

    onEdit(k) {
      this.edit = {
        id: k.id,
        name: k.name,
        can: [...k.can],
        allowed_from: k.allowed_from.join('\n'),
								webhook_url: k.webhook_url,
								link_domains: (k.link_domains || []).join('\n'),
								sender: k.sender,
        daily_limit: k.daily_limit,
        expires_on: k.expires_at ? denmaYMD(new Date(k.expires_at)) : '',
      };
      this.$refs.keyDialog.showModal();
    },

    async onSubmit() {
      const body = {
        name: this.edit.name,
        allowed_from: this.edit.allowed_from.split(/[\s,;]+/).map((s) => s.trim()).filter(Boolean),
        webhook_url: this.edit.webhook_url.trim(),
								can: this.edit.can,
								link_domains: this.edit.link_domains.split(/[\s,;]+/).map((s) => s.trim()).filter(Boolean),
								sender: this.edit.sender.trim(),
        daily_limit: this.edit.daily_limit || 0,
        expires_on: this.edit.expires_on,
      };
      const k = this.edit.id
        ? await api('keys', `/denma/center/api-keys/${this.edit.id}`, 'PUT', body)
        : await api('keys', '/denma/center/api-keys', 'POST', body);
						this.$refs.keyDialog.close();
						if (k.key || k.webhook_secret) {
								this.showKey(k);
						} else {
								u.toast(`Saved ${k.name}.`, 'success');
						}
						this.put(k);
    },

    onNewKey(k) {
      this.rotate = { id: k.id, name: k.name, overlap_hours: 0 };
      this.$refs.rotateDialog.showModal();
    },

    async onRotate() {
      const nk = await api('keys', `/denma/center/api-keys/${this.rotate.id}/key`, 'POST', { overlap_hours: this.rotate.overlap_hours });
      this.$refs.rotateDialog.close();
      this.showKey(nk);
      this.put(nk);
    },

    async onNewSecret(k) {
						if (!(await u.confirm(`Give ${k.name}'s notifications a new signing secret? The other system has to check signatures with the new one from now on.`))) {
								return;
						}
						const nk = await api('keys', `/denma/center/api-keys/${k.id}/webhook-secret`, 'POST');
						this.showKey(nk);
						this.put(nk);
    },

    async onTest(k) {
      await api('test', `/denma/center/api-keys/${k.id}/webhook-test`, 'POST');
						u.toast(`${k.name}'s notifications address took a test event.`, 'success');
    },

    onDelete(k) {
      this.del = { id: k.id, name: k.name, leaked: false };
      this.$refs.deleteDialog.showModal();
    },

    async onDeleteConfirm() {
      const { id, name, leaked } = this.del;
      const res = await api('keys', `/denma/center/api-keys/${id}${leaked ? '?leaked=true' : ''}`, 'DELETE');
      this.$refs.deleteDialog.close();
      this.keys = await api('keys', '/denma/center/api-keys', 'GET');
      const n = res.cancelled;
      u.toast(leaked ? `Deleted ${name}, and cancelled ${n} of its campaign${n === 1 ? '' : 's'}.` : `Deleted ${name}.`, 'success');
    },

    async onRestore(k) {
      const msg = k.leaked
        ? `Restore ${k.name}? It gets a new key, shown once: the old one never works again.`
        : `Restore ${k.name}? Its key works again at once.`;
      if (!(await u.confirm(msg))) {
        return;
      }
      const rk = await api('keys', `/denma/center/api-keys/${k.id}/restore`, 'POST');
      this.put(rk);
      if (rk.key) {
        this.showKey(rk);
      } else {
        u.toast(`Restored ${rk.name}.`, 'success');
      }
    },

    async onResume() {
      if (!(await u.confirm('Resume sending through the API? Its paused campaigns go on too.'))) {
        return;
      }
      await api('resume', '/denma/center/api/resume', 'POST');
      u.reload({ message: 'Sending through the API resumed.', variant: 'success' });
    },
  };
}

// The sending API's reference on Config -> API, made from its OpenAPI 3.1
// description (cmd/denma_api_openapi.json): each operation by tag, with what
// a key needs (x-denma-can), its parameters, request fields, a curl example,
// the answer's fields and its responses; and the webhook. The sign-up
// webhooks' (cmd/denma_signup_openapi.json) on Config -> Webhooks, with
// specVar '_denmaSignupSpec' and their secret in its curl examples.
function denmaAPIDocs(specVar = '_denmaAPISpec', secretVar = 'KEY') {
		const spec = window[specVar] || { info: {}, paths: {} };
		const can = {
				messages: 'send transactional e-mails',
				campaigns: 'send campaigns',
				events: 'read the events',
		};
  const methods = ['get', 'post', 'put', 'patch', 'delete'];

  const esc = (t) => String(t).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));

  // A schema, its $ref (#/components/...) followed.
  const resolve = (sc) => {
    let out = sc || {};
    while (out.$ref) {
      out = out.$ref.replace(/^#\//, '').split('/').reduce((o, k) => (o || {})[k], spec) || {};
    }
    return out;
  };

  const typeOf = (sc0) => {
    const sc = resolve(sc0);
    if (sc.oneOf) {
      return sc.oneOf.map(typeOf).join(' or ');
    }
    if (sc.allOf) {
      return 'object';
    }
    let t = Array.isArray(sc.type) ? sc.type.join(' or ') : (sc.type || 'any');
    if (sc.type === 'array') {
      t = `array of ${typeOf(sc.items)}`;
    }
    if (sc.format) {
      t += ` (${sc.format})`;
    }
    if (sc.enum) {
      t += `: ${sc.enum.join(', ')}`;
    }
    return t;
  };

  // An object schema's fields, nested ones as a.b and a[].b.
  const fields = (sc0, prefix = '') => {
    const sc = resolve(sc0);
    const rows = [];
    Object.entries(sc.properties || {}).forEach(([name, p0]) => {
      const p = resolve(p0);
      const desc = [p.description || '', p.default !== undefined ? `Default: \`${JSON.stringify(p.default)}\`.` : '']
        .filter(Boolean).join(' ');
      rows.push({
        name: prefix + name, type: typeOf(p0), required: (sc.required || []).includes(name), desc,
      });
      const items = p.type === 'array' ? resolve(p.items) : null;
      if (items && items.properties) {
        rows.push(...fields(items, `${prefix}${name}[].`));
      } else if (p.properties) {
        rows.push(...fields(p, `${prefix}${name}.`));
      }
    });
    return rows;
  };

  const json = (r) => ((resolve(r).content || {})['application/json'] || {});

  // The operations, under their first tag, in the document's order.
  const groups = (spec.tags || []).map((t) => ({ name: t.name, description: t.description || '', ops: [] }));
  const add = (path, method, op, pathParams) => {
    const g = groups.find((x) => x.name === (op.tags || [])[0]);
    if (g) {
      g.ops.push({
        key: `${method} ${path}`, method: method.toUpperCase(), path, op, pathParams: pathParams || [],
      });
    }
  };
  Object.entries(spec.paths || {}).forEach(([path, item]) => {
    methods.filter((m) => item[m]).forEach((m) => add(path, m, item[m], item.parameters));
  });
  Object.entries(spec.webhooks || {}).forEach(([name, item]) => {
    methods.filter((m) => item[m]).forEach((m) => add(`(your webhook address): ${name}`, m, item[m], item.parameters));
  });

  return {
    spec,
    groups,
    base: ((spec.servers || [])[0] || {}).url || '',

    // Markdown's paragraphs and `code`, as HTML (the text escaped first).
    md(t, inline) {
      const html = (t || '').split(/\n{2,}/).map((p) => esc(p).replace(/`([^`]+)`/g, '<code>$1</code>'));
      return inline ? html.join(' ') : html.map((p) => `<p>${p}</p>`).join('');
    },

    isWebhook(o) {
      return o.path.startsWith('(');
    },

    needs(o) {
      if (this.isWebhook(o)) {
        return 'none: the center sends this to the key\'s webhook, signed with its secret.';
      }
      const c = o.op['x-denma-can'];
      if (c) {
        return `one that can ${can[c] || c} (x-denma-can: ${c}).`;
      }
      return o.op.security && !o.op.security.length ? 'none needed.' : 'any.';
    },

    params(o) {
      return [...o.pathParams, ...(o.op.parameters || [])].map(resolve).map((p) => ({
        name: p.name, in: p.in, required: !!p.required, type: typeOf(p.schema), desc: p.description || '',
      }));
    },

    body(o) {
      return o.op.requestBody ? fields(json(o.op.requestBody).schema) : [];
    },

    answer(o) {
      const ok = (o.op.responses || {})['200'];
      if (!ok) {
        return [];
      }
      const data = resolve((resolve(json(ok).schema).properties || {}).data);
      return fields(data.type === 'array' ? data.items : data);
    },

    responses(o) {
      return Object.entries(o.op.responses || {}).map(([code, r]) => ({ code, desc: resolve(r).description || '' }));
    },

    // A curl command for the operation, with its example.
    curl(o) {
      if (this.isWebhook(o)) {
        return '';
      }
						const path = o.path === '/' ? '' : o.path.replace('{id}', '42').replace(/\{(\w+)\}/g, '<$1>');
						const lines = [`curl ${o.method === 'GET' ? '' : `-X ${o.method} `}${this.base}${path}`];
						if (!(o.op.security && !o.op.security.length)) {
								lines.push(`-H "Authorization: Bearer $${secretVar}"`);
						}
      const ex = o.op.requestBody ? json(o.op.requestBody).example : null;
      if (ex) {
        lines.push('-H "Content-Type: application/json"');
        if ((o.op.parameters || []).some((p) => resolve(p).name === 'Idempotency-Key')) {
          lines.push('-H "Idempotency-Key: <unique to this send>"');
        }
        lines.push(`-d '${JSON.stringify(ex, null, 2).replace(/'/g, "'\\''")}'`);
      }
      return lines.join(' \\\n  ');
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaCenter', denmaCenter);
  Alpine.data('denmaSignupHooks', denmaSignupHooks);
  Alpine.data('denmaAPIKeys', denmaAPIKeys);
  Alpine.data('denmaAPIDocs', denmaAPIDocs);
}, { once: true });
