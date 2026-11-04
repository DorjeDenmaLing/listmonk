// denma: a save gives the version it started from; one refused because someone saved since is merged with
// theirs, asking which to keep where both changed the same thing (cmd/denma_versions.go).
// Used by views/campaign.js, views/templates.js and views/denma-automations.js.

import { api, urls } from './main.js';
import * as u from './utils.js';

// The same value, whatever its keys' order.
function canon(v) {
  if (Array.isArray(v)) {
    return `[${v.map(canon).join(',')}]`;
  }
  if (v && typeof v === 'object') {
    return `{${Object.keys(v).sort().map((k) => `${JSON.stringify(k)}:${canon(v[k])}`).join(',')}}`;
  }
  return JSON.stringify(v ?? null);
}
const same = (a, b) => canon(a) === canon(b);

const ids = (xs) => (xs || []).map((x) => (x && typeof x === 'object' ? x.id : x)).filter((x) => x).map(Number).sort((a, b) => a - b);
const minute = (t) => {
  const d = t ? new Date(t) : null;
  return d && !Number.isNaN(d.getTime()) ? `${d.toISOString().slice(0, 16)}:00Z` : null;
};
const text = (s) => s || '';
const num = (n) => Number(n) || null;

// Each kind's save, made comparable: from a page's save or a version's contents.
const NORM = {
  campaign: (p) => ({
    name: text(p.name),
    subject: text(p.subject),
    preview_text: text(p.preview_text),
    lists: ids(p.lists),
    subscriber_tags: (p.subscriber_tags || []).map(String),
    from_email: text(p.from_email),
    reply_to: text(p.reply_to),
    messenger: p.messenger || 'email',
    tags: p.tags || [],
    send_at: minute(p.send_at),
    attribs: p.attribs || {},
    template_id: p.content_type === 'visual' ? null : num(p.template_id),
    content_type: p.content_type,
    body: text(p.body),
    body_source: p.body_source || null,
    altbody: p.content_type === 'plain' ? null : (p.altbody || null),
    archive: !!p.archive,
    archive_slug: text(p.archive_slug),
    archive_template_id: p.content_type === 'visual' ? null : num(p.archive_template_id),
    archive_meta: p.archive ? (p.archive_meta || {}) : {},
    media: ids(p.media),
  }),
  template: (p) => ({
    name: text(p.name),
    type: p.type,
    subject: text(p.subject),
    body: text(p.body),
    body_source: p.body_source || null,
  }),
  automation: (p) => ({
    name: text(p.name),
    trigger_type: p.trigger_type,
    list_ids: ids(p.list_ids),
    trigger_tags: (p.trigger_tags || []).map(String),
    trigger_campaign_id: num(p.trigger_campaign_id),
    if_tags: (p.if_tags || []).map(String),
    unless_tags: (p.unless_tags || []).map(String),
    add_list_ids: ids(p.add_list_ids),
    remove_list_ids: ids(p.remove_list_ids),
    add_tags: (p.add_tags || []).map(String),
    remove_tags: (p.remove_tags || []).map(String),
    send_email: !!p.send_email,
    template_id: num(p.template_id),
    subject: text(p.subject),
    from_email: text(p.from_email),
    delay_minutes: Number(p.delay_minutes) || 0,
    each_time: !!p.each_time,
    quiet_days: Number(p.quiet_days) || 0,
    steps: p.send_email ? (p.steps || []).map((s) => ({
      delay_minutes: Number(s.delay_minutes) || 0, template_id: num(s.template_id), subject: text(s.subject),
    })) : [],
  }),
};

