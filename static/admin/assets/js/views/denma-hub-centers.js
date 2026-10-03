// denma: the hub's Centers page (views/denma-centers.html): every center with
// its subscribers and its sending for a date range (as on the dashboard,
// views/denma-hub.js), to open, enable or disable. ?status= (running,
// starting, error, disabled) opens it filtered.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';
import * as s from '../denma-stats.js';
import { centerTable, centerState, hubFigures, hubStats, saveCenter } from '../denma-hub-ui.js';
import * as u from '../utils.js';

const HEALTH_CLASS = { warning: 'denma-warn', danger: 'text-danger' };

function component() {
  const table = centerTable();
  table.tbl.status = new URLSearchParams(window.location.search).get('status') || '';

  return {
    ...s.loadRange(),
    ...table,
    ranges: s.RANGES,
    centers: [],
    rows: [],
    busy: true,
    seq: 0,

    init() {
      this.load();
    },

    openURL(slug) {
      return `${urls.admin}/centers/${encodeURIComponent(slug)}/open`;
    },

    // Goes to the dashboard (page '') or Analytics showing only center c.
    showFigures(c, page) {
      saveCenter(c.slug);
      window.location.href = `${urls.admin}${page}`;
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
      if (!p) return;
      s.saveRange(this);
      this.seq += 1;
      const { seq } = this;
      this.busy = true;
      try {
        const data = await hubStats(p);
        if (seq !== this.seq) return;
        this.centers = data || [];
        this.render();
      } catch (err) {
        if (seq !== this.seq) return;
        u.toast(`Couldn't load the centers (${err.message}). Try reloading the page.`, 'error');
      } finally {
        if (seq === this.seq) this.busy = false;
      }
    },

    render() {
      this.rows = this.centers.map((c) => {
        const f = c.cur ? hubFigures(c.cur) : null;
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
          hardText: sent ? s.fmt(f.hardRate, 'pct2') : '—',
          hardClass: (hard && HEALTH_CLASS[hard.variant]) || '',
          complaintText: sent ? s.fmt(f.complaintRate, 'pct2') : '—',
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
  Alpine.data('denmaHubCenters', component);
}, { once: true });
