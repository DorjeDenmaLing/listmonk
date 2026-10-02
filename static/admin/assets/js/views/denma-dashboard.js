// denma: "Campaign performance" and "Bounces and complaints" at the top of the
// dashboard (partials/denma/dashboard.html), for a date range compared with the
// previous period of the same length. Campaigns count in the period they
// started sending.
//   Total sends        sum of campaigns' sent counts
//   Delivery rate      (sent - bounces) / sent
//   Open rate          unique openers per campaign / delivered
//   Click rate         unique clickers per campaign / delivered
//   Clicks per opens   unique clickers / unique openers
//   Unsubscribed rate  people who unsubscribed in the period (not bounce
//                      removals) / delivered
// Bounce rates divide by the same sends. Health bands follow Amazon SES: it
// reviews an account at a 5% bounce or 0.1% complaint rate.
import Alpine from 'alpinejs';
import { urls } from '../main.js';
import * as s from '../denma-stats.js';

// [key, label, format, higher is good, description]
const METRICS = [
  ['sends', 'Total sends', 'count', true, 'Emails sent by campaigns that started in this period.'],
  ['delivery', 'Delivery rate', 'pct1', true, "Sent emails that didn't bounce."],
  ['open', 'Open rate', 'pct1', true, 'Unique people who opened, as a share of delivered emails (per campaign).'],
  ['click', 'Click rate', 'pct2', true, 'Unique people who clicked a link, as a share of delivered emails (per campaign).'],
  ['cpo', 'Clicks per unique opens', 'pct1', true, 'Unique clickers as a share of unique openers.'],
  ['unsub', 'Unsubscribed rate', 'pct2', false, 'People who unsubscribed in this period (not bounce removals), as a share of delivered emails.'],
];

const { BOUNCE_METRICS, health } = s;

const BOUNCE_TYPES = { hard: 'Hard', soft: 'Soft', complaint: 'Complaint' };

const placeholders = (list) => list.map((m) => ({ key: m[0], label: m[1], help: m[3] || m[4], value: '…', change: null, health: null }));

function bounceCounts(rows, p) {
  const c = { hard: 0, soft: 0, complaint: 0, list: [] };
  rows.forEach((b) => {
    const t = new Date(b.created_at);
    if (t < p.start || t >= p.end) {
      return;
    }
    if (c[b.type] !== undefined) {
      c[b.type] += 1;
    }
    c.list.push(b);
  });
  return c;
}

function component(opts) {
  return {
    ...s.loadRange(),
    ranges: s.RANGES,
    showPerf: opts.performance,
    showBounces: opts.bounces,

    perf: { cells: placeholders(METRICS), note: '', busy: true },
    bounce: { cells: placeholders(BOUNCE_METRICS), recent: [], summary: '', more: '', note: '', busy: true },
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

    async load() {
      const p = s.periods(this);
      if (!p) {
        this.perf.note = 'Choose a start and end date.';
        return;
      }
      s.saveRange(this);
      this.seq += 1;
      const { seq } = this;
      this.perf.busy = true;
      this.bounce.busy = true;

      // Fresh campaigns on every load: sends feed both sections.
      const camps = s.fetchCampaigns();
      const sends = (c, per) => s.startedIn(c, per).reduce((n, x) => n + x.sent, 0);

      if (this.showPerf) {
        camps.then((c) => Promise.all([s.measure(c, p.cur), s.measure(c, p.prev)]))
          .then(([cur, prev]) => {
            if (seq !== this.seq) return;
            this.perf.cells = METRICS.map((m) => ({
              key: m[0],
              label: m[1],
              help: m[4],
              value: s.fmt(cur[m[0]], m[2]),
              change: s.change(cur[m[0]], prev[m[0]], m[3], m[2]),
            }));
            const days = s.periodDays(p);
            this.perf.note = `${cur.campaigns} campaign${cur.campaigns === 1 ? '' : 's'} in this period, compared with the previous ${days} day${days === 1 ? '' : 's'}.`;
          })
          .catch((err) => {
            if (seq !== this.seq) return;
            this.perf.note = `Couldn't load the figures (${err.message}). Try reloading the page.`;
          })
          .finally(() => {
            if (seq === this.seq) this.perf.busy = false;
          });
      }

      if (this.showBounces) {
        Promise.all([s.fetchBounces(p.prev.start), camps.catch(() => null)])
          .then(([rows, c]) => {
            if (seq !== this.seq) return;
            this.renderBounces(rows, c && sends(c, p.cur), c && sends(c, p.prev), p);
          })
          .catch((err) => {
            if (seq !== this.seq) return;
            this.bounce.note = `Couldn't load bounces (${err.message}).`;
          })
          .finally(() => {
            if (seq === this.seq) this.bounce.busy = false;
          });
      }
    },

    renderBounces(rows, sends, prevSends, p) {
      const cur = bounceCounts(rows, p.cur);
      const prev = bounceCounts(rows, p.prev);
      const rate = (n, d) => (d > 0 ? n / d : null);
      cur.hardRate = rate(cur.hard, sends);
      prev.hardRate = rate(prev.hard, prevSends);
      cur.complaintRate = rate(cur.complaint, sends);
      prev.complaintRate = rate(prev.complaint, prevSends);

      this.bounce.cells = BOUNCE_METRICS.map((m) => ({
        key: m[0],
        label: m[1],
        help: m[3],
        value: s.fmt(cur[m[0]], m[2]),
        change: s.change(cur[m[0]], prev[m[0]], false, m[2], true),
        health: health(cur[m[0]], m[4]),
      }));

      // Latest ten in the period.
      this.bounce.recent = cur.list.slice(0, 10).map((b) => {
        const when = new Date(b.created_at);
        return {
          id: b.id,
          when: `${s.fmtDate(when)}, ${s.fmtTime(when)}`,
          type: b.type,
          typeLabel: BOUNCE_TYPES[b.type] || b.type,
          email: b.email || (b.subscriber_id ? `Subscriber ${b.subscriber_id}` : '—'),
          url: b.subscriber_id ? `${urls.admin}/subscribers/${encodeURIComponent(b.subscriber_id)}` : '',
          campaign: (b.campaign && typeof b.campaign === 'object' && b.campaign.name) || '—',
        };
      });
      const n = cur.list.length;
      this.bounce.summary = n > 10 ? `Latest bounces and complaints (10 of ${n} in this period)`
        : `Bounces and complaints in this period (${n})`;
      this.bounce.more = n > 10 ? 'Showing the latest 10. View all bounces for the rest.' : '';
      this.bounce.note = '';
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaDashboard', component);
});