// What's chosen together, as people name it.
const GROUPS = {
  campaign: [
    ['Name', ['name']], ['Subject', ['subject']], ['Preview text', ['preview_text']], ['Lists', ['lists']],
    ['Tags it goes to', ['subscriber_tags']], ['From', ['from_email']], ['Reply-to', ['reply_to']], ['Messenger', ['messenger']],
    ['Tags', ['tags']], ['When to send', ['send_at']], ['Attributes', ['attribs']], ['Template', ['template_id']],
    ['Content', ['content_type', 'body', 'body_source']], ['Plain text', ['altbody']], ['Attachments', ['media']],
    ['Archive', ['archive', 'archive_slug', 'archive_template_id', 'archive_meta']],
  ],
  template: [['Name', ['name']], ['Subject', ['subject']], ['Content', ['body', 'body_source']], ['Type', ['type']]],
  automation: [
    ['Name', ['name']], ['What starts it', ['trigger_type', 'list_ids', 'trigger_tags', 'trigger_campaign_id']],
    ['Conditions', ['if_tags', 'unless_tags']], ['What it does', ['add_list_ids', 'remove_list_ids', 'add_tags', 'remove_tags']],
    ['Its e-mails', ['send_email', 'template_id', 'subject', 'from_email', 'delay_minutes', 'each_time', 'quiet_days', 'steps']],
  ],
};

const NOUN = { campaign: 'campaign', template: 'template', automation: 'automation' };
const LOADING = { campaign: 'campaigns', template: 'templates', automation: 'automation' }; // each editor's Save
const STOP = Symbol('stop'); // the save ends without an answer

// The merge of mine and theirs from base (null: unknown, so every difference is asked about):
// [merged save, the groups both changed differently].
export function denmaMerge(kind, base, mine, theirs) {
  const b = base ? NORM[kind](base) : null;
  const m = NORM[kind](mine);
  const t = NORM[kind](theirs);
  const merged = { ...mine };
  const clashes = [];
  GROUPS[kind].forEach(([label, keys]) => {
    const mineChanged = !b || keys.some((k) => !same(m[k], b[k]));
    const theirsChanged = !b || keys.some((k) => !same(t[k], b[k]));
    if (!theirsChanged || keys.every((k) => same(m[k], t[k]))) {
      return;
    }
    if (!mineChanged) {
      keys.forEach((k) => { merged[k] = t[k]; });
      return;
    }
    clashes.push({ label, keys, mine: m, theirs: t });
  });
  return [merged, clashes];
}

