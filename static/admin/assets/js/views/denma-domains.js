// denma: the hub's Sending domains pages (views/denma-domains.html, a list,
// and views/denma-domain.html, one domain; cmd/denma_domains.go).
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import { centerPicker, centerTable } from '../denma-hub-ui.js';
import * as u from '../utils.js';

const STATUS_ORDER = ['failed', 'missing', 'error', 'waiting', '', 'incomplete', 'ready'];

const SES_WORDS = {
  SUCCESS: 'Verified',
  PENDING: 'Waiting for its DNS records',
  FAILED: 'Failed',
  TEMPORARY_FAILURE: "Amazon SES couldn't tell for now",
  NOT_STARTED: 'Not started',
  '': 'None',
};

const FIX_WORDS = {
  setup: 'Add it to Amazon SES',
  dkim: 'Start DKIM again',
  mail_from: 'Set it up again',
  notifications: 'Send them to the hub',
  config_set: 'Use it',
};

const RECORD_WORDS = { ok: 'In place', missing: 'Not found yet', wrong: 'Different', conflict: 'To fix' };
const RECORD_BADGES = { ok: 'status-finished', missing: 'status-scheduled', wrong: 'status-paused', conflict: 'status-cancelled' };

// How long ago an ISO time was: "4 minutes ago".
function ago(iso) {
  if (!iso) return 'Never';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  const units = [[86400, 'day'], [3600, 'hour'], [60, 'minute']];
  for (const [n, name] of units) {
    if (s >= n) {
      const v = Math.floor(s / n);
      return `${v} ${name}${v === 1 ? '' : 's'} ago`;
    }
  }
  return 'Just now';
}

const domainURL = (domain) => `${urls.admin}/domains/${encodeURIComponent(domain)}`;

// The list.
function denmaDomains() {
  const table = centerTable();
  table.tbl.status = new URLSearchParams(window.location.search).get('status') || '';

  return {
    ...table,
    ...centerPicker([{ slug: '', name: 'None yet' }]),
    centers: window._denmaCenters || [],
    center: '',
    add: { domain: '', mail_from: '' },
    mailFromEdited: false,

    // Rows for centerTable: name is the domain, slug what's found, state the status.
    rows: (window._denmaDomains || []).map((d) => {
      const names = d.centers.map((c) => c.name);
      return {
        ...d,
        name: d.domain,
        slug: `${d.domain} ${names.join(' ')} ${d.centers.map((c) => c.slug).join(' ')}`.toLowerCase(),
        state: d.checked_at ? d.state.status : '',
        centerNames: names.length > 3 ? `${names.slice(0, 3).join(', ')} and ${names.length - 3} more` : names.join(', '),
        checkedText: d.checked_at ? ago(d.checked_at) : 'Not yet',
        sort: {
          name: d.domain,
          centers: names[0] ? names[0].toLowerCase() : null,
          status: STATUS_ORDER.indexOf(d.checked_at ? d.state.status : ''),
          checked: d.checked_at,
        },
      };
    }),

    url: domainURL,

    // What's shown, e.g. "1–50 of 300 domains".
    tblInfo() {
      const n = this.tblRows().length;
      const of = n < this.rows.length ? ` (of ${this.rows.length})` : '';
      if (n === 0) {
        return this.rows.length ? `No domain matches${of}.` : '';
      }
      const page = Math.min(this.tbl.page, this.tblPages());
      const from = (page - 1) * this.tbl.perPage + 1;
      const to = Math.min(n, page * this.tbl.perPage);
      const range = n > this.tbl.perPage ? `${from}–${to} of ` : '';
      return `${range}${n.toLocaleString()} domain${n === 1 ? '' : 's'}${of}`;
    },

    onAdd() {
      this.add = { domain: '', mail_from: '' };
      this.mailFromEdited = false;
      this.center = '';
      this.$refs.addDialog.showModal();
      this.$nextTick(() => this.$refs.addDomain.focus());
    },

    onDomain() {
      if (!this.mailFromEdited) {
        const d = this.add.domain.trim().toLowerCase();
        this.add.mail_from = d ? `bounce.${d}` : '';
      }
    },

    onCenter() {},

    async onSubmitAdd() {
      const data = await api('domains', '/denma/domains', 'POST', {
        domain: this.add.domain,
        mail_from: this.add.mail_from,
        centers: this.center ? [this.center] : [],
      });
      this.$refs.addDialog.close();
      const toast = data.warning
        ? { message: data.warning, variant: 'danger' }
        : { message: `Added ${data.domain.domain}. Send its DNS records to the center.`, variant: 'success' };
      u.redirect(domainURL(data.domain.domain), toast);
    },
  };
}

