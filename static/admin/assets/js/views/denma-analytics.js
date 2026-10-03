// DDL: three sections above listmonk's own charts on Campaigns -> Analytics
// (partials/denma/analytics.html), for the dashboard's date range:
//   Campaign reports   each campaign sent in the period, with unique opens and
//                      clicks as a share of delivered emails
//   Audience growth    subscribers gained and lost, with a chart
//   Mailbox providers  open and click rates per provider, to spot one sending
//                      the newsletter to spam (its rate drops while the others
//                      hold)
// Ported from the production admin add-on (listmonk-js/admin-custom.js).
import Alpine from 'alpinejs';
import { urls } from '../main.js';
import * as s from '../denma-stats.js';

const { pct, PROVIDERS } = s;
const campaignURL = (c) => `${urls.admin}/campaigns/${encodeURIComponent(c.id)}`;
const campaignName = (c) => c.name || c.subject || 'Untitled';

// ---- Audience growth ----

// The figures (new, unsub, removed, active, ...) are computed on the server
// (cmd/denma_stats.go); the cells and chart are shared with the hub's
// Analytics (denma-stats.js).

// ---- Mailbox providers ----
//
// listmonk doesn't record who each campaign went to, so "received" is rebuilt
// from list memberships as listmonk picks recipients: on the campaign's lists
// when it started, subscription confirmed on double opt-in lists, not
// unsubscribed or blocklisted then. Each campaign costs 15 small counts (3
// figures x 5 groups), so only the latest PROVIDER_MAX_CAMPAIGNS are used.

const PROVIDER_MAX_CAMPAIGNS = 20;
const PROVIDER_CONCURRENCY = 6;
const PROVIDER_MIN_CAMPAIGN = 20;
const FIGURES = ['received', 'opens', 'clicks'];

// A provider's addresses, as a search (cmd/denma_search.go).
function providerSearch(key) {
  if (key === 'all') return '';
  const pr = PROVIDERS.find((x) => x[0] === key);
  return pr[2].map((d) => `domain:${d}`).join(' OR ');
}

// Runs at most `n` requests at once.
function limiter(n) {
  let active = 0;
  const queue = [];
  const next = () => {
    if (active >= n || !queue.length) return;
    active += 1;
    const job = queue.shift();
    job.fn().then(job.resolve, job.reject).then(() => { active -= 1; next(); });
  };
  return (fn) => new Promise((resolve, reject) => { queue.push({ fn, resolve, reject }); next(); });
}
const providerQueue = limiter(PROVIDER_CONCURRENCY);
const countWhere = (metric, opts) => providerQueue(() => s.subscriberStat(metric, opts)).then((d) => (d && d.total) || 0);

const atCampaign = (id) => (x) => x.campaigns.find((y) => y.c.id === id) || { received: 0, opens: 0 };

function campaignWarning(rows, key, m) {
  if (key === 'apple') return false;
  return s.lowVersus(m, s.providerBaseline(rows, key, atCampaign(m.c.id)), PROVIDER_MIN_CAMPAIGN);
}

// Why a provider is flagged, or null: over the period (denma-stats.js), or on
// its latest campaign. Apple is never flagged.
function providerWarning(rows, key) {
  const period = s.providerWarning(rows, key);
  if (period || key === 'apple' || key === 'all') return period;
  const latest = rows[key].campaigns.find((m) => m.received >= PROVIDER_MIN_CAMPAIGN);
  if (latest && campaignWarning(rows, key, latest)) {
    return `Open rate on the latest campaign (${latest.c.name || 'Untitled'}) was less than half of everyone else's.`;
  }
  return null;
}

