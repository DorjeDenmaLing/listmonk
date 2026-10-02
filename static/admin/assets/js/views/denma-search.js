// denma: the subscribers search box and its filter builder
// (partials/denma/search.html). The builder's conditions become filters in
// the search language (cmd/denma_search.go), added to what's in the box; the
// box is checked with the server before it searches, so a mistake is a
// message rather than an error page.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as u from '../utils.js';

const TEXT_OPS = [['has', 'contains'], ['is', 'is'], ['isnt', 'is not'], ['not', "doesn't contain"]];
const DATE_OPS = [['last', 'in the last'], ['ago', 'more than … ago'], ['on', 'on'], ['after', 'after'], ['before', 'before']];

// The fields: [ops], the value's kind (or a function of the op), and choices.
const FIELDS = [
  { id: 'text', label: 'Name or email', ops: [['has', 'contains'], ['not', "doesn't contain"]], kind: 'text' },
  { id: 'email', label: 'Email', ops: TEXT_OPS, kind: 'text' },
  { id: 'domain', label: 'Email domain', ops: [['is', 'is'], ['isnt', 'is not']], kind: 'text', placeholder: 'gmail.com' },
  { id: 'name', label: 'Name', ops: TEXT_OPS, kind: 'text' },
  {
    id: 'status',
    label: 'Status',
    ops: [['is', 'is'], ['isnt', 'is not']],
    kind: 'choice',
    choices: [['enabled', 'enabled'], ['blocklisted', 'blocklisted'], ['disabled', 'disabled']],
  },
  {
    id: 'list',
    label: 'List',
    ops: [['list', 'is subscribed to'], ['confirmed', 'has confirmed'], ['unconfirmed', "hasn't confirmed"],
      ['unsubscribed', 'unsubscribed from'], ['notlist', "isn't subscribed to"]],
    kind: 'list',
  },
  { id: 'created', label: 'Added', ops: DATE_OPS, kind: (op) => (op === 'last' || op === 'ago' ? 'days' : 'date') },
  { id: 'updated', label: 'Last changed', ops: DATE_OPS, kind: (op) => (op === 'last' || op === 'ago' ? 'days' : 'date') },
  {
    id: 'opened',
    label: 'Opened',
    ops: [['did', 'opened'], ['didnt', "didn't open"], ['last', 'opened in the last'], ['notlast', "hasn't opened in the last"]],
    kind: (op) => (op === 'did' || op === 'didnt' ? 'campaign' : 'days'),
  },
  {
    id: 'clicked',
    label: 'Clicked',
    ops: [['did', 'clicked in'], ['didnt', "didn't click in"], ['last', 'clicked in the last'], ['notlast', "hasn't clicked in the last"]],
    kind: (op) => (op === 'did' || op === 'didnt' ? 'campaign' : 'days'),
  },
  {
    id: 'bounced',
    label: 'Bounced',
    ops: [['did', 'has bounced'], ['didnt', "hasn't bounced"], ['last', 'bounced in the last']],
    kind: (op) => (op === 'last' ? 'days' : 'choice'),
    choices: [['any', 'any bounce'], ['hard', 'hard'], ['soft', 'soft'], ['complaint', 'complaint (spam)']],
  },
  {
    id: 'attr',
    label: 'Attribute',
    ops: [['is', 'is'], ['has', 'contains'], ['isnt', 'is not'], ['gt', 'is more than'], ['lt', 'is less than'], ['set', 'is set'], ['unset', 'is not set']],
    kind: (op) => ({ set: 'none', unset: 'none', gt: 'text', lt: 'text' }[op] || 'text'),
  },
];

const byID = Object.fromEntries(FIELDS.map((f) => [f.id, f]));

