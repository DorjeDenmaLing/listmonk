// denma: the search box and filter builder for subscribers, lists and
// campaigns (partials/denma/search.html). The builder's conditions become
// filters in the search language (cmd/denma_search.go), added to what's in
// the box; the box is checked with the server before it searches, so a
// mistake is a message rather than an error page.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as u from '../utils.js';

// A value, quoted unless it's one plain word.
const q = (v) => {
  const s = String(v).replace(/"/g, '').trim();
  return /^[^\s"()<>=:!-][^\s"()<>=:!]*$/.test(s) && !/^(or|and|not)$/i.test(s) ? s : `"${s}"`;
};
const int = (v) => parseInt(v, 10);
const neg = (t) => `-${t}`;

const TEXT_OPS = [['has', 'contains'], ['is', 'is'], ['isnt', 'is not'], ['not', "doesn't contain"]];
const IS_OPS = [['is', 'is'], ['isnt', 'is not']];
const NUM_OPS = [['gt', 'is more than'], ['lt', 'is less than'], ['eq', 'is exactly']];
const DATE_OPS = [['last', 'in the last'], ['ago', 'more than … ago'], ['on', 'on'], ['after', 'after'], ['before', 'before']];

// Field makers. A field has ops, the value's kind (or a function of the op:
// text, number, days, date, choice, list, campaign, template or none), and
// tok(op, value, row), its filter.
const words = (label) => ({
  id: 'text', label, ops: [['has', 'contains'], ['not', "doesn't contain"]], kind: 'text', tok: (op, v) => (op === 'not' ? neg(q(v)) : q(v)),
});
const text = (id, label, extra = {}) => ({
  id,
  label,
  ops: TEXT_OPS,
  kind: 'text',
  tok: (op, v) => ({
    has: `${id}:${q(v)}`, is: `${id}=${q(v)}`, isnt: `${id}!=${q(v)}`, not: neg(`${id}:${q(v)}`),
  }[op]),
  ...extra,
});
const choice = (id, label, choices) => ({
  id, label, ops: IS_OPS, kind: 'choice', choices, tok: (op, v) => (op === 'isnt' ? '-' : '') + `${id}:${v}`,
});
const date = (id, label) => ({
  id,
  label,
  ops: DATE_OPS,
  kind: (op) => (op === 'last' || op === 'ago' ? 'days' : 'date'),
  tok: (op, v) => ({
    last: `${id}>${int(v)}d`, ago: `${id}<${int(v)}d`, on: `${id}:${v}`, after: `${id}>${v}`, before: `${id}<${v}`,
  }[op]),
});
const number = (id, label) => ({
  id, label, ops: NUM_OPS, kind: 'number', tok: (op, v) => `${id}${{ gt: '>', lt: '<', eq: '=' }[op]}${int(v)}`,
});
const tag = () => ({
  id: 'tag', label: 'Tag', ops: [['is', 'is'], ['isnt', 'is not']], kind: 'text', tok: (op, v) => (op === 'isnt' ? '-' : '') + `tag:${q(v)}`,
});
// Something done (opened, clicked, sent): a campaign (or any), or when.
const did = (id, label, ops, pick) => ({
  id,
  label,
  ops,
  any: `any ${pick}`,
  kind: (op) => (op === 'did' || op === 'didnt' ? pick : 'days'),
  tok: (op, v) => ({
    did: `${id}:${q(v)}`, didnt: neg(`${id}:${q(v)}`), last: `${id}>${int(v)}d`, notlast: neg(`${id}>${int(v)}d`),
  }[op]),
});

const FIELDS = {
  subscribers: [
    words('Name or email'),
    text('email', 'Email'),
    {
      id: 'domain', label: 'Email domain', ops: IS_OPS, kind: 'text', placeholder: 'gmail.com', tok: (op, v) => (op === 'isnt' ? '-' : '') + `domain:${q(v.replace(/^@/, ''))}`,
    },
    text('name', 'Name'),
    choice('status', 'Status', [['enabled', 'enabled'], ['blocklisted', 'blocklisted'], ['disabled', 'disabled']]),
    {
      id: 'list',
      label: 'List',
      ops: [['list', 'is subscribed to'], ['confirmed', 'has confirmed'], ['unconfirmed', "hasn't confirmed"],
        ['unsubscribed', 'unsubscribed from'], ['notlist', "isn't subscribed to"]],
      kind: 'list',
      any: 'any list',
      tok: (op, v) => (op === 'notlist' ? neg(`list:${q(v)}`) : `${op}:${q(v)}`),
    },
    date('created', 'Added'),
    date('updated', 'Last changed'),
    did('opened', 'Opened', [['did', 'opened'], ['didnt', "didn't open"], ['last', 'opened in the last'], ['notlast', "hasn't opened in the last"]], 'campaign'),
    did('clicked', 'Clicked', [['did', 'clicked in'], ['didnt', "didn't click in"], ['last', 'clicked in the last'], ['notlast', "hasn't clicked in the last"]], 'campaign'),
    {
      id: 'bounced',
      label: 'Bounced',
      ops: [['did', 'has bounced'], ['didnt', "hasn't bounced"], ['last', 'bounced in the last']],
      kind: (op) => (op === 'last' ? 'days' : 'choice'),
      choices: [['any', 'any bounce'], ['hard', 'hard'], ['soft', 'soft'], ['complaint', 'complaint (spam)']],
      tok: (op, v) => ({ did: `bounced:${v}`, didnt: neg(`bounced:${v}`), last: `bounced>${int(v)}d` }[op]),
    },
    tag(),
    {
      id: 'attr',
      label: 'Attribute',
      ops: [['is', 'is'], ['has', 'contains'], ['isnt', 'is not'], ['gt', 'is more than'], ['lt', 'is less than'], ['set', 'is set'], ['unset', 'is not set']],
      kind: (op) => (op === 'set' || op === 'unset' ? 'none' : 'text'),
      tok: (op, v, r) => {
        const key = `attr.${(r.attr || '').trim().replace(/^attr\./, '')}`;
        return {
          is: `${key}=${q(v)}`, has: `${key}:${q(v)}`, isnt: `${key}!=${q(v)}`, gt: `${key}>${q(v)}`, lt: `${key}<${q(v)}`, set: `has:${key}`, unset: neg(`has:${key}`),
        }[op];
      },
    },
  ],

  lists: [
    words('Name'),
    text('name', 'Name'),
    choice('type', 'Type', [['public', 'public'], ['private', 'private'], ['temporary', 'temporary']]),
    choice('optin', 'Opt-in', [['single', 'single'], ['double', 'double']]),
    choice('status', 'Status', [['active', 'active'], ['archived', 'archived']]),
    tag(),
    number('subscribers', 'Subscribers'),
    did('campaign', 'Campaign', [['did', 'was sent'], ['didnt', "wasn't sent"]], 'campaign'),
    {
      id: 'mailed',
      label: 'Last mailed',
      ops: [['last', 'in the last'], ['notlast', 'not in the last'], ['never', 'never']],
      kind: (op) => (op === 'never' ? 'none' : 'days'),
      tok: (op, v) => ({ last: `mailed>${int(v)}d`, notlast: neg(`mailed>${int(v)}d`), never: '-mailed:any' }[op]),
    },
    date('created', 'Created'),
    date('updated', 'Last changed'),
  ],

  campaigns: [
    words('Name or subject'),
    text('name', 'Name'),
    text('subject', 'Subject'),
    text('from', 'From address'),
    choice('status', 'Status', ['draft', 'scheduled', 'running', 'paused', 'cancelled', 'finished'].map((s) => [s, s])),
    choice('type', 'Type', [['regular', 'regular'], ['optin', 'opt-in']]),
    choice('format', 'Format', [['richtext', 'rich text'], ['html', 'HTML'], ['markdown', 'Markdown'], ['plain', 'plain text'], ['visual', 'visual']]),
    tag(),
    {
      id: 'list', label: 'List', ops: [['did', 'is sent to'], ['didnt', "isn't sent to"]], kind: 'list', any: 'any list', tok: (op, v) => (op === 'didnt' ? '-' : '') + `list:${q(v)}`,
    },
    {
      id: 'template', label: 'Template', ops: IS_OPS, kind: 'template', tok: (op, v) => (op === 'isnt' ? '-' : '') + `template:${q(v)}`,
    },
    date('started', 'Started sending'),
    date('scheduled', 'Scheduled for'),
    date('created', 'Created'),
    date('updated', 'Last changed'),
    number('sent', 'Emails sent'),
    number('opens', 'Opens'),
    number('clicks', 'Clicks'),
    number('bounces', 'Bounces'),
    choice('archive', 'In the public archive', [['yes', 'yes'], ['no', 'no']]),
  ],
};

// "Typing filters": [[examples], what they do].
const HELP = {
  subscribers: {
    rows: [
      [['email:gmail', 'email=a@b.org', 'name:smith'], 'contains, or is'],
      [['domain:gmail.com', 'domain:yahoo.*'], 'email domain'],
      [['status:blocklisted'], 'enabled, disabled, blocklisted'],
      [['list:"Weekly news"', 'list:any', 'list:none'], 'subscribed (not unsubscribed)'],
      [['confirmed:X', 'unconfirmed:X', 'unsubscribed:X'], 'X a list, or any'],
      [['created>30d', 'created<2024-01-01', 'updated:today'], '30d is 30 days ago (also h, w, m, y)'],
      [['opened:"Spring appeal"', 'opened:any', 'opened>90d'], 'a campaign, or when'],
      [['clicked:X', 'clicked:https://…'], 'a campaign, a link, or when'],
      [['bounced:hard', 'bounced:any', 'bounced>30d'], 'hard, soft, complaint, any'],
      [['tag:volunteer', 'tag:any', 'tag:none'], 'has the tag (Subscribers -> Tags)'],
      [['attr.city=Halifax', 'attr.age>=30', 'has:attr.city'], 'attributes'],
    ],
    example: ['list:Newsletter -opened>90d -status:blocklisted', "finds Newsletter subscribers who haven't opened anything in 90 days."],
    words: 'names and emails',
  },
  lists: {
    rows: [
      [['name:news', 'name="DDL Newsletter"'], 'contains, or is'],
      [['type:public', 'optin:double', 'status:archived'], 'public, private, temporary; single, double; active, archived'],
      [['tag:retreats'], 'has the tag'],
      [['subscribers>100'], 'subscribed people (not unsubscribed)'],
      [['campaign:"Spring appeal"', 'campaign:any'], 'sent to by a campaign'],
      [['mailed>90d', '-mailed>90d', '-mailed:any'], 'when a campaign last went to it'],
      [['created>30d', 'updated<2024-01-01'], '30d is 30 days ago (also h, w, m, y)'],
    ],
    example: ['status:active -mailed>180d', 'finds active lists no campaign has gone to in six months.'],
    words: 'list names',
  },
  campaigns: {
    rows: [
      [['name:appeal', 'subject:"Save the date"', 'from:info@'], 'contains, or is'],
      [['status:draft', 'type:optin', 'format:visual'], 'draft, scheduled, running, paused, cancelled, finished'],
      [['tag:retreats', 'list:"DDL Newsletter"', 'template:Newsletter'], 'a tag, list or template'],
      [['started>30d', 'scheduled>today', 'created<2024-01-01'], 'also updated; 30d is 30 days ago'],
      [['sent>1000', 'opens>100', 'clicks>10', 'bounces>5'], 'counts'],
      [['archive:yes'], 'in the public archive'],
    ],
    example: ['status:finished started>90d list:Newsletter', 'finds Newsletter campaigns sent in the last 90 days.'],
    words: 'names and subjects',
  },
};

let nextKey = 1;

function denmaSearch(kind = 'subscribers') {
  const fields = FIELDS[kind];
  const byID = Object.fromEntries(fields.map((f) => [f.id, f]));

  return {
    kind,
    fields,
    help: HELP[kind],
    lists: window._lists || [],
    campaigns: [],
    templates: [],
    loaded: false,
    match: 'all',
    rows: [],
    hasFilters: false,

    init() {
      this.hasFilters = this.filtersIn(this.$refs.search.value);
      this.onAdd();

      // Check the search with the server before searching.
      this.$refs.search.form.addEventListener('submit', (e) => {
        e.preventDefault();
        this.submit();
      });
    },

    filtersIn(s) {
      return /(^|[\s(-])[A-Za-z_][\w.-]*(:\S|[<>!]?=|[<>])/.test(s || '');
    },

    field(r) {
      return byID[r.field];
    },

    kindOf(r) {
      const k = byID[r.field].kind;
      return typeof k === 'function' ? k(r.op) : k;
    },

    onField(r) {
      const f = byID[r.field];
      r.op = f.ops[0][0];
      r.value = '';
      if (f.choices) {
        r.value = f.choices[0][0];
      } else if (f.any) {
        r.value = 'any';
      } else if (this.kindOf(r) === 'template') {
        r.value = this.templates[0]?.name || '';
      }
    },

    onAdd() {
      const f = fields[0];
      this.rows.push({
        key: nextKey++, field: f.id, op: f.ops[0][0], value: '', attr: '',
      });
    },

    onRemove(r) {
      this.rows = this.rows.filter((x) => x !== r);
      if (!this.rows.length) {
        this.onAdd();
      }
    },

    // The names to pick from, loaded when the builder first opens.
    async onToggle(e) {
      if (e.newState !== 'open' || this.loaded) {
        return;
      }
      this.loaded = true;
      const get = async (uri) => {
        try {
          const res = await fetch(`${urls.api}${uri}`, { headers: { Accept: 'application/json' } });
          const d = (await res.json()).data;
          return (Array.isArray(d) ? d : (d && d.results) || []).map((x) => ({ id: x.id, name: x.name }));
        } catch {
          return []; // Not allowed, or failed: there's nothing to pick from.
        }
      };
      if (!this.lists.length && kind !== 'lists' && fields.some((f) => f.kind === 'list')) {
        this.lists = await get('/lists?minimal=true&per_page=all');
      }
      if (kind !== 'campaigns') {
        this.campaigns = await get('/campaigns?per_page=all&no_body=true&order_by=created_at&order=desc');
      }
      if (kind === 'campaigns') {
        this.templates = await get('/templates?no_body=true');
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
      const toks = [];
      for (const r of this.rows) {
        const k = this.kindOf(r);
        const v = String(r.value ?? '').trim();
        if (k !== 'none' && v === '') {
          if (this.rows.length > 1) {
            continue; // an untouched extra row
          }
          u.toast(`${byID[r.field].label}: fill in a value.`, 'danger');
          return;
        }
        if (r.field === 'attr' && !/^[A-Za-z0-9_][A-Za-z0-9_.-]*$/.test((r.attr || '').trim().replace(/^attr\./, ''))) {
          u.toast('Attribute: give its name (letters, numbers, _ and -; a.b for one inside another).', 'danger');
          return;
        }
        toks.push(byID[r.field].tok(r.op, v, r));
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
          await api('search', `/denma/search/check?kind=${kind}&search=${encodeURIComponent(s)}`);
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
