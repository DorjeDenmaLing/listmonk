// denma: the hub's dashboard (partials/denma/hub.html): every center's
// sending, all together or one at a time, for a date range compared with the
// previous period of the same length, and the list of centers. The figures
// come from /api/denma/hub/stats (cmd/denma_hub.go), per center; they're
// added up here, so rates for all centers weigh each center by its sending.
// Definitions are the centers' dashboards' (views/denma-dashboard.js).
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as s from '../denma-stats.js';
import { centerPicker, centerTable, centerState } from '../denma-hub-ui.js';
import * as u from '../utils.js';

// [key, label, format, higher is good, description]
const METRICS = [
  ['sends', 'Total sends', 'count', true, 'Emails sent by campaigns that started in this period.'],
  ['delivery', 'Delivery rate', 'pct1', true, "Sent emails that didn't bounce."],
  ['open', 'Open rate', 'pct1', true, 'Unique people who opened, as a share of delivered emails (per campaign).'],
  ['click', 'Click rate', 'pct2', true, 'Unique people who clicked a link, as a share of delivered emails (per campaign).'],
  ['unsub', 'Unsubscribed rate', 'pct2', false, 'People who unsubscribed in this period (not bounce removals), as a share of delivered emails.'],
  ['newSubs', 'New subscribers', 'count', true, 'Subscribers added in this period.'],
];

const CENTER_KEY = 'denma-hub-center';
const KEYS = ['campaigns', 'sends', 'camp_bounces', 'tracked_delivered', 'opens', 'clicks', 'unsubs', 'hard', 'soft', 'complaint', 'new_subscribers'];

const placeholders = (list) => list.map((m) => ({ key: m[0], label: m[1], help: m[4] || m[3], value: '…', change: null, health: null }));

// Adds up the periods of some centers.
function total(list, k) {
  const out = Object.fromEntries(KEYS.map((x) => [x, 0]));
  list.forEach((c) => {
    if (c[k]) {
      KEYS.forEach((x) => { out[x] += c[k][x] || 0; });
    }
  });
  return out;
}

// The figures shown, from a (summed) period.
function figures(p) {
  const rate = (n, d) => (d > 0 ? n / d : null);
  const delivered = p.sends - p.camp_bounces;
  return {
    campaigns: p.campaigns,
    sends: p.sends,
    delivery: rate(delivered, p.sends),
    open: rate(p.opens, p.tracked_delivered),
    click: rate(p.clicks, p.tracked_delivered),
    unsub: rate(p.unsubs, delivered),
    newSubs: p.new_subscribers,
    hard: p.hard,
    soft: p.soft,
    complaint: p.complaint,
    hardRate: rate(p.hard, p.sends),
    complaintRate: rate(p.complaint, p.sends),
  };
}

const HEALTH_CLASS = { warning: 'denma-warn', danger: 'text-danger' };

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
    rows: [],
    perf: { cells: placeholders(METRICS) },
    bounce: { cells: placeholders(s.BOUNCE_METRICS) },
    note: '',
    busy: true,
    seq: 0,

    get selected() {
      return this.centers.find((c) => c.slug === this.center) || null;
    },

    init() {
      this.load();
    },

    openURL(slug) {
      return `${urls.admin}/centers/${encodeURIComponent(slug)}/open`;
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
        this.note = 'Choose a start and end date.';
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
      });
      try {
        const data = await s.getJSON(`/denma/hub/stats?${q}`);
        if (seq !== this.seq) return;
        this.centers = data || [];
        if (this.center && !this.selected) {
          this.center = '';
        }
        this.render();
      } catch (err) {
        if (seq !== this.seq) return;
        this.note = `Couldn't load the figures (${err.message}). Try reloading the page.`;
      } finally {
        if (seq === this.seq) this.busy = false;
      }
    },

    render() {
      const list = this.selected ? [this.selected] : this.centers;
      const cur = figures(total(list, 'cur'));
      const prev = figures(total(list, 'prev'));

      this.perf.cells = METRICS.map((m) => ({
        key: m[0],
        label: m[1],
        help: m[4],
        value: s.fmt(cur[m[0]], m[2]),
        change: s.change(cur[m[0]], prev[m[0]], m[3], m[2]),
        health: null,
      }));
      this.bounce.cells = s.BOUNCE_METRICS.map((m) => ({
        key: m[0],
        label: m[1],
        help: m[3],
        value: s.fmt(cur[m[0]], m[2]),
        change: s.change(cur[m[0]], prev[m[0]], false, m[2], true),
        health: s.health(cur[m[0]], m[4]),
      }));

      const days = this.p ? s.periodDays(this.p) : 0;
      const subs = list.reduce((n, c) => n + (c.subscribers || 0), 0);
      const sending = list.filter((c) => c.cur && c.cur.campaigns > 0).length;
      const who = this.selected ? '' : ` from ${sending} of ${list.length} center${list.length === 1 ? '' : 's'}`;
      this.note = `${cur.campaigns} campaign${cur.campaigns === 1 ? '' : 's'}${who} in this period, compared with the previous ${days} day${days === 1 ? '' : 's'}. ${subs.toLocaleString()} subscribers in all.`;

      this.rows = this.centers.map((c) => {
        const f = c.cur ? figures(c.cur) : null;
        const hard = f && s.health(f.hardRate, s.BOUNCE_METRICS[1][4]);
        const complaint = f && s.health(f.complaintRate, s.BOUNCE_METRICS[4][4]);
        const sent = f && f.sends > 0;
        return {
          ...c,
          state: centerState(c),
          sort: {
            name: c.name.toLowerCase(),
            subscribers: f ? c.subscribers : null,
            sends: f ? f.sends : null,
            open: f ? f.open : null,
            hard: sent ? f.hardRate : null,
            complaint: sent ? f.complaintRate : null,
            last: c.last_sent ? new Date(c.last_sent).getTime() : null,
          },
          subscribersText: s.fmt(c.subscribers, 'count'),
          newText: f && f.newSubs ? `+${f.newSubs.toLocaleString()} new` : '',
          sendsText: f ? s.fmt(f.sends, 'count') : '—',
          campaignsText: f ? `${f.campaigns} campaign${f.campaigns === 1 ? '' : 's'}` : '',
          openText: f ? s.fmt(f.open, 'pct1') : '—',
          hardText: f && f.sends ? s.fmt(f.hardRate, 'pct2') : '—',
          hardClass: (hard && HEALTH_CLASS[hard.variant]) || '',
          complaintText: f && f.sends ? s.fmt(f.complaintRate, 'pct2') : '—',
          complaintClass: (complaint && HEALTH_CLASS[complaint.variant]) || '',
          lastText: c.last_sent ? s.fmtDate(new Date(c.last_sent)) : '—',
        };
      });
    },

    async setStatus(c, status) {
      if (status === 'disabled' && !(await u.confirm(`Disable ${c.name}? Its admin pages and its subscribers' links stop working until it's enabled again. Its data is kept.`))) {
        return;
      }
      await api('centers', `/denma/centers/${encodeURIComponent(c.slug)}/status`, 'PUT', { status });
      u.toast(`${c.name}: ${status === 'disabled' ? 'disabled' : 'enabled'}`, 'success');
      this.load();
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaHub', component);
}, { once: true });
