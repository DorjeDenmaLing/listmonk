// denma: the hub's Analytics page (views/denma-hub-analytics.html): audience
// growth and mailbox providers for every center, together or one at a time,
// and each center's, for the dashboard's date range compared with the
// previous period of the same length. The figures come from
// /api/denma/hub/analytics (cmd/denma_hub_analytics.go), per center; they're
// added up here. Definitions are the centers' Analytics'
// (views/denma-analytics.js), over the period rather than per campaign.
import Alpine from 'alpinejs';
import * as s from '../denma-stats.js';

const CENTER_KEY = 'denma-hub-center';
const PROVIDER_KEYS = s.PROVIDERS.map((x) => x[0]).concat('other');
const sumOf = (m) => Object.values(m || {}).reduce((n, v) => n + v, 0);

// Adds up some centers: growth (as in denma-stats.js growthCells), days
// ({ 'YYYY-MM-DD': { new, unsub, removed } }) and provider rows ({ key: {
// subscribers, unsub, received, opens, clicks } }, with 'all').
function total(list) {
  const out = { days: {}, campaigns: 0, providers: {} };
  PROVIDER_KEYS.concat('all').forEach((k) => { out.providers[k] = { subscribers: 0, unsub: 0, received: 0, opens: 0, clicks: 0 }; });
  const g = { active: 0, added: 0, unsub: 0, removed: 0, prevAdded: 0, prevUnsub: 0, prevRemoved: 0 };

  list.forEach((c) => {
    if (!c.counted) return;
    const cur = c.cur || {};
    g.active += sumOf(c.active);
    g.added += sumOf(cur.new);
    g.unsub += sumOf(cur.unsub);
    g.removed += sumOf(cur.removed);
    g.prevAdded += (c.prev && c.prev.new) || 0;
    g.prevUnsub += (c.prev && c.prev.unsub) || 0;
    g.prevRemoved += (c.prev && c.prev.removed) || 0;
    out.campaigns += c.campaigns || 0;

    Object.entries(c.days || {}).forEach(([day, v]) => {
      const d = out.days[day] || (out.days[day] = { new: 0, unsub: 0, removed: 0 });
      Object.keys(d).forEach((k) => { d[k] += v[k] || 0; });
    });

    PROVIDER_KEYS.forEach((k) => {
      const r = out.providers[k];
      const pick = (m) => ((m || {})[k] || 0);
      const add = { subscribers: pick(c.active), unsub: pick(cur.unsub), received: pick(cur.received), opens: pick(cur.opens), clicks: pick(cur.clicks) };
      Object.keys(r).forEach((f) => {
        r[f] += add[f];
        out.providers.all[f] += add[f];
      });
    });
  });

  g.net = g.added - g.unsub - g.removed;
  g.prevNet = g.prevAdded - g.prevUnsub - g.prevRemoved;
  out.growth = g;
  return out;
}

const parseDay = (d) => {
  const [y, m, n] = d.split('-').map(Number);
  return new Date(y, m - 1, n);
};

