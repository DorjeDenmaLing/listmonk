// denma: campaign calendar (views/denma-calendar.html). Campaigns come from
// /api/campaigns with the user's own session, so they're limited to the
// lists the user can see.
import Alpine from 'alpinejs';
import { api, urls } from '../main.js';

const STATUSES = {
  finished: 'Sent',
  running: 'Sending',
  paused: 'Paused',
  scheduled: 'Scheduled',
  cancelled: 'Cancelled',
  draft: 'Draft',
};

function startOfMonth(d) {
  return new Date(d.getFullYear(), d.getMonth(), 1);
}

function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function dayKey(d) {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

// The day a campaign belongs on: when it went out for sent/sending ones,
// its scheduled time for scheduled ones and drafts that have a send date.
function whenOf(c) {
  const t = (c.status === 'scheduled' || c.status === 'draft')
    ? c.send_at
    : (c.started_at || c.send_at || c.updated_at);
  return t ? new Date(t) : null;
}

const byWhen = (a, b) => (a.when || 0) - (b.when || 0);

function component() {
  return {
    campaigns: null,
    cursor: startOfMonth(new Date()),
    drafts: true,
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone,

    async init() {
      const d = await api('denma-calendar', '/campaigns?per_page=all&no_body=true');
      this.campaigns = ((d && d.results) || []).map((c) => ({ ...c, when: whenOf(c) }));
    },

    get monthLabel() {
      return this.cursor.toLocaleDateString([], { month: 'long', year: 'numeric' });
    },

    shift(n) {
      this.cursor = new Date(this.cursor.getFullYear(), this.cursor.getMonth() + n, 1);
    },

    goToday() {
      this.cursor = startOfMonth(new Date());
    },

    // Campaigns with a date, drafts only if they're shown.
    get shown() {
      return (this.campaigns || []).filter((c) => c.when && (this.drafts || c.status !== 'draft'));
    },

    // Whole weeks covering the month, each day with its campaigns.
    get days() {
      const byDay = {};
      this.shown.forEach((c) => {
        (byDay[dayKey(c.when)] = byDay[dayKey(c.when)] || []).push(c);
      });

      const cur = this.cursor;
      const today = new Date();
      const last = new Date(cur.getFullYear(), cur.getMonth() + 1, 0);
      const cells = Math.ceil((cur.getDay() + last.getDate()) / 7) * 7;
      const out = [];
      for (let i = 0; i < cells; i += 1) {
        const d = new Date(cur.getFullYear(), cur.getMonth(), 1 - cur.getDay() + i);
        out.push({
          key: dayKey(d),
          num: d.getDate(),
          other: d.getMonth() !== cur.getMonth(),
          today: sameDay(d, today),
          items: (byDay[dayKey(d)] || []).sort(byWhen),
        });
      }
      return out;
    },

    // This month as a list grouped by day (narrow screens).
    get agenda() {
      const cur = this.cursor;
      const groups = [];
      this.shown
        .filter((c) => c.when.getFullYear() === cur.getFullYear() && c.when.getMonth() === cur.getMonth())
        .sort(byWhen)
        .forEach((c) => {
          const g = groups[groups.length - 1];
          if (g && sameDay(g.date, c.when)) {
            g.items.push(c);
          } else {
            groups.push({ key: dayKey(c.when), date: c.when, label: this.fmtDate(c.when), items: [c] });
          }
        });
      return groups;
    },

    get upcoming() {
      return (this.campaigns || [])
        .filter((c) => ['scheduled', 'running', 'paused'].includes(c.status))
        .sort(byWhen);
    },

    get unscheduled() {
      return (this.campaigns || []).filter((c) => c.status === 'draft' && !c.when);
    },

    url(c) {
      return `${urls.admin}/campaigns/${encodeURIComponent(c.id)}`;
    },

    name(c) {
      return c.name || c.subject || 'Untitled';
    },

    statusLabel(s) {
      return STATUSES[s] || s;
    },

    tip(c) {
      const lists = (Array.isArray(c.lists) ? c.lists : []).map((l) => l && l.name).filter(Boolean);
      const tip = [this.statusLabel(c.status)];
      if (c.subject) tip.push(`Subject: ${c.subject}`);
      if (lists.length) tip.push(`Lists: ${lists.join(', ')}`);
      if (c.status === 'finished' || c.status === 'running') tip.push(`Sent: ${c.sent} of ${c.to_send}`);
      return tip.join('\n');
    },

    fmtTime(d) {
      return d ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }) : '';
    },

    fmtDate(d) {
      return d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaCalendar', component);
});