// "Jane, at 10:42" (or with the day, if it wasn't today).
function whoWhen(now) {
  const d = new Date(now.saved_at);
  const today = d.toDateString() === new Date().toDateString();
  const at = today ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
    : d.toLocaleString([], { day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit' });
  const who = now.saved_by ? `${now.saved_by}${now.via ? ` (through ${now.via})` : ''}` : 'Someone';
  return [who, at];
}

// A short look at a value, for the dialog.
function peek(v) {
  if (v === null || v === undefined || v === '') {
    return '(empty)';
  }
  const s = typeof v === 'string' ? v : canon(v);
  return s.length > 60 ? `${s.slice(0, 60)}…` : s;
}

// The dialog: resolves { label: 'mine' | 'theirs' } or null if cancelled.
function ask(kind, now, clashes) {
  const tpl = document.getElementById('confirm-dialog-template');
  const [who, at] = whoWhen(now);
  return new Promise((resolve) => {
    const dialog = tpl.content.firstElementChild.cloneNode(true);
    dialog.dataset.testid = 'conflict-dialog';
    dialog.querySelector('[data-confirm-message]').textContent = `${who} saved this ${NOUN[kind]} at ${at}, after you opened it. `
      + 'You both changed what\'s below: choose which to keep. Everything else is merged. '
      + 'Whichever you don\'t keep stays in the history.';
    const field = dialog.querySelector('[data-field]');
    clashes.forEach((c, i) => {
      const fs = document.createElement('fieldset');
      fs.className = 'denma-conflict';
      const lg = document.createElement('legend');
      lg.textContent = c.label;
      fs.appendChild(lg);
      const short = c.keys.length === 1 && c.label !== 'Content';
      [['mine', 'Yours'], ['theirs', `${who.split(' (')[0]}'s`]].forEach(([v, name]) => {
        const l = document.createElement('label');
        const r = document.createElement('input');
        r.type = 'radio';
        r.name = `denma-conflict-${i}`;
        r.value = v;
        r.checked = v === 'mine';
        r.dataset.choice = c.label;
        l.append(r, ` ${name}${short ? `: ${peek(c[v][c.keys[0]])}` : ''}`);
        fs.appendChild(l);
      });
      field.appendChild(fs);
    });
    let resolved = false;
    const done = (v) => {
      if (resolved) {
        return;
      }
      resolved = true;
      resolve(v);
      dialog.close();
      dialog.remove();
    };
    const ok = dialog.querySelector('[data-confirm-ok]');
    ok.textContent = 'Save';
    ok.addEventListener('click', () => {
      const out = {};
      clashes.forEach((c, i) => { out[c.label] = dialog.querySelector(`input[name="denma-conflict-${i}"]:checked`).value; });
      done(out);
    });
    dialog.querySelector('[data-confirm-cancel]').addEventListener('click', () => done(null));
    dialog.addEventListener('cancel', (e) => {
      e.preventDefault();
      done(null);
    });
    document.body.appendChild(dialog);
    dialog.showModal();
    ok.focus();
  });
}

// PUT with the version; the answer's data, or {status, message} for a refusal.
async function put(uri, data) {
  try {
    const resp = await fetch(`${urls.api}${uri}`, {
      method: 'PUT',
      body: JSON.stringify(data),
      headers: { Accept: 'application/json', 'Content-Type': 'application/json; charset=utf-8' },
    });
    const out = await resp.json().catch(() => ({}));
    return resp.ok ? { data: out.data } : { status: resp.status, message: out.message || resp.statusText };
  } catch (err) {
    return { status: 0, message: err.message };
  }
}

// kind: campaign, template or automation; version: as loaded; id(): the item's; base(): the page's save as it
// was loaded or last saved, null if unknown.
export function denmaVersions({
  kind, version, id, base,
}) {
  return {
    denmaVersion: version || 0,
    denmaSaving: false,

    // Saves over the version it started from. Merged with someone else's save, it saves the merge and reloads
    // the page to show it; the promise then doesn't resolve, nor if the person cancels.
    async denmaPut(uri, payload) {
      if (this.denmaSaving) {
        return new Promise(() => {}); // a save is under way
      }
      const store = window.Alpine.store('admin');
      this.denmaSaving = true;
      store.loading[LOADING[kind]] = true;
      let out;
      try {
        out = await this.denmaPutOnce(uri, payload);
      } finally {
        this.denmaSaving = false;
        store.loading[LOADING[kind]] = false;
      }
      return out === STOP ? new Promise(() => {}) : out;
    },

    async denmaPutOnce(uri, payload) {
      let from = base.call(this);
      let save = payload;
      let ver = this.denmaVersion;
      for (let tries = 0; tries < 3; tries += 1) {
        // eslint-disable-next-line no-await-in-loop
        const r = await put(uri, { ...save, version: ver });
        if (r.data !== undefined) {
          this.denmaVersion = (r.data && r.data.version) || ver;
          if (this.editing) {
            this.editing.changed = false;
          }
          if (tries > 0) {
            u.reload({ message: 'Saved, merged with the other changes.', variant: 'success' });
            return STOP;
          }
          return r.data;
        }
        if (r.status !== 409) {
          u.toast(r.message, 'danger');
          throw new Error(r.message);
        }
        const item = id.call(this);
        // eslint-disable-next-line no-await-in-loop
        const now = await api('versions', `/denma/versions/${kind}/${item}/now`);
        const [merged, clashes] = denmaMerge(kind, from, save, now.data);
        if (clashes.length) {
          // eslint-disable-next-line no-await-in-loop
          const choice = await ask(kind, now, clashes);
          if (!choice) {
            return STOP; // nothing saved; the page keeps the edit
          }
          const theirs = clashes.filter((c) => choice[c.label] === 'theirs');
          theirs.forEach((c) => c.keys.forEach((k) => { merged[k] = c.theirs[k]; }));
          if (theirs.length) {
            // eslint-disable-next-line no-await-in-loop
            await api('versions', `/denma/versions/${kind}/${item}/aside`, 'POST', { version: ver, data: save });
          }
        }
        ver = now.version;
        from = now.data;
        save = merged;
      }
      u.toast(`Others keep saving this ${NOUN[kind]}. Reload it, then make your change again.`, 'danger');
      throw new Error('conflict');
    },
  };
}

// ---- The history: an item's versions, what each save changed, and restoring one as a new version.

// The lines removed and added between two lists of lines, or null if they're too long to compare.
function diffLines(x, y) {
  if (x.length > 3000 || y.length > 3000) {
    return null;
  }
  const n = x.length;
  const m = y.length;
  const lcs = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let i = n - 1; i >= 0; i -= 1) {
    for (let j = m - 1; j >= 0; j -= 1) {
      lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && x[i] === y[j]) {
      i += 1;
      j += 1;
    } else if (j < m && (i === n || lcs[i][j + 1] >= lcs[i + 1][j])) {
      out.push({ add: true, text: y[j] });
      j += 1;
    } else {
      out.push({ add: false, text: x[i] });
      i += 1;
    }
  }
  return out;
}

const htmlLines = (s) => (s || '').replace(/>\s*</g, '>\n<').split('\n').map((l) => l.trim()).filter(Boolean);
const textLines = (s) => (s || '').split('\n').map((l) => l.replace(/\s+/g, ' ').trim()).filter(Boolean);

// The text HTML shows, a line for each block; parsed inert, never run.
function shownText(html) {
  const doc = new DOMParser().parseFromString(html || '', 'text/html');
  doc.querySelectorAll('head, style, script, template, noscript').forEach((e) => e.remove());
  doc.querySelectorAll('br').forEach((e) => e.replaceWith('\n'));
  doc.querySelectorAll('td, th').forEach((e) => e.append(' '));
  doc.querySelectorAll('p, div, h1, h2, h3, h4, h5, h6, li, tr, table, ul, ol, blockquote, pre, section').forEach((e) => e.append('\n'));
  return textLines(doc.body ? doc.body.textContent : '');
}

// A version's content as lines: its HTML where it's written by hand, else the text it shows.
function contentLines(kind, d) {
  if (kind === 'campaign' && ['markdown', 'plain'].includes(d.content_type)) {
    return textLines(d.body);
  }
  const byHand = kind === 'campaign' ? d.content_type === 'html' : !d.body_source;
  return byHand ? htmlLines(d.body) : shownText(d.body);
}

// The groups a list of changed fields is in; the plain text is made from the content, so it goes with it.
function groupsOf(kind, changed) {
  const gs = GROUPS[kind].filter(([, keys]) => keys.some((k) => changed(k)));
  return gs.some(([l]) => l === 'Content') ? gs.filter(([l]) => l !== 'Plain text') : gs;
}

// Fields shown by name: lists and attachments.
const NAMED = { lists: 'name', media: 'filename' };

// The names of the IDs in key, from either version.
function named(key, idList, a, b) {
  const names = {};
  [a, b].forEach((d) => (d[key] || []).forEach((x) => {
    if (x && typeof x === 'object') {
      names[x.id] = x[NAMED[key]] || `#${x.id}`;
    }
  }));
  return idList.length ? idList.map((i) => names[i] || `#${i}`).join(', ') : '(none)';
}

// What changed from version a to b, by group: [{label, before, after}] or [{label, lines}].
function changesBetween(kind, a, b) {
  const x = NORM[kind](a);
  const y = NORM[kind](b);
  return groupsOf(kind, (k) => !same(x[k], y[k])).map(([label, keys]) => {
    if (label === 'Content') {
      const lines = diffLines(contentLines(kind, x), contentLines(kind, y));
      if (!lines || lines.length) {
        return { label, lines };
      }
    }
    const show = (n) => keys.map((k) => (NAMED[k] ? named(k, n[k], a, b) : peek(n[k]))).join(' · ');
    return { label, before: show(x), after: show(y) };
  });
}

// What a version's row says it is.
function describe(kind, v, last) {
  if (v.set_aside) {
    return 'Edit not kept: someone else saved first';
  }
  if (!v.changed || v.changed.length === 0) {
    return last && !v.saved_by ? 'As it was when the history began' : 'Created';
  }
  const labels = groupsOf(kind, (k) => v.changed.includes(k)).map(([l]) => l);
  return labels.length ? `Changed: ${labels.join(', ')}` : 'Changed';
}

// The version items[i] was made from: for an edit set aside, the one it started from; else the save before it.
function baseOf(items, i) {
  const v = items[i];
  const from = v.set_aside && items.find((x) => !x.set_aside && x.version === v.version);
  return from || items.slice(i + 1).find((x) => !x.set_aside) || null;
}

// When, as people say it: "Today, 10:04", "Yesterday, 15:20", "3 Nov, 9:12", "3 Nov 2025, 9:12".
function when(t) {
  const d = new Date(t);
  const time = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  const today = new Date().setHours(0, 0, 0, 0);
  const days = Math.round((today - new Date(t).setHours(0, 0, 0, 0)) / 864e5);
  if (days === 0) {
    return `Today, ${time}`;
  }
  if (days === 1) {
    return `Yesterday, ${time}`;
  }
  const sameYear = d.getFullYear() === new Date().getFullYear();
  return `${d.toLocaleDateString([], { day: 'numeric', month: 'short', year: sameYear ? undefined : 'numeric' })}, ${time}`;
}

// A version's content in the page's own preview dialog (partials/campaign-preview.html, template-preview.html).
const PREVIEW = {
  campaign(d, title) {
    Object.assign(this.preview, {
      title, loading: true, url: `${urls.api}/campaigns/${this.form.id}/preview`, templateID: d.template_id || '',
      contentType: d.content_type, archiveMeta: '', body: d.body, inbox: null,
    });
    this.$refs.previewDialog.showModal();
    this.$nextTick(() => this.$refs.previewForm.submit());
  },
  template(d, title) {
    this.preview = {
      title, url: `${urls.api}/templates/preview`, templateType: d.type, body: d.body, loading: true,
    };
    this.$refs.previewDialog.showModal();
    this.$nextTick(() => this.$refs.previewForm.submit());
  },
};

// kind and id() as for denmaVersions; canRestore(): whether the page can save it.
export function denmaHistory({ kind, id, canRestore = () => true }) {
  const preview = PREVIEW[kind] || null;
  return {
    history: {
      items: [], loading: false, open: null, changes: null,
    },

    async denmaHistoryOpen() {
      this.denmaHistoryBack();
      this.history.loading = true;
      this.$refs.denmaHistoryDialog.showModal();
      try {
        this.history.items = await api('versions', `/denma/versions/${kind}/${id.call(this)}`);
      } finally {
        this.history.loading = false;
      }
    },

    // One version: what its save changed, with View and Restore.
    async denmaHistoryShow(i) {
      const v = this.history.items[i];
      const base = baseOf(this.history.items, i);
      const get = (x) => api('versions', `/denma/versions/${kind}/${id.call(this)}/${x.id}`);
      this.history.loading = true;
      try {
        const [one, before] = await Promise.all([get(v), base ? get(base) : null]);
        this.history.changes = before ? changesBetween(kind, before.data, { ...before.data, ...one.data }) : null;
        this.history.open = { ...v, index: i };
      } finally {
        this.history.loading = false;
      }
    },

    denmaHistoryBack() {
      this.history.open = null;
      this.history.changes = null;
    },

    denmaHistoryWhen(v) {
      return when(v.saved_at);
    },

    denmaHistoryWho(v) {
      if (!v.saved_by) {
        return 'automatic';
      }
      return v.via ? `${v.saved_by}, through ${v.via}` : v.saved_by;
    },

    denmaHistoryWhat(v, i) {
      return describe(kind, v, i === this.history.items.length - 1);
    },

    // What's there now: the newest save.
    denmaHistoryCurrent(v) {
      const now = this.history.items.find((x) => !x.set_aside);
      return !!now && now.id === v.id;
    },

    denmaHistoryCanPreview() {
      return !!preview;
    },

    denmaHistoryCanRestore(v) {
      return canRestore.call(this) && !this.denmaHistoryCurrent(v);
    },

    async denmaHistoryView(v) {
      const one = await api('versions', `/denma/versions/${kind}/${id.call(this)}/${v.id}`);
      preview.call(this, NORM[kind]({ ...(await this._denmaNow()).data, ...one.data }), `${this.denmaHistoryWhen(v)}, ${this.denmaHistoryWho(v)}`);
    },

    async _denmaNow() {
      return api('versions', `/denma/versions/${kind}/${id.call(this)}/now`);
    },

    async denmaHistoryRestore(v) {
      const at = new Date(v.saved_at).toLocaleString([], {
        day: 'numeric', month: 'short', year: 'numeric', hour: 'numeric', minute: '2-digit',
      });
      const ask = () => u.confirm(`Restore the version of ${at}? It's saved as a new version, `
        + 'so nothing is lost: what\'s there now stays in the history.');
      if (!(await (this.denmaUnsavedFirst ? this.denmaUnsavedFirst('restoring', ask) : ask()))) {
        return;
      }
      const item = id.call(this);
      const [one, now] = await Promise.all([api('versions', `/denma/versions/${kind}/${item}/${v.id}`), this._denmaNow()]);
      const save = NORM[kind]({ ...now.data, ...one.data });
      if (kind === 'campaign') {
        save.type = 'regular';
      }
      const path = { campaign: `/campaigns/${item}`, template: `/templates/${item}`, automation: `/denma/automations/${item}` }[kind];
      const r = await put(path, { ...save, version: now.version });
      if (r.data === undefined) {
        u.toast(r.status === 409 ? `Someone saved this ${NOUN[kind]} just now. Look at its history again.` : r.message, 'danger');
        return;
      }
      u.reload({ message: 'Restored: saved as a new version.', variant: 'success' });
    },
  };
}

// ---- Who else has it open, and whether it's been saved since the page loaded it.

// kind and id() as for denmaVersions. The page calls denmaEditingStart() in its init.
export function denmaEditing({ kind, id }) {
  const tab = (crypto.randomUUID && crypto.randomUUID()) || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  let timer = null;
  return {
    editing: {
      others: [], self: false, changed: false, savedBy: '', savedAt: null,
    },

    async _denmaEditingPing(leaving = false) {
      const item = id.call(this);
      if (!item) {
        return;
      }
      try {
        const resp = await fetch(`${urls.api}/denma/editing/${kind}/${item}`, {
          method: 'POST',
          keepalive: leaving,
          body: JSON.stringify({ tab, leaving }),
          headers: { Accept: 'application/json', 'Content-Type': 'application/json; charset=utf-8' },
        });
        if (leaving || !resp.ok) {
          return;
        }
        const d = (await resp.json()).data;
        const who = d.saved_by ? `${d.saved_by}${d.via ? ` (through ${d.via})` : ''}` : 'Someone';
        Object.assign(this.editing, {
          others: d.others || [], self: d.self, changed: d.version > this.denmaVersion, savedBy: who, savedAt: d.saved_at,
        });
      } catch (err) {
        // Offline for a moment: the next one tries again.
      }
    },

    denmaEditingStart() {
      this._denmaEditingPing();
      timer = setInterval(() => { if (!document.hidden) this._denmaEditingPing(); }, 30000);
      document.addEventListener('visibilitychange', () => { if (!document.hidden) this._denmaEditingPing(); });
      window.addEventListener('pagehide', () => {
        clearInterval(timer);
        this._denmaEditingPing(true);
      });
    },

    // The notice's words, or '' for none.
    denmaEditingText() {
      const e = this.editing;
      const parts = [];
      if (e.others.length) {
        const names = e.others.length > 1 ? `${e.others.slice(0, -1).join(', ')} and ${e.others[e.others.length - 1]}` : e.others[0];
        parts.push(`${names} ${e.others.length > 1 ? 'are' : 'is'} editing this too.`);
      } else if (e.self) {
        parts.push('You have this open in another window too.');
      }
      if (e.changed) {
        const at = e.savedAt ? ` at ${new Date(e.savedAt).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}` : '';
        parts.push(`${e.savedBy} saved it${at}, after you opened it.`);
        if (this.denmaDirty && this.denmaDirty()) {
          parts.push('When you save, your changes are merged with theirs.');
        }
      }
      return parts.join(' ');
    },

    // Loads what was saved, if the page has no unsaved changes.
    denmaEditingLoad() {
      u.reload();
    },
  };
}
