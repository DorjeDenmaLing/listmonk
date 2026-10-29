// denma: the import's preview (cmd/denma_importpreview.go,
// partials/denma/import.html): once a file is picked, what it holds and what
// importing it would do, made again when the file or a setting changes.
// Upload waits for it, and first adds the tags picked to the center. The file
// is sent once: the server keeps it for a few minutes, and a change of
// settings sends its token only (a request a newer one replaces is cancelled).
// Spread into the import page's component.
import { api, urls } from './main.js';
import * as u from './utils.js';

export function denmaImportPreview() {
  return {
    denmaPrev: null, // the preview, for denmaPrevKey
    denmaPrevKey: '',
    denmaPrevBusy: false,
    denmaPrevError: '',
    denmaPrevSeq: 0,
    denmaPrevTimer: null,
    denmaPrevAbort: null, // cancels the request in flight
    denmaPrevToken: '', // the server's kept copy of the file ...
    denmaPrevTokenFile: '', // ... and which file it is
    denmaAddTags: {}, // tag: true to add it to the center before importing

    denmaInitPreview() {
      this.form.denmaRunAutos = false;
      this.form.denmaKeepConsent = false;
      this.form.denmaBlockReason = '';
      this.$watch('form', () => {
        clearTimeout(this.denmaPrevTimer);
        this.denmaPrevTimer = setTimeout(() => this.denmaPreview(), 300);
      });
    },

    // What a preview depends on: the file and the settings.
    denmaKey() {
      const f = this.form;
      if (!f.file) {
        return '';
      }
      return JSON.stringify([f.file.name, f.file.size, f.file.lastModified, f.mode, f.subStatus, f.delim,
        f.lists.map((l) => l.id).sort(), f.overwriteUserInfo, f.overwriteSubStatus, f.denmaRunAutos, f.denmaKeepConsent, f.denmaBlockReason]);
    },

    async denmaPreview() {
      const key = this.denmaKey();
      if (key === this.denmaPrevKey && (this.denmaPrev || this.denmaPrevError)) {
        return;
      }
      const seq = ++this.denmaPrevSeq;
      if (this.denmaPrevAbort) {
        this.denmaPrevAbort.abort();
        this.denmaPrevAbort = null;
      }
      if (!key || this.form.delim.length !== 1) {
        Object.assign(this, { denmaPrev: null, denmaPrevKey: '', denmaPrevError: '', denmaPrevBusy: false });
        return;
      }
      this.denmaPrevBusy = true;
      const abort = new AbortController();
      this.denmaPrevAbort = abort;

      let prev = null;
      let err = '';
      try {
        let resp = await this.denmaPreviewRequest(abort.signal, true);
        if (resp.status === 410) { // no longer kept: send the file again
          resp = await this.denmaPreviewRequest(abort.signal, false);
        }
        const out = await resp.json().catch(() => ({}));
        if (resp.ok) {
          prev = out.data;
          this.denmaPrevToken = prev.token;
          this.denmaPrevTokenFile = this.denmaFileKey();
        } else {
          err = out.message || resp.statusText;
        }
      } catch (e) {
        if (e.name === 'AbortError') {
          return; // a later one's on its way
        }
        err = e.message;
      }
      if (seq !== this.denmaPrevSeq) {
        return;
      }
      Object.assign(this, { denmaPrev: prev, denmaPrevKey: key, denmaPrevError: err, denmaPrevBusy: false });
    },

    // The file as the server keeps it.
    denmaFileKey() {
      const f = this.form.file;
      return f ? JSON.stringify([f.name, f.size, f.lastModified]) : '';
    },

    // A preview's request: the settings, and the file's token if the server
    // has the file (and it's wanted), else the file.
    denmaPreviewRequest(signal, useToken) {
      const params = new FormData();
      params.set('params', JSON.stringify({
        mode: this.form.mode,
        subscription_status: this.form.subStatus,
        delim: this.form.delim,
        lists: this.form.lists.map((l) => l.id),
        overwrite_userinfo: this.form.overwriteUserInfo,
        overwrite_subscription_status: this.form.overwriteSubStatus,
        ...this.denmaImportParams(),
      }));
      if (useToken && this.denmaPrevToken && this.denmaPrevTokenFile === this.denmaFileKey()) {
        params.set('token', this.denmaPrevToken);
      } else {
        params.set('file', this.form.file);
      }
      return fetch(`${urls.api}/denma/subscribers/import/preview`, {
        method: 'POST', body: params, headers: { Accept: 'application/json' }, signal,
      });
    },

    // Whether the preview is of the file and settings as they are, and finds
    // something to import.
    denmaPreviewOK() {
      const p = this.denmaPrev;
      return !!p && !p.error && p.valid > 0 && !this.denmaPrevBusy && this.denmaPrevKey === this.denmaKey();
    },

    // The import's own options (cmd/denma_imports.go), in its params.
    denmaImportParams() {
      const sub = this.form.mode === 'subscribe';
      return {
        run_automations: sub && !!this.form.denmaRunAutos,
        keep_consent: sub && !!this.form.denmaKeepConsent,
        blocklist_reason: sub ? '' : (this.form.denmaBlockReason || ''),
      };
    },

    // What a blocklist import's reason does.
    denmaBlockReasonHint() {
      switch (this.form.denmaBlockReason) {
        case 'unsubscribed':
          return 'Recorded as their own unsubscribe: only they can subscribe again, by signing up and confirming. Dated by unsubscribed_at in the attributes, if given.';
        case 'bounced':
          return 'Marked as bounced in this center (not others), as a bounce is: an admin can still enable one on purpose. Dated by bounced_at in the attributes, if given.';
        default:
          return 'An admin can enable them again.';
      }
    },

    // The automations option's hint, with the automations the preview found.
    denmaAutosHint() {
      const names = this.denmaPrev && !this.denmaPrev.error ? this.denmaPrev.automations : null;
      if (names && names.length === 0) {
        return 'No active automations start for these lists and tags.';
      }
      const which = names ? names.join(', ') : 'automations for these lists and tags';
      return `Off: they're added quietly. On: ${which} start for them, e-mails included.`;
    },

    denmaNewTags() {
      return (this.denmaPrev?.tags || []).filter((t) => !t.exists);
    },

    denmaAddAllTags(on) {
      this.denmaNewTags().forEach((t) => { this.denmaAddTags[t.tag] = on; });
    },

    // Adds the new tags picked to the center (as Subscribers -> Tags does),
    // before the upload. False if one couldn't be.
    async denmaCreateTags() {
      for (const t of this.denmaNewTags()) {
        if (!this.denmaAddTags[t.tag]) {
          continue;
        }
        try {
          await api('tags', '/denma/tags', 'POST', { tag: t.tag });
        } catch (e) {
          return false;
        }
        t.exists = true;
      }
      return true;
    },

    denmaN(n) {
      return u.formatNumber(n || 0);
    },

    // A tag that still has an export's quote marks.
    denmaQuoted(tag) {
      return /["'`]/.test(tag);
    },

    denmaAttribsText(a) {
      return a && Object.keys(a).length ? JSON.stringify(a) : '';
    },

    // What the import does that may not be expected.
    denmaWarnings() {
      const p = this.denmaPrev;
      const f = this.form;
      if (!p || p.error) {
        return [];
      }
      const n = (x) => this.denmaN(x);
      const w = [];
      const dropped = (p.attribs || []).filter((a) => a.dropped).map((a) => a.key);
      if (dropped.length) {
        w.push(`The file gives ${dropped.join(', ')}, which the center keeps itself (its consent record and the like): they're left out.`);
        if (f.mode === 'subscribe' && dropped.some((k) => k === 'consent_at' || k === 'consent_source')) {
          w.push('To keep the file\'s consent_at and consent_source as their consent record, turn on "Keep the consent dates in the file".');
        }
      }
      if (p.no_name) {
        w.push(`${n(p.no_name)} ${p.no_name === 1 ? 'row has' : 'rows have'} no name, so they're named from their address (jane.doe@… becomes Jane Doe).`);
      }
      if (p.duplicates) {
        w.push(`${n(p.duplicates)} ${p.duplicates === 1 ? 'row repeats' : 'rows repeat'} an earlier row's address: each is imported in turn, so the last one's details win if "Overwrite user info" is on.`);
      }
      if (f.mode === 'blocklist') {
        const d = p.block_dates;
        const dated = d ? ` ${n(d.dated)} new to the center are dated by their ${d.key}${d.undated ? `, ${n(d.undated)} by today (none given, or not a date like 2019-03-04 15:22:10)` : ''}.` : '';
        if (f.denmaBlockReason === 'unsubscribed') {
          w.push(`These are recorded as having unsubscribed themselves: only they can subscribe again, by signing up and confirming.${dated}`);
        } else if (f.denmaBlockReason === 'bounced') {
          w.push(`These are marked as bounced in this center only: an admin can still enable one on purpose.${dated}`);
        } else {
          w.push('These are blocklisted as an admin\'s blocklisting is: an admin can undo it. It isn\'t recorded as their own unsubscribe.');
        }
        if (p.existing) {
          w.push(`${n(p.existing)} already in the center are only blocklisted: their names, attributes and tags aren't changed.`);
        }
        return w;
      }
      if (f.denmaRunAutos && (p.automations || []).length) {
        w.push(`"Start automations" is on: ${p.automations.join(', ')} start for up to ${n(p.addresses)} people. Their e-mails count towards the daily limit.`);
      }
      if (p.existing_with_tags && !f.overwriteUserInfo) {
        w.push(`${n(p.existing_with_tags)} already in the center have tags in the file, but won't get them: their details aren't changed unless "Overwrite user info" is on.`);
      }
      if (p.existing && f.overwriteUserInfo) {
        w.push(`"Overwrite user info" is on: ${n(p.existing)} already in the center get the file's name and attributes in place of theirs (they keep their tags and consent record).`);
      }
      (p.lists || []).forEach((l) => {
        if (l.optin === 'double' && f.subStatus === 'unconfirmed') {
          w.push(`${l.name} is double opt-in, and they're added unconfirmed with no opt-in e-mail: campaigns to it won't reach them until they confirm.`);
        }
        if (l.unsubscribed && f.overwriteSubStatus) {
          w.push(`"Overwrite subscription status" is on: ${n(l.unsubscribed)} who unsubscribed from ${l.name} would be subscribed again, but for those blocklisted.`);
        }
      });
      return w;
    },
  };
}