function component(opts) {
  return {
    ...s.loadRange(),
    ranges: s.RANGES,
    show: opts,

    reports: { rows: [], total: null, note: '', busy: true },
    growth: { cells: s.growthPlaceholders(), note: '', busy: true },
    providers: { rows: [], all: null, note: 'Loading…', busy: true },
    seq: 0,
    providerCache: {},

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

    load() {
      const p = s.periods(this);
      if (!p) {
        this.reports.note = 'Choose a start and end date.';
        return;
      }
      s.saveRange(this);
      this.seq += 1;
      const { seq } = this;
      const current = () => seq === this.seq;
      const camps = s.fetchCampaigns();
      camps.catch(() => {});

      const run = (section, work, render, what) => {
        this[section].busy = true;
        work()
          .then((d) => { if (current()) render(d); })
          .catch((err) => { if (current()) this[section].note = `Couldn't load ${what} (${err.message}).`; })
          .finally(() => { if (current()) this[section].busy = false; });
      };

      if (this.show.reports) {
        run('reports', () => camps.then((c) => {
          const tracked = s.startedIn(c, p.cur).filter(s.isTracked);
          const ids = tracked.map((x) => x.id);
          const from = tracked.length ? new Date(Math.min(...tracked.map((x) => new Date(x.started_at)))) : p.cur.start;
          return Promise.all([c, s.perCampaign('views', ids, from), s.perCampaign('clicks', ids, from)]);
        }), ([c, opens, clicks]) => this.renderReports(c, p, opens, clicks), 'campaign reports');
      }

      if (this.show.growth) {
        run('growth', () => this.loadGrowth(p), (g) => this.renderGrowth(p, g), 'audience growth');
      }

      if (this.show.providers) {
        this.providers.note = 'Loading…';
        run('providers', () => camps.then((c) => this.loadProviders(c, p, current)), (d) => this.renderProviders(d), 'mailbox providers');
      }
    },

    // ---- Campaign reports ----

    renderReports(campaigns, p, opens, clicks) {
      const list = s.startedIn(campaigns, p.cur).sort((a, b) => new Date(b.started_at) - new Date(a.started_at));
      const tot = { sent: 0, bounces: 0, delivered: 0, trackedDelivered: 0, opens: 0, clicks: 0 };
      const metric = (count, delivered, digits, tracked) => (tracked ? { pct: pct(count, delivered, digits), n: count.toLocaleString() } : null);

      this.reports.rows = list.map((c) => {
        const bounces = Math.min(c.bounces || 0, c.sent);
        const delivered = c.sent - bounces;
        const tracked = s.isTracked(c);
        const o = opens[c.id] || 0;
        const k = clicks[c.id] || 0;
        tot.sent += c.sent;
        tot.bounces += bounces;
        tot.delivered += delivered;
        if (tracked) {
          tot.trackedDelivered += delivered;
          tot.opens += o;
          tot.clicks += k;
        }
        return {
          id: c.id,
          name: campaignName(c),
          url: campaignURL(c),
          date: s.fmtDate(new Date(c.started_at)),
          sent: c.sent.toLocaleString(),
          delivered: pct(delivered, c.sent, 1),
          open: metric(o, delivered, 1, tracked),
          click: metric(k, delivered, 2, tracked),
          bounces: bounces.toLocaleString(),
        };
      });

      const tracked = tot.trackedDelivered > 0;
      this.reports.total = list.length ? {
        count: list.length,
        sent: tot.sent.toLocaleString(),
        delivered: pct(tot.delivered, tot.sent, 1),
        open: tracked ? { pct: pct(tot.opens, tot.trackedDelivered, 1), n: tot.opens.toLocaleString() } : null,
        click: tracked ? { pct: pct(tot.clicks, tot.trackedDelivered, 2), n: tot.clicks.toLocaleString() } : null,
        bounces: tot.bounces.toLocaleString(),
      } : null;
      this.reports.note = list.length ? 'Opens and clicks count unique people per campaign, as a share of delivered emails.' : '';
    },

    // ---- Audience growth ----

    async loadGrowth(p) {
      const q = (metric, period, all) => s.subscriberStat(metric, { period }, all);
      const r = await Promise.all([
        q('active'), q('new', p.cur, true), q('unsub', p.cur, true), q('removed', p.cur, true),
        q('new', p.prev), q('unsub', p.prev), q('removed', p.prev), s.fetchBounces(p.cur.start),
      ]);
      // Each removed subscriber counts on the day of their first bounce in the period.
      const removedIds = new Set((r[3].results || []).map((x) => x.id));
      const firstBounce = {};
      r[7].forEach((b) => {
        const t = new Date(b.created_at);
        if (removedIds.has(b.subscriber_id) && t >= p.cur.start && t < p.cur.end
          && (!firstBounce[b.subscriber_id] || t < firstBounce[b.subscriber_id])) {
          firstBounce[b.subscriber_id] = t;
        }
      });
      const g = {
        active: r[0].total || 0,
        added: r[1].total || 0,
        addedRows: r[1].results || [],
        unsub: r[2].total || 0,
        unsubRows: r[2].results || [],
        removed: r[3].total || 0,
        removedDates: Object.values(firstBounce),
        prevAdded: r[4].total || 0,
        prevUnsub: r[5].total || 0,
        prevRemoved: r[6].total || 0,
      };
      g.net = g.added - g.unsub - g.removed;
      g.prevNet = g.prevAdded - g.prevUnsub - g.prevRemoved;
      return g;
    },

    renderGrowth(p, g) {
      this.growth.cells = s.growthCells(g);

      const bk = s.buckets(p);
      const place = (date, key) => {
        const t = new Date(date);
        const b = bk.list.find((x) => t >= x.start && t < x.end);
        if (b) b[key] += 1;
      };
      g.addedRows.forEach((x) => place(x.created_at, 'added'));
      g.unsubRows.forEach((x) => {
        let latest = null;
        (x.lists || []).forEach((l) => {
          if (l.subscription_status === 'unsubscribed' && l.subscription_updated_at
            && (!latest || l.subscription_updated_at > latest)) latest = l.subscription_updated_at;
        });
        if (latest) place(latest, 'unsub');
      });
      g.removedDates.forEach((d) => place(d, 'removed'));

      const box = this.$refs.growthChart;
      box.replaceChildren(s.growthChart(bk, box.clientWidth));
      this.growth.note = `Per ${bk.unit}. Deleted subscribers aren't counted. Hover over a bar for its numbers.`;
    },

    // ---- Mailbox providers ----

    campaignCount(c, figure, key) {
      const k = `${c.id}|${figure}|${key}`;
      if (!this.providerCache[k]) {
        this.providerCache[k] = countWhere(figure, { campaign: c.id, search: providerSearch(key) });
        this.providerCache[k].catch(() => { delete this.providerCache[k]; });
      }
      return this.providerCache[k];
    },

    async loadProviders(campaigns, p, current) {
      const keys = PROVIDERS.map((x) => x[0]).concat('all');
      const inRange = s.startedIn(campaigns, p.cur)
        .filter((c) => s.isTracked(c) && c.type !== 'optin')
        .sort((a, b) => new Date(b.started_at) - new Date(a.started_at));
      const list = inRange.slice(0, PROVIDER_MAX_CAMPAIGNS);

      let jobs = 0;
      let done = 0;
      const track = (pr) => {
        jobs += 1;
        pr.then(() => {
          done += 1;
          if (current() && done < jobs) this.providers.note = `Loading… ${Math.round(done / jobs * 100)}%`;
        }, () => {});
        return pr;
      };
      const perGroup = keys.map((k) => Promise.all([
        track(countWhere('active', { search: providerSearch(k) })),
        track(countWhere('unsub', { period: p.cur, search: providerSearch(k) })),
      ]));
      const perCampaign = list.map((c) => Promise.all(keys.map((k) => Promise.all(FIGURES.map((f) => track(this.campaignCount(c, f, k)))))));
      const [groups, camps] = await Promise.all([Promise.all(perGroup), Promise.all(perCampaign)]);

      // rows[key] = { subscribers, unsub, received, opens, clicks, campaigns: [{ c, received, opens, clicks }] }
      const rows = {};
      keys.concat('other').forEach((k) => { rows[k] = { subscribers: 0, unsub: 0, received: 0, opens: 0, clicks: 0, campaigns: [] }; });
      keys.forEach((k, i) => { [rows[k].subscribers, rows[k].unsub] = groups[i]; });
      list.forEach((c, ci) => {
        const other = { c, received: 0, opens: 0, clicks: 0 };
        keys.forEach((k, ki) => {
          const v = camps[ci][ki];
          const m = { c, received: v[0], opens: v[1], clicks: v[2] };
          rows[k].campaigns.push(m);
          FIGURES.forEach((f) => {
            rows[k][f] += m[f];
            other[f] += k === 'all' ? m[f] : -m[f];
          });
        });
        rows.other.campaigns.push(other);
        FIGURES.forEach((f) => { rows.other[f] += other[f]; });
      });
      ['subscribers', 'unsub'].forEach((f) => {
        rows.other[f] = rows.all[f] - PROVIDERS.reduce((n, x) => n + rows[x[0]][f], 0);
      });
      return { rows, used: list.length, total: inRange.length };
    },

    renderProviders({ rows, used, total }) {
      this.providers.rows = PROVIDERS.map((x) => [x[0], x[1], x[3]]).concat([['other', 'Other', false]]).map(([key, label, apple]) => ({
        key,
        label,
        apple: !!apple,
        title: key === 'other' ? s.OTHER_PROVIDER : '',
        warn: used ? providerWarning(rows, key) : null,
        ...s.providerFigures(rows[key]),
        detail: rows[key].campaigns.filter((m) => m.received > 0).map((m) => {
          const rest = s.providerBaseline(rows, key, atCampaign(m.c.id));
          return {
            id: m.c.id,
            name: campaignName(m.c),
            url: campaignURL(m.c),
            date: s.fmtDate(new Date(m.c.started_at)),
            warn: campaignWarning(rows, key, m),
            received: m.received.toLocaleString(),
            open: pct(m.opens, m.received, 1),
            rest: pct(rest.opens, rest.received, 1),
            click: pct(m.clicks, m.received, 2),
          };
        }),
        expanded: false,
      }));
      this.providers.all = s.providerFigures(rows.all);

      if (!used) {
        this.providers.note = 'No campaigns with open tracking were sent in this period, so there are no rates yet.';
      } else {
        this.providers.note = 'Rates are per campaign, as a share of emails received; received is estimated from list memberships when each campaign went out.'
          + " ⚠ marks an open rate under half of everyone else's (Apple left out of the comparison). Select a provider to see its campaigns."
          + (total > used ? ` Based on the latest ${used} of ${total} campaigns in this period.` : '');
      }
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaAnalytics', component);
});
