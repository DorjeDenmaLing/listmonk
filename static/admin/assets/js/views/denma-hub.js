// denma: the hub's dashboard (partials/denma/hub.html): every center's
// sending, all together or one at a time, for a date range compared with the
// previous period of the same length. The figures come from
// /api/denma/hub/stats (cmd/denma_hub.go), per center; they're added up here,
// so rates for all centers weigh each center by its sending. Definitions are
// the centers' dashboards' (views/denma-dashboard.js). The centers' own rows
// are on the Centers page (views/denma-hub-centers.js).
import Alpine from 'alpinejs';
import { urls } from '../main.js';
import * as s from '../denma-stats.js';
import { centerPicker, hubFigures, hubStats, savedCenter, saveCenter } from '../denma-hub-ui.js';

// [key, label, format, higher is good, description]
const METRICS = [
  ['sends', 'Total sends', 'count', true, 'Emails sent by campaigns that started in this period.'],
  ['delivery', 'Delivery rate', 'pct1', true, "Sent emails that didn't bounce."],
  ['open', 'Open rate', 'pct1', true, 'Unique people who opened, as a share of delivered emails (per campaign).'],
  ['click', 'Click rate', 'pct2', true, 'Unique people who clicked a link, as a share of delivered emails (per campaign).'],
  ['unsub', 'Unsubscribed rate', 'pct2', false, 'People who unsubscribed in this period (not bounce removals), as a share of delivered emails.'],
  ['newSubs', 'New subscribers', 'count', true, 'Subscribers added in this period.'],
];

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

const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

function component() {
  return {
    ...s.loadRange(),
    ...centerPicker([{ slug: '', name: 'All centers' }]),
    ranges: s.RANGES,
    center: savedCenter(),
    centers: [],
    perf: { cells: placeholders(METRICS) },
    bounce: { cells: placeholders(s.BOUNCE_METRICS) },
    problems: { text: '', url: '' },
    note: '',
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
      saveCenter(this.center);
      this.render();
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

      try {
        const data = await hubStats(p);
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
      const cur = hubFigures(total(list, 'cur'));
      const prev = hubFigures(total(list, 'prev'));

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
      const who = this.selected ? '' : ` from ${sending} of ${plural(list.length, 'center', 'centers')}`;
      this.note = `${plural(cur.campaigns, 'campaign', 'campaigns')}${who} in this period, compared with the previous ${plural(days, 'day', 'days')}. ${subs.toLocaleString()} subscribers in all.`;

      // Centers not running (other than disabled ones), to look into.
      const failed = this.centers.filter((c) => c.error && c.status !== 'disabled' && !c.starting).length;
      const starting = this.centers.filter((c) => c.starting).length;
      const text = [];
      if (failed) text.push(`${plural(failed, 'center', 'centers')} didn't start.`);
      if (starting) text.push(`${plural(starting, 'center is', 'centers are')} still starting.`);
      this.problems = {
        text: text.join(' '),
        url: `${urls.admin}/centers?status=${failed ? 'error' : 'starting'}`,
      };
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaHub', component);
}, { once: true });