// One domain.
function denmaDomain() {
  return {
    ...centerPicker([{ slug: '', name: 'Choose a center' }]),
    d: window._denmaDomain,
    ref: window._denmaDomainRef || {},
    centers: [],
    center: '',
    removeSES: false,
    mailFrom: '',

    init() {
      this.setCenters();
    },

    // The centers it isn't for yet, for the picker.
    setCenters() {
      const on = new Set(this.d.centers.map((c) => c.slug));
      this.centers = (window._denmaCenters || []).filter((c) => !on.has(c.slug));
    },

    update(data) {
      this.d = data.domain;
      this.setCenters();
      if (data.warning) {
        u.toast(data.warning, 'danger', 8000);
      }
    },

    sesWord(s) {
      return SES_WORDS[s || ''] || s;
    },

    fixWords(f) {
      return FIX_WORDS[f] || f;
    },

    recordWords(r) {
      if (r.ses) return 'Verified by Amazon SES';
      return RECORD_WORDS[r.status] || r.status;
    },

    recordBadge(r) {
      return RECORD_BADGES[r.status] || '';
    },

    notifWords() {
      const s = this.d.state.ses;
      if (!s.bounce_topic && !s.complaint_topic) return 'Not sent anywhere';
      const topic = (s.bounce_topic || '').split(':').pop();
      const same = this.ref.topic && s.bounce_topic === this.ref.topic && s.complaint_topic === this.ref.topic;
      return `${same ? 'To the hub' : 'To'} (${topic}${s.complaint_topic !== s.bounce_topic ? ', complaints elsewhere' : ''})${s.headers ? ', with their headers' : ', without their headers'}`;
    },

    checkedText() {
      return ago(this.d.checked_at);
    },

    isSender(c) {
      return this.d.senders.some((s) => s.slug === c.slug);
    },

    // The records still to add or change.
    needed() {
      return (this.d.state.records || []).filter((r) => r.status !== 'ok' && r.value);
    },

    copy(text) {
      u.copyToClipboard(text);
    },

    // The records still needed, as CSV to send to the center: a header row,
    // then one row a record.
    copyAll() {
      const cell = (v) => (/[",\r\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v);
      const rows = [['Type', 'Host', 'Name', 'Value']]
        .concat(this.needed().map((r) => [r.type, r.host, r.name, r.value]));
      u.copyToClipboard(rows.map((r) => r.map(cell).join(',')).join('\r\n'));
    },

    async onCheck() {
      this.update(await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}/check`, 'POST'));
      u.toast(`Checked ${this.d.domain}: ${this.d.label}.`, 'success');
    },

    async onFix(action) {
      if (action === 'dkim' && !(await u.confirm(`Start DKIM again for ${this.d.domain}? Amazon SES may give it new DKIM records, to publish in place of the old ones; it then looks for them for another 72 hours.`))) {
        return;
      }
      this.update(await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}/fix`, 'POST', { action }));
      u.toast('Done.', 'success');
    },

    onMailFrom() {
      this.mailFrom = this.d.state.ses.mail_from || this.d.mail_from;
      this.$refs.mailFromDialog.showModal();
    },

    async onSubmitMailFrom() {
      const data = await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}/fix`, 'POST', { action: 'mail_from', mail_from: this.mailFrom });
      this.$refs.mailFromDialog.close();
      this.update(data);
      u.toast(`The bounce subdomain is ${this.d.mail_from}. Send its records to the center.`, 'success');
    },

    // Adding a center from the picker.
    async onCenter() {
      const slug = this.center;
      this.center = '';
      if (!slug) return;
      this.update(await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}/centers`, 'POST', { slug }));
    },

    async onRemoveCenter(c) {
      if (!(await u.confirm(`Stop ${c.name} sending from ${this.d.domain}? Its campaigns and automations at it won't save or start.`))) {
        return;
      }
      this.update(await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}/centers/${encodeURIComponent(c.slug)}`, 'DELETE'));
    },

    onRemove() {
      this.removeSES = false;
      this.$refs.removeDialog.showModal();
    },

    async onSubmitRemove() {
      await api('domain', `/denma/domains/${encodeURIComponent(this.d.domain)}${this.removeSES ? '?ses=true' : ''}`, 'DELETE');
      this.$refs.removeDialog.close();
      u.redirect(`${urls.admin}/domains`, { message: `Removed ${this.d.domain}.`, variant: 'success' });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaDomains', denmaDomains);
  Alpine.data('denmaDomain', denmaDomain);
}, { once: true });
