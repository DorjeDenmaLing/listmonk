// DDL: the sections of a center's Analytics pages (partials/denma/analytics.html,
// cmd/denma_analytics.go), for the dashboard's date range:
//   Campaign reports    each campaign sent in the period, with unique opens and
//                       clicks, click-to-open, unsubscribes, bounces and
//                       complaints (cmd/denma_campaign_stats.go)
//   Automation reports  the same for automations' e-mails sent in the period,
//                       with a row per e-mail of a series
//                       (cmd/denma_auto_reports.go)
//   Audience growth     subscribers gained and lost, with a chart
//   Mailbox providers   open and click rates per provider, to spot one sending
//                       the newsletter to spam (its rate drops while the
//                       others hold)
// Ported from the production admin add-on (listmonk-js/admin-custom.js).
import Alpine from 'alpinejs';
import { urls } from '../main.js';
import * as s from '../denma-stats.js';

const { pct, PROVIDERS } = s;
const campaignURL = (c) => `${urls.admin}/campaigns/${encodeURIComponent(c.id)}`;

// A campaign's analytics (Campaigns -> Analytics), from the day before it
// started to tomorrow: the page reads dates in the server's time zone, and
// shows only the last 7 days without them.
const analyticsURL = (c) => {
  const day = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T00:00`;
  const from = new Date(c.started_at);
  from.setDate(from.getDate() - 1);
  const to = new Date();
  to.setDate(to.getDate() + 1);
  return `${urls.admin}/campaigns/analytics?id=${encodeURIComponent(c.id)}&from=${day(from)}&to=${day(to)}`;
};
const campaignName = (c) => c.name || c.subject || 'Untitled';

// An automation's page, and its figures on Explore from the period's start
// to tomorrow (opens and clicks come after the e-mails).
const automationURL = (id) => `${urls.admin}/automations/${encodeURIComponent(id)}`;
const exploreURL = (id, p) => {
  const day = (d) => `${s.isoDay(d)}T00:00`;
  return `${urls.admin}/campaigns/analytics?auto=${encodeURIComponent(id)}&from=${day(p.cur.start)}&to=${day(s.addDays(new Date(), 1))}`;
};
// The automation to show first (?id=, from its page).
const focusID = () => parseInt(new URLSearchParams(window.location.search).get('id'), 10) || 0;

// ---- Audience growth ----

// The figures (new, unsub, removed, active, ...) are computed on the server
// (cmd/denma_stats.go); the cells and chart are shared with the hub's
// Analytics (denma-stats.js).

// ---- Mailbox providers ----
//
// listmonk doesn't record who each campaign went to, so "received" is rebuilt
// from list memberships as listmonk picks recipients: on the campaign's lists
// when it started, subscription confirmed on double opt-in lists, not
// unsubscribed or blocklisted then. The server counts every figure in one
// request (cmd/denma_stats.go), for the latest PROVIDER_MAX_CAMPAIGNS.

const PROVIDER_MAX_CAMPAIGNS = 20;
const PROVIDER_MIN_CAMPAIGN = 20;
const FIGURES = ['received', 'opens', 'clicks'];

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
    autos: { rows: [], total: null, note: '', busy: true },
    focused: false,
    growth: { cells: s.growthPlaceholders(), note: '', busy: true },
    providers: { rows: [], all: null, note: 'Loading…', busy: true },
    seq: 0,

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
          return Promise.all([c, s.perCampaign('views', ids, from), s.perCampaign('clicks', ids, from),
            s.campaignOutcomes(s.startedIn(c, p.cur).map((x) => x.id))]);
        }), ([c, opens, clicks, outcomes]) => this.renderReports(c, p, opens, clicks, outcomes), 'campaign reports');
      }

      if (this.show.automations) {
        const q = `from=${encodeURIComponent(p.cur.start.toISOString())}&to=${encodeURIComponent(p.cur.end.toISOString())}`;
        run('autos', () => s.getJSON(`/denma/stats/automations?${q}`), (d) => this.renderAutomations(p, d), 'automation reports');
      }

      if (this.show.growth) {
        run('growth', () => this.loadGrowth(p), (g) => this.renderGrowth(p, g), 'audience growth');
      }

      if (this.show.providers) {
        this.providers.note = 'Loading…';
        run('providers', () => camps.then((c) => this.loadProviders(c, p)), (d) => this.renderProviders(d), 'mailbox providers');
      }
    },

    // ---- Campaign reports ----

    renderReports(campaigns, p, opens, clicks, outcomes) {
      const list = s.startedIn(campaigns, p.cur).sort((a, b) => new Date(b.started_at) - new Date(a.started_at));
      const tot = { sent: 0, bounces: 0, complaints: 0, delivered: 0, trackedDelivered: 0, opens: 0, clicks: 0, unsubs: 0, unsubDelivered: 0 };
      const metric = (count, delivered, digits, shown = true) => (shown ? { pct: pct(count, delivered, digits), n: count.toLocaleString() } : null);
      // Unsubscribes are counted from outcomes.since (cmd/denma_campaign_stats.go).
      const since = outcomes.since;
      const counted = (c) => since && new Date(c.started_at) >= since;
      const complaints = (n, delivered) => (n > 0 ? metric(n, delivered, 2) : { pct: '0', n: '' });

      this.reports.rows = list.map((c) => {
        const out = outcomes.byId[c.id] || { bounces: 0, complaints: 0, unsubscribes: 0 };
        const bounces = Math.min(out.bounces, c.sent);
        const delivered = c.sent - bounces;
        const tracked = s.isTracked(c);
        const o = opens[c.id] || 0;
        const k = clicks[c.id] || 0;
        tot.sent += c.sent;
        tot.bounces += bounces;
        tot.complaints += out.complaints;
        tot.delivered += delivered;
        if (tracked) {
          tot.trackedDelivered += delivered;
          tot.opens += o;
          tot.clicks += k;
        }
        if (counted(c)) {
          tot.unsubs += out.unsubscribes;
          tot.unsubDelivered += delivered;
        }
        return {
          id: c.id,
          name: campaignName(c),
          url: analyticsURL(c),
          campaignUrl: campaignURL(c),
          date: s.fmtDate(new Date(c.started_at)),
          sent: c.sent.toLocaleString(),
          delivered: pct(delivered, c.sent, 1),
          open: metric(o, delivered, 1, tracked),
          click: metric(k, delivered, 2, tracked),
          cto: tracked && o > 0 ? pct(k, o, 1) : '—',
          unsub: metric(out.unsubscribes, delivered, 2, counted(c)),
          bounces: bounces.toLocaleString(),
          complaint: complaints(out.complaints, delivered),
        };
      });

      const tracked = tot.trackedDelivered > 0;
      this.reports.total = list.length ? {
        count: list.length,
        sent: tot.sent.toLocaleString(),
        delivered: pct(tot.delivered, tot.sent, 1),
        open: metric(tot.opens, tot.trackedDelivered, 1, tracked),
        click: metric(tot.clicks, tot.trackedDelivered, 2, tracked),
        cto: tracked && tot.opens > 0 ? pct(tot.clicks, tot.opens, 1) : '—',
        unsub: metric(tot.unsubs, tot.unsubDelivered, 2, tot.unsubDelivered > 0),
        bounces: tot.bounces.toLocaleString(),
        complaint: complaints(tot.complaints, tot.delivered),
      } : null;
      const early = since && list.some((c) => !counted(c));
      this.reports.note = list.length ? 'Opens, clicks, unsubscribes and complaints are shares of delivered emails; opens and clicks count unique people per campaign. Click-to-open is the people who clicked, as a share of those who opened.' : '';
    },

    // ---- Automation reports ----

    // Each automation that sent in the period, its total and, for a series,
    // a row per e-mail; rates are shares of the people reached, opens and
    // clicks unique people (with individual tracking; else every one, as
    // shares of the e-mails delivered).
    renderAutomations(p, d) {
      const ind = d.individual_tracking;
      const off = d.tracking_disabled;
      const metric = (count, of, digits, shown = true) => (shown ? { pct: pct(count, of, digits), n: count.toLocaleString() } : null);
      const complaints = (n, of) => (n > 0 ? metric(n, of, 2) : { pct: '0', n: '' });
      const figures = (r) => {
        const bounces = Math.min(r.bounces, r.sent);
        const delivered = r.sent - bounces;
        const opened = ind ? r.opened : r.opens;
        const clicked = ind ? r.clicked : r.clicks;
        const of = ind ? Math.max(r.people - bounces, 0) : delivered;
        return {
          step: r.step,
          subject: r.subject,
          sent: r.sent.toLocaleString(),
          people: r.people.toLocaleString(),
          delivered: pct(delivered, r.sent, 1),
          open: metric(opened, of, 1, !off),
          click: metric(clicked, of, 2, !off),
          cto: !off && opened > 0 ? pct(clicked, opened, 1) : '—',
          unsub: metric(r.unsubscribes, r.people, 2),
          bounces: bounces.toLocaleString(),
          complaint: complaints(r.complaints, r.people),
        };
      };

      const groups = new Map();
      (d.results || []).forEach((r) => {
        if (!groups.has(r.automation_id)) groups.set(r.automation_id, { id: r.automation_id, name: r.name, total: null, steps: [] });
        const g = groups.get(r.automation_id);
        if (r.step === 0) g.total = r;
        else g.steps.push(r);
      });
      const focus = focusID();
      const list = [...groups.values()].filter((g) => g.total && g.total.sent > 0).sort((a, b) => b.total.sent - a.total.sent);
      const tot = { sent: 0, people: 0, opened: 0, clicked: 0, opens: 0, clicks: 0, unsubscribes: 0, bounces: 0, complaints: 0 };
      this.autos.rows = list.map((g) => {
        Object.keys(tot).forEach((k) => { tot[k] += g.total[k]; });
        const steps = g.steps.filter((r) => r.sent > 0);
        return {
          id: g.id,
          name: g.name || 'Untitled',
          url: exploreURL(g.id, p),
          autoUrl: automationURL(g.id),
          focus: g.id === focus,
          rows: [figures(g.total), ...(steps.length > 1 || (steps.length && steps[0].step > 1) ? steps.map(figures) : [])],
        };
      });
      this.autos.total = list.length ? { count: list.length, ...figures({ step: 0, ...tot }) } : null;

      const since = (k) => d.since && d.since[k] && new Date(d.since[k]) > p.cur.start ? s.fmtDate(new Date(d.since[k])) : null;
      const notes = [];
      if (list.length) {
        notes.push(ind
          ? 'For the e-mails sent in this period: opens and clicks are the people who opened or clicked them, once each, and with unsubscribes and complaints, shares of the people they were sent to.'
          : "Individual tracking is off, so opens and clicks aren't tied to people: they're every open and click in this period, as shares of the e-mails delivered.");
        if (off) notes.push('Tracking is off, so there are no opens or clicks.');
        if (since('views')) notes.push(`Opens are counted from ${since('views')}.`);
        if (since('unsubs') || since('bounces')) notes.push(`Unsubscribes and bounces are counted from ${since('unsubs') || since('bounces')}.`);
        if (this.autos.rows.some((g) => g.rows.length > 1)) notes.push('A series has a row for each of its e-mails under its total. Clicks from before e-mails were told apart count only in the total.');
      }
      this.autos.note = notes.join(' ');

      if (focus && !this.focused) {
        this.focused = true;
        this.$nextTick(() => document.getElementById(`automation-${focus}`)?.scrollIntoView({ block: 'center' }));
      }
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
      this.growth.note = `Per ${bk.unit}. Deleted contacts aren't counted. Hover over a bar for its numbers.`;
    },

    // ---- Mailbox providers ----

    async loadProviders(campaigns, p) {
      const keys = PROVIDERS.map((x) => x[0]).concat('other');
      const inRange = s.startedIn(campaigns, p.cur)
        .filter((c) => s.isTracked(c) && c.type !== 'optin')
        .sort((a, b) => new Date(b.started_at) - new Date(a.started_at));
      const list = inRange.slice(0, PROVIDER_MAX_CAMPAIGNS);
      const q = new URLSearchParams({ from: p.cur.start.toISOString(), to: p.cur.end.toISOString() });
      list.forEach((c) => q.append('id', c.id));
      const d = await s.getJSON(`/denma/stats/providers?${q}`);

      // rows[key] = { subscribers, unsub, received, opens, clicks, campaigns: [{ c, received, opens, clicks }] }
      const rows = {};
      keys.concat('all').forEach((k) => { rows[k] = { subscribers: 0, unsub: 0, received: 0, opens: 0, clicks: 0, campaigns: [] }; });
      keys.forEach((k) => {
        rows[k].subscribers = d.active[k] || 0;
        rows[k].unsub = d.unsub[k] || 0;
        rows.all.subscribers += rows[k].subscribers;
        rows.all.unsub += rows[k].unsub;
      });
      list.forEach((c) => {
        const all = { c, received: 0, opens: 0, clicks: 0 };
        keys.forEach((k) => {
          const m = { c };
          FIGURES.forEach((f) => {
            m[f] = d.campaigns[c.id][f][k] || 0;
            rows[k][f] += m[f];
            all[f] += m[f];
          });
          rows[k].campaigns.push(m);
        });
        rows.all.campaigns.push(all);
        FIGURES.forEach((f) => { rows.all[f] += all[f]; });
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
            url: analyticsURL(m.c),
            campaignUrl: campaignURL(m.c),
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
