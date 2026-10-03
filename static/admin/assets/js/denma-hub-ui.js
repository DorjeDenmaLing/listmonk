// denma: the hub's center picker and center tables, for hundreds of centers
// (partials/denma/hub-ui.html), on its Dashboard, Centers, Analytics and
// Activity pages. Both are mixed into a page's component. Also what the
// Dashboard and Centers pages share: their figures and the center last shown.
import { getJSON } from './denma-stats.js';

// The center the Dashboard and Analytics last showed ('' for all).
const CENTER_KEY = 'denma-hub-center';

export function savedCenter() {
  try {
    return localStorage.getItem(CENTER_KEY) || '';
  } catch {
    return '';
  }
}

export function saveCenter(slug) {
  try {
    localStorage.setItem(CENTER_KEY, slug);
  } catch { /* storage unavailable */ }
}

// hubStats gets every center's sending for the periods p (denma-stats.js
// periods) from /api/denma/hub/stats (cmd/denma_hub.go).
export function hubStats(p) {
  const q = new URLSearchParams({
    from: p.cur.start.toISOString(),
    to: p.cur.end.toISOString(),
    prev_from: p.prev.start.toISOString(),
    prev_to: p.prev.end.toISOString(),
  });
  return getJSON(`/denma/hub/stats?${q}`);
}

// hubFigures are the figures shown from a period of hubStats (or several
// added up).
export function hubFigures(p) {
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

// centerPicker is a searchable center picker ("denma-center-picker"), for a
// component with centers ([{ slug, name }]), center (the chosen slug) and
// onCenter() (called when it changes). fixed are the choices before the
// centers, the first being the default: e.g. [{ slug: '', name: 'All centers' }].
export function centerPicker(fixed) {
  return {
    pick: { q: '', open: false, i: 0 },

    // The choices matching what's typed: names starting with it first, then
    // with a word starting with it, then the rest (addresses too).
    pickChoices() {
      const all = fixed.concat(this.centers);
      const q = this.pick.q.trim().toLowerCase();
      if (!q) return all;
      const rank = (c) => {
        const name = c.name.toLowerCase();
        if (name.startsWith(q) || c.slug.startsWith(q)) return 0;
        if (name.includes(` ${q}`)) return 1;
        return name.includes(q) || c.slug.includes(q) ? 2 : -1;
      };
      const ranked = all.map((c) => [rank(c), c]).filter(([r]) => r >= 0);
      return [0, 1, 2].flatMap((n) => ranked.filter(([r]) => r === n).map(([, c]) => c));
    },

    pickName() {
      const c = fixed.concat(this.centers).find((x) => x.slug === this.center);
      return c ? c.name : fixed[0].name;
    },

    pickOpen() {
      if (this.pick.open) return;
      this.pick.q = '';
      this.pick.i = Math.max(0, this.pickChoices().findIndex((c) => c.slug === this.center));
      this.pick.open = true;
      this.pickShow();
    },

    pickClose() {
      this.pick.open = false;
      this.pick.q = '';
    },

    pickInput() {
      this.pick.open = true;
      this.pick.i = 0;
    },

    pickMove(n) {
      if (!this.pick.open) {
        this.pickOpen();
        return;
      }
      const len = this.pickChoices().length;
      if (len) {
        this.pick.i = (this.pick.i + n + len) % len;
        this.pickShow();
      }
    },

    // Scrolls the highlighted choice into view.
    pickShow() {
      this.$nextTick(() => {
        const el = this.$refs.pickList && this.$refs.pickList.querySelectorAll('[role=option]')[this.pick.i];
        if (el) el.scrollIntoView({ block: 'nearest' });
      });
    },

    pickChoose(c) {
      const x = c || this.pickChoices()[this.pick.i];
      if (!x) return;
      this.pickClose();
      if (x.slug !== this.center) {
        this.center = x.slug;
        this.onCenter();
      }
    },
  };
}

// Names in natural order: "Center 9" before "Center 10".
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });
const compare = (x, y) => (typeof x === 'string' ? collator.compare(x, y) : (x > y) - (x < y));

// centerTable adds search, sorting and pages ("denma-th", "denma-pager") to a
// table of this.rows, each with name, slug, state (running, starting, error,
// disabled) and sort ({ key: value }: null sorts last either way).
export function centerTable(key = 'name', dir = 'asc', perPage = 50) {
  return {
    tbl: { q: '', status: '', key, dir, page: 1, perPage },

    // The rows found, sorted.
    tblRows() {
      const q = this.tbl.q.trim().toLowerCase();
      const { key: k, dir: d, status } = this.tbl;
      const sign = d === 'asc' ? 1 : -1;
      return this.rows
        .filter((r) => (!q || r.name.toLowerCase().includes(q) || r.slug.includes(q)) && (!status || r.state === status))
        .sort((a, b) => {
          const x = a.sort[k];
          const y = b.sort[k];
          if (x == null || y == null) {
            return (x == null) - (y == null);
          }
          return sign * compare(x, y) || compare(a.sort.name, b.sort.name);
        });
    },

    tblPages() {
      return Math.max(1, Math.ceil(this.tblRows().length / this.tbl.perPage));
    },

    // The current page's rows.
    tblPage() {
      const page = Math.min(this.tbl.page, this.tblPages());
      return this.tblRows().slice((page - 1) * this.tbl.perPage, page * this.tbl.perPage);
    },

    tblGo(n) {
      this.tbl.page = Math.min(Math.max(1, this.tbl.page + n), this.tblPages());
    },

    tblFind() {
      this.tbl.page = 1;
    },

    // What's shown, e.g. "51–100 of 300 centers".
    tblInfo() {
      const n = this.tblRows().length;
      const of = n < this.rows.length ? ` (of ${this.rows.length})` : '';
      if (n === 0) {
        return this.rows.length ? `No center matches${of}.` : '';
      }
      const page = Math.min(this.tbl.page, this.tblPages());
      const from = (page - 1) * this.tbl.perPage + 1;
      const to = Math.min(n, page * this.tbl.perPage);
      const range = n > this.tbl.perPage ? `${from}–${to} of ` : '';
      return `${range}${n.toLocaleString()} center${n === 1 ? '' : 's'}${of}`;
    },

    // Sorts by k: first in direction first, then the other way.
    tblSort(k, first = 'asc') {
      if (this.tbl.key === k) {
        this.tbl.dir = this.tbl.dir === 'asc' ? 'desc' : 'asc';
      } else {
        this.tbl.key = k;
        this.tbl.dir = first;
      }
      this.tbl.page = 1;
    },

    tblSorted(k) {
      return this.tbl.key === k ? this.tbl.dir : null;
    },

    tblAria(k) {
      if (this.tbl.key !== k) return null;
      return this.tbl.dir === 'asc' ? 'ascending' : 'descending';
    },
  };
}

// centerState is a center's state for the table's status filter.
export function centerState(c) {
  if (c.starting) return 'starting';
  if (c.status === 'disabled') return 'disabled';
  if (c.error) return 'error';
  return 'running';
}