function component() {
  let saved = '';
  try {
    saved = localStorage.getItem(CENTER_KEY) || '';
  } catch { /* storage unavailable */ }

  return {
    ...s.loadRange(),
    ranges: s.RANGES,
    center: saved,
    centers: [],
    rows: [],
    growth: { cells: s.growthPlaceholders(), note: '' },
    providers: { rows: [], all: null, note: 'Loading…' },
    busy: true,
    seq: 0,

    get selected() {
      return this.centers.find((c) => c.slug === this.center) || null;
    },

    init() {
      this.load();
    },

    onRange() {
      if (this.range !== 'custom') {
        this.load();
      } else if (!this.from) {
        this.from = s.isoDay(s.addDays(new Date(), -30));
        this.to = s.isoDay(new Date());
      }
    },

    onCenter() {
      try {
        localStorage.setItem(CENTER_KEY, this.center);
      } catch { /* storage unavailable */ }
      this.render();
      window.scrollTo({ top: 0, behavior: 'smooth' });
    },

    async load() {
      const p = s.periods(this);
      if (!p) {
        this.growth.note = 'Choose a start and end date.';
        return;
      }
      s.saveRange(this);
      this.seq += 1;
      const { seq } = this;
      this.busy = true;
      this.p = p;

      const q = new URLSearchParams({
        from: p.cur.start.toISOString(),
        to: p.cur.end.toISOString(),
        prev_from: p.prev.start.toISOString(),
        prev_to: p.prev.end.toISOString(),
        tz: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
      });
      try {
        const data = await s.getJSON(`/denma/hub/analytics?${q}`);
        if (seq !== this.seq) return;
        this.centers = data || [];
        if (this.center && !this.selected) {
          this.center = '';
        }
        this.render();
      } catch (err) {
        if (seq !== this.seq) return;
        const msg = `Couldn't load the figures (${err.message}). Try reloading the page.`;
        this.growth.note = msg;
        this.providers.note = msg;
      } finally {
        if (seq === this.seq) this.busy = false;
      }
    },

    render() {
      const list = this.selected ? [this.selected] : this.centers;
      const t = total(list);
      const { p } = this;

      // Audience growth.
      this.growth.cells = s.growthCells(t.growth);
      const bk = s.buckets(p);
      Object.entries(t.days).forEach(([day, v]) => {
        const d = parseDay(day);
        const b = bk.list.find((x) => d >= x.start && d < x.end);
        if (b) {
          b.added += v.new;
          b.unsub += v.unsub;
          b.removed += v.removed;
        }
      });
      const box = this.$refs.growthChart;
      box.replaceChildren(s.growthChart(bk, box.clientWidth));
      const days = s.periodDays(p);
      const failed = list.filter((c) => !c.counted).length;
      this.growth.note = `Per ${bk.unit}, compared with the previous ${days} day${days === 1 ? '' : 's'}`
        + `${this.selected ? '' : `, ${list.length} center${list.length === 1 ? '' : 's'} together`}.`
        + " Deleted subscribers aren't counted. Hover over a bar for its numbers."
        + (failed ? ` ${failed} center${failed === 1 ? '' : 's'} couldn't be counted (see By center).` : '');

      // Mailbox providers.
      const pr = t.providers;
      this.providers.rows = s.PROVIDERS.map((x) => [x[0], x[1], x[3]]).concat([['other', 'Other', false]]).map(([key, label, apple]) => ({
        key,
        label,
        apple: !!apple,
        title: key === 'other' ? s.OTHER_PROVIDER : '',
        warn: t.campaigns ? s.providerWarning(pr, key) : null,
        ...s.providerFigures(pr[key]),
      }));
      this.providers.all = s.providerFigures(pr.all);
      this.providers.note = t.campaigns
        ? `Over ${t.campaigns.toLocaleString()} campaign${t.campaigns === 1 ? '' : 's'} sent with open tracking in this period. `
          + 'Rates are unique people per campaign, as a share of emails received; received is estimated from list memberships when each campaign went out.'
          + " ⚠ marks an open rate under half of everyone else's (Apple left out of the comparison)."
        : 'No campaigns with open tracking were sent in this period, so there are no rates yet.';

      // By center.
      this.rows = this.centers.map((c) => {
        const x = total([c]);
        const g = x.growth;
        const all = s.providerFigures(x.providers.all);
        const ok = c.counted;
        const n = (v) => (ok ? v.toLocaleString() : '—');
        return {
          slug: c.slug,
          name: c.name,
          error: c.error,
          active: n(g.active),
          added: n(g.added),
          unsub: n(g.unsub),
          removed: n(g.removed),
          net: ok ? (g.net > 0 ? '+' : '') + g.net.toLocaleString() : '—',
          open: all.open,
          click: all.click,
        };
      });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaHubAnalytics', component);
}, { once: true });