// A value, quoted unless it's one plain word.
const q = (v) => {
  const s = String(v).replace(/"/g, '').trim();
  return /^[^\s"()<>=:!-][^\s"()<>=:!]*$/.test(s) && !/^(or|and|not)$/i.test(s) ? s : `"${s}"`;
};

// A row's filter, or an error.
function filter(r) {
  const v = String(r.value ?? '').trim();
  const kind = typeof byID[r.field].kind === 'function' ? byID[r.field].kind(r.op) : byID[r.field].kind;
  if (kind !== 'none' && v === '') {
    return { err: `${byID[r.field].label}: fill in a value.` };
  }
  const neg = (t) => `-${t}`;
  const textOp = (key) => ({
    has: `${key}:${q(v)}`, is: `${key}=${q(v)}`, isnt: `${key}!=${q(v)}`, not: neg(`${key}:${q(v)}`),
  }[r.op]);
  const dateOp = (key) => ({
    last: `${key}>${parseInt(v, 10)}d`, ago: `${key}<${parseInt(v, 10)}d`, on: `${key}:${v}`, after: `${key}>${v}`, before: `${key}<${v}`,
  }[r.op]);

  switch (r.field) {
    case 'text':
      return { tok: r.op === 'not' ? neg(q(v)) : q(v) };
    case 'email':
    case 'name':
      return { tok: textOp(r.field) };
    case 'domain':
      return { tok: (r.op === 'isnt' ? '-' : '') + `domain:${q(v.replace(/^@/, ''))}` };
    case 'status':
      return { tok: (r.op === 'isnt' ? '-' : '') + `status:${v}` };
    case 'list':
      return { tok: r.op === 'notlist' ? neg(`list:${q(v)}`) : `${r.op}:${q(v)}` };
    case 'created':
    case 'updated':
      return { tok: dateOp(r.field) };
    case 'opened':
    case 'clicked':
    case 'bounced':
      if (r.op === 'last' || r.op === 'notlast') {
        const t = `${r.field}>${parseInt(v, 10)}d`;
        return { tok: r.op === 'notlast' ? neg(t) : t };
      }
      return { tok: (r.op === 'didnt' ? '-' : '') + `${r.field}:${q(v)}` };
    case 'attr': {
      const a = (r.attr || '').trim().replace(/^attr\./, '');
      if (!/^[A-Za-z0-9_][A-Za-z0-9_.-]*$/.test(a)) {
        return { err: 'Attribute: give its name (letters, numbers, _ and -; a.b for one inside another).' };
      }
      const key = `attr.${a}`;
      return {
        tok: {
          is: `${key}=${q(v)}`,
          has: `${key}:${q(v)}`,
          isnt: `${key}!=${q(v)}`,
          gt: `${key}>${q(v)}`,
          lt: `${key}<${q(v)}`,
          set: `has:${key}`,
          unset: neg(`has:${key}`),
        }[r.op],
      };
    }
    default:
      return { err: 'Unknown field.' };
  }
}

let nextKey = 1;

function denmaSearch() {
  return {
    fields: FIELDS,
    lists: window._lists || [],
    campaigns: [],
    match: 'all',
    rows: [],
    hasFilters: false,

    init() {
      this.hasFilters = this.filtersIn(this.$refs.search.value);
      this.onAdd();

      // Check the search with the server before searching.
      const form = this.$refs.search.form;
      form.addEventListener('submit', (e) => {
        e.preventDefault();
        this.submit();
      });
    },

    filtersIn(s) {
      return /(^|[\s(-])[A-Za-z_][\w.-]*(:|[<>!]?=|[<>])/.test(s || '');
    },

    field(r) {
      return byID[r.field];
    },

    kind(r) {
      const k = byID[r.field].kind;
      return typeof k === 'function' ? k(r.op) : k;
    },

    newRow() {
      const f = FIELDS[0];
      return { key: nextKey++, field: f.id, op: f.ops[0][0], value: '', attr: '' };
    },

    onField(r) {
      const f = byID[r.field];
      r.op = f.ops[0][0];
      r.value = f.choices ? f.choices[0][0] : (['list'].includes(f.id) ? (this.lists[0]?.name || 'any') : '');
      if (f.id === 'opened' || f.id === 'clicked') {
        r.value = 'any';
      }
    },

    onAdd() {
      this.rows.push(this.newRow());
    },

    onRemove(r) {
      this.rows = this.rows.filter((x) => x !== r);
      if (!this.rows.length) {
        this.onAdd();
      }
    },

    async onToggle(e) {
      if (e.newState !== 'open' || this.campaigns.length) {
        return;
      }
      try {
        const res = await fetch(`${urls.api}/campaigns?per_page=all&no_body=true&order_by=created_at&order=desc`, { headers: { Accept: 'application/json' } });
        const d = (await res.json()).data;
        this.campaigns = ((d && d.results) || []).map((c) => ({ id: c.id, name: c.name }));
      } catch {
        // No campaigns to pick from; "any campaign" still works.
      }
    },

    hasSearch() {
      return this.$refs.search.value.trim() !== '';
    },

    onClear() {
      this.$refs.search.value = '';
      this.submit();
    },

    onApply() {
      // Skip untouched blank rows; build the rest.
      const toks = [];
      for (const r of this.rows) {
        const blank = this.kind(r) !== 'none' && String(r.value ?? '').trim() === '';
        if (blank && this.rows.length > 1) {
          continue;
        }
        const out = filter(r);
        if (out.err) {
          u.toast(out.err, 'danger');
          return;
        }
        toks.push(out.tok);
      }
      let add = toks.join(this.match === 'any' ? ' OR ' : ' ');
      if (this.match === 'any' && toks.length > 1) {
        add = `(${add})`;
      }
      const cur = this.$refs.search.value.trim();
      this.$refs.search.value = cur ? `${cur} ${add}` : add;
      this.submit();
    },

    async submit() {
      const s = this.$refs.search.value.trim();
      if (s) {
        try {
          await api('search', `/denma/search/check?search=${encodeURIComponent(s)}`);
        } catch {
          return; // api() showed the message
        }
      }
      const form = this.$refs.search.form;
      // Leave out an empty search, and start from the first page.
      form.querySelectorAll('input[name="page"]').forEach((el) => el.remove());
      this.$refs.search.disabled = !s;
      HTMLFormElement.prototype.submit.call(form);
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaSearch', denmaSearch);
}, { once: true });
