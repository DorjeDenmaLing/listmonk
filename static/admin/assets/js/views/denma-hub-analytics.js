// denma: the hub's Analytics page (views/denma-hub-analytics.html): audience
// growth and mailbox providers for every center, together or one at a time,
// and each center's, for the dashboard's date range compared with the
// previous period of the same length. The figures come from
// /api/denma/hub/analytics (cmd/denma_hub_analytics.go): each center's totals,
// and the full figures of the one chosen or of all of them, added up there.
// Definitions are the centers' Analytics' (views/denma-analytics.js), over the
// period rather than per campaign.
import Alpine from 'alpinejs';
import * as s from '../denma-stats.js';
import { centerPicker, centerTable, centerState } from '../denma-hub-ui.js';

const CENTER_KEY = 'denma-hub-center';
const PROVIDER_KEYS = s.PROVIDERS.map((x) => x[0]).concat('other');
const sumOf = (m) => Object.values(m || {}).reduce((n, v) => n + v, 0);

// The figures shown, from the API's total: growth (as in denma-stats.js
// growthCells), days ({ 'YYYY-MM-DD': { new, unsub, removed } }) and provider
// rows ({ key: { subscribers, unsub, received, opens, clicks } }, with 'all').
function figures(t) {
  const cur = t.cur || {};
  const prev = t.prev || {};
  const g = {
    active: sumOf(t.active),
    added: sumOf(cur.new),
    unsub: sumOf(cur.unsub),
    removed: sumOf(cur.removed),
    prevAdded: prev.new || 0,
    prevUnsub: prev.unsub || 0,
    prevRemoved: prev.removed || 0,
  };
  g.net = g.added - g.unsub - g.removed;
  g.prevNet = g.prevAdded - g.prevUnsub - g.prevRemoved;

  const providers = {};
  const all = { subscribers: 0, unsub: 0, received: 0, opens: 0, clicks: 0 };
  PROVIDER_KEYS.forEach((k) => {
    const pick = (m) => ((m || {})[k] || 0);
    const r = { subscribers: pick(t.active), unsub: pick(cur.unsub), received: pick(cur.received), opens: pick(cur.opens), clicks: pick(cur.clicks) };
    Object.keys(all).forEach((f) => { all[f] += r[f]; });
    providers[k] = r;
  });
  providers.all = all;

  return { growth: g, days: t.days || {}, campaigns: t.campaigns || 0, providers };
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
    ...centerPicker([{ slug: '', name: 'All centers' }]),
    ...centerTable(),
    ranges: s.RANGES,
    center: saved,
    centers: [],
    total: null,
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
      this.load();
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

      const q = new URLSearchParams({
        from: p.cur.start.toISOString(),
        to: p.cur.end.toISOString(),
        prev_from: p.prev.start.toISOString(),
        prev_to: p.prev.end.toISOString(),
        tz: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
        center: this.center,
      });
      try {
        const data = await s.getJSON(`/denma/hub/analytics?${q}`);
        if (seq !== this.seq) return;
        this.p = p;
        this.centers = data.centers || [];
        this.total = data.total;
        // A center no longer there: all of them.
        this.center = data.center;
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
      const t = figures(this.total || {});
      const { p, selected } = this;

      // Audience growth.
      this.growth.cells = s.growthCells(t.growth);
      const bk = s.buckets(p);
      Object.entries(t.days).forEach(([day, v]) => {
        const d = parseDay(day);
        const b = bk.list.find((x) => d >= x.start && d < x.end);
        if (b) {
          b.added += v.new || 0;
          b.unsub += v.unsub || 0;
          b.removed += v.removed || 0;
        }
      });
      const box = this.$refs.growthChart;
      box.replaceChildren(s.growthChart(bk, box.clientWidth));
      const days = s.periodDays(p);
      const n = this.centers.length;
      const failed = (selected ? [selected] : this.centers).filter((c) => !c.counted).length;
      this.growth.note = `Per ${bk.unit}, compared with the previous ${days} day${days === 1 ? '' : 's'}`
        + `${selected ? '' : `, ${n} center${n === 1 ? '' : 's'} together`}.`
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
        const ok = c.counted;
        const net = c.new - c.unsub - c.removed;
        const all = s.providerFigures({ subscribers: c.active, unsub: c.unsub, received: c.received, opens: c.opens, clicks: c.clicks });
        const fmt = (v) => (ok ? v.toLocaleString() : '—');
        const rate = (v) => (ok && c.received > 0 ? v / c.received : null);
        return {
          slug: c.slug,
          name: c.name,
          error: c.error,
          state: centerState(c),
          sort: {
            name: c.name.toLowerCase(),
            active: ok ? c.active : null,
            added: ok ? c.new : null,
            unsub: ok ? c.unsub : null,
            removed: ok ? c.removed : null,
            net: ok ? net : null,
            open: rate(c.opens),
            click: rate(c.clicks),
          },
          active: fmt(c.active),
          added: fmt(c.new),
          unsub: fmt(c.unsub),
          removed: fmt(c.removed),
          net: ok ? (net > 0 ? '+' : '') + net.toLocaleString() : '—',
          open: ok ? all.open : null,
          click: ok ? all.click : null,
        };
      });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaHubAnalytics', component);
}, { once: true });
