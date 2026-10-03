// denma: shared figures for the dashboard and Analytics sections (ported from
// the production admin add-on, listmonk-js/admin-custom.js). Everything comes
// from listmonk's own API with the user's session; subscriber counts are
// named figures computed on the server (cmd/denma_stats.go).
import { urls } from './main.js';

// Unique opens and clicks exist only since privacy.individual_tracking was
// turned on in production; earlier campaigns only have anonymous totals, so
// they're left out of open and click figures.
export const TRACKING_SINCE = new Date('2026-09-29T18:41:52Z');

export const RANGES = [
  ['7', 'Last 7 days'],
  ['30', 'Last 30 days'],
  ['90', 'Last 90 days'],
  ['365', 'Last 12 months'],
  ['ytd', 'Year to date'],
  ['custom', 'Custom range'],
];

// ---- Date range, shared by the dashboard and Analytics (kept per browser) ----
const RANGE_KEY = 'denma-stats-range';

export function loadRange() {
  try {
    const r = JSON.parse(localStorage.getItem(RANGE_KEY) || 'null');
    if (r && r.range) {
      return { range: r.range, from: r.from || '', to: r.to || '' };
    }
  } catch { /* storage unavailable: defaults */ }
  return { range: '30', from: '', to: '' };
}

export function saveRange(r) {
  try {
    localStorage.setItem(RANGE_KEY, JSON.stringify({ range: r.range, from: r.from, to: r.to }));
  } catch { /* storage unavailable */ }
}

export function isoDay(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

export function addDays(d, n) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
}

function parseDay(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s || '');
  return m ? new Date(+m[1], +m[2] - 1, +m[3]) : null;
}

// [start, end) in local time, plus the previous period of the same length.
// null when a custom range is incomplete.
export function periods(r) {
  const today = new Date();
  let end = addDays(new Date(today.getFullYear(), today.getMonth(), today.getDate()), 1);
  let start;
  if (r.range === 'custom') {
    const f = parseDay(r.from);
    const t = parseDay(r.to);
    if (!f || !t || t < f) {
      return null;
    }
    start = f;
    end = addDays(t, 1);
  } else if (r.range === 'ytd') {
    start = new Date(today.getFullYear(), 0, 1);
  } else {
    start = addDays(end, -parseInt(r.range, 10));
  }
  const len = end - start;
  return { cur: { start, end }, prev: { start: new Date(start - len), end: start } };
}

export function periodDays(p) {
  return Math.round((p.cur.end - p.cur.start) / 86400000);
}

// ---- API ----

// GET an /api path and return its `data`. Errors are thrown (not toasted), so
// each section can say what failed in its own note.
export async function getJSON(uri) {
  const res = await fetch(`${urls.api}${uri}`, { headers: { Accept: 'application/json' } });
  const out = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(out.message || `HTTP ${res.status}`);
  }
  return out.data;
}

export async function fetchCampaigns() {
  const d = await getJSON('/campaigns?per_page=all&no_body=true');
  return (d && d.results) || [];
}

// Campaigns that started sending in period p.
export function startedIn(campaigns, p) {
  return campaigns.filter((c) => {
    const t = c.started_at && new Date(c.started_at);
    return t && t >= p.start && t < p.end && c.sent > 0;
  });
}

export const isTracked = (c) => new Date(c.started_at) >= TRACKING_SINCE;

// Unique views or clicks per campaign ({id: count}) since `from`.
export async function perCampaign(type, ids, from) {
  if (!ids.length) {
    return {};
  }
  const q = `${ids.map((id) => `id=${id}`).join('&')}&from=${isoDay(from)}&to=${isoDay(addDays(new Date(), 1))}`;
  const rows = await getJSON(`/campaigns/analytics/${type}?${q}`);
  const out = {};
  (rows || []).forEach((r) => {
    out[r.campaign_id] = (out[r.campaign_id] || 0) + (r.count || 0);
  });
  return out;
}

const sumValues = (m) => Object.values(m).reduce((n, v) => n + v, 0);

// Subscribers in one of the server's figures (cmd/denma_stats.go): { total,
// results } (results only when all). opts: period ({ start, end }), campaign
// (an ID), and search (the search language, cmd/denma_search.go).
export function subscriberStat(metric, opts = {}, all = false) {
  const q = new URLSearchParams({ metric, per_page: all ? 'all' : '1' });
  if (opts.period) {
    q.set('from', opts.period.start.toISOString());
    q.set('to', opts.period.end.toISOString());
  }
  if (opts.campaign) {
    q.set('campaign', opts.campaign);
  }
  if (opts.search) {
    q.set('search', opts.search);
  }
  return getJSON(`/denma/stats/subscribers?${q.toString()}`);
}

// Bounces, newest first, paging back only as far as `since`.
const BOUNCE_PAGE = 500;
export async function fetchBounces(since) {
  let out = [];
  for (let n = 1; ; n += 1) {
    // eslint-disable-next-line no-await-in-loop
    const d = await getJSON(`/bounces?order_by=created_at&order=desc&per_page=${BOUNCE_PAGE}&page=${n}`);
    const rows = (d && d.results) || [];
    out = out.concat(rows);
    const last = rows.length && new Date(rows[rows.length - 1].created_at);
    if (rows.length < BOUNCE_PAGE || last < since) {
      return out;
    }
  }
}

// Campaign performance for period p (see METRICS in views/denma-dashboard.js).
export async function measure(campaigns, p) {
  const inPeriod = startedIn(campaigns, p);
  const tracked = inPeriod.filter(isTracked);
  const sum = (list, f) => list.reduce((n, c) => n + f(c), 0);
  const bounced = (c) => Math.min(c.bounces || 0, c.sent);
  const sends = sum(inPeriod, (c) => c.sent);
  const bounces = sum(inPeriod, bounced);
  const trackedDelivered = sum(tracked, (c) => c.sent - bounced(c));
  const ids = tracked.map((c) => c.id);
  const from = tracked.length ? new Date(Math.min(...tracked.map((c) => new Date(c.started_at)))) : p.start;

  const [opens, clicks, unsubs] = await Promise.all([
    perCampaign('views', ids, from).then(sumValues),
    perCampaign('clicks', ids, from).then(sumValues),
    subscriberStat('unsub', { period: p }).then((d) => (d && d.total) || 0),
  ]);
  const delivered = sends - bounces;
  const rate = (n, d) => (d > 0 ? n / d : null);
  return {
    campaigns: inPeriod.length,
    sends,
    delivery: rate(delivered, sends),
    open: rate(opens, trackedDelivered),
    click: rate(clicks, trackedDelivered),
    cpo: rate(clicks, opens),
    unsub: rate(unsubs, delivered),
  };
}

// ---- Bounces ----

// [key, label, format, description, health bands [watch from, at risk from]].
// Health bands follow Amazon SES: it reviews an account at a 5% bounce or
// 0.1% complaint rate.
export const BOUNCE_METRICS = [
  ['hard', 'Hard bounces', 'count', "Permanent failures (address doesn't exist). listmonk blocklists these addresses."],
  ['hardRate', 'Hard bounce rate', 'pct2', 'Hard bounces as a share of sends. Amazon SES reviews accounts at 5% and can pause sending at 10%.', [0.02, 0.05]],
  ['soft', 'Soft bounces', 'count', 'Temporary failures (mailbox full, server busy). listmonk records these but keeps sending.'],
  ['complaint', 'Complaints', 'count', 'People who marked an email as spam. listmonk blocklists them.'],
  ['complaintRate', 'Complaint rate', 'pct2', 'Complaints as a share of sends. Amazon SES reviews accounts at 0.1% and can pause sending at 0.5%; Gmail expects under 0.3%.', [0.0005, 0.001]],
];

export function health(v, bands) {
  if (v == null || !bands) {
    return null;
  }
  if (v < bands[0]) {
    return { variant: 'success', label: 'Healthy' };
  }
  return v < bands[1] ? { variant: 'warning', label: 'Watch' } : { variant: 'danger', label: 'At risk' };
}

// ---- Audience growth (Analytics, and the hub's Analytics) ----
//
// g: { active, added, unsub, removed, net, and prevAdded, prevUnsub,
// prevRemoved, prevNet }. Website signups waiting on a double opt-in holding
// list aren't counted as subscribers until they confirm.

const GROWTH = [
  // [label, current key, previous key, higher is good, description]
  ['Active subscribers', 'active', null, null, 'Enabled subscribers with at least one list subscription, right now.'],
  ['New', 'added', 'prevAdded', true, 'Subscribers added in this period (imports count on the day they were imported).'],
  ['Unsubscribed', 'unsub', 'prevUnsub', false, 'People who unsubscribed in this period (not bounce removals).'],
  ['Bounced or complained', 'removed', 'prevRemoved', false, 'People removed after a bounce or spam complaint in this period.'],
  ['Net change', 'net', 'prevNet', true, 'New minus unsubscribed minus bounced or complained.'],
];

export const growthPlaceholders = () => GROWTH.map((m) => ({ key: m[1], label: m[0], help: m[4], value: '…', change: null, health: null }));

export function growthCells(g) {
  return GROWTH.map(([label, key, prevKey, higherIsGood, help]) => {
    const v = g[key];
    let ch = null;
    if (prevKey) {
      const pv = g[prevKey];
      if (pv === v) {
        ch = { cls: 'flat', arrow: '▬', text: 'same as before', title: '' };
      } else {
        const up = v > pv;
        ch = { cls: up === higherIsGood ? 'good' : 'bad', arrow: up ? '▲' : '▼', text: `${Math.abs(v - pv).toLocaleString()} vs previous`, title: `Previous period: ${pv.toLocaleString()}` };
      }
    }
    return { key, label, help, value: (key === 'net' && v > 0 ? '+' : '') + v.toLocaleString(), change: ch, health: null };
  });
}

// Chart buckets: days up to 45 days, weeks up to 200, then months.
export function buckets(p) {
  const days = (p.cur.end - p.cur.start) / 86400000;
  const unit = days <= 45 ? 'day' : (days <= 200 ? 'week' : 'month');
  let d = new Date(p.cur.start);
  if (unit === 'week') d = addDays(d, -d.getDay());
  if (unit === 'month') d = new Date(d.getFullYear(), d.getMonth(), 1);
  const list = [];
  while (d < p.cur.end) {
    const next = unit === 'day' ? addDays(d, 1) : (unit === 'week' ? addDays(d, 7) : new Date(d.getFullYear(), d.getMonth() + 1, 1));
    list.push({ start: d, end: next, added: 0, unsub: 0, removed: 0 });
    d = next;
  }
  return { unit, list };
}

function bucketLabel(b, unit, short) {
  if (unit === 'month') {
    return b.start.toLocaleDateString([], { month: short ? 'short' : 'long', year: short ? undefined : 'numeric' });
  }
  const str = b.start.toLocaleDateString([], { month: 'short', day: 'numeric' });
  return unit === 'week' && !short ? `Week of ${str}` : str;
}

// Bars of subscribers gained (up) and lost (down) per bucket, drawn at the
// container's real width so labels stay readable on phones.
export function growthChart(bk, width) {
  const NS = 'http://www.w3.org/2000/svg';
  const W = Math.max(300, Math.round(width || 900));
  const H = W < 500 ? 180 : 220;
  const top = 12;
  const bottom = 26;
  const left = 36;
  const n = bk.list.length;
  let max = 1;
  bk.list.forEach((b) => { max = Math.max(max, b.added, b.unsub + b.removed); });
  const mid = top + (H - top - bottom) / 2;
  const scale = (H - top - bottom) / 2 / max;
  const slot = (W - left) / n;
  const bw = Math.max(2, Math.min(28, slot * 0.7));

  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('class', 'denma-chart');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', `Subscribers gained and lost per ${bk.unit}`);
  const node = (parent, tag, attrs, text) => {
    const e = document.createElementNS(NS, tag);
    Object.entries(attrs).forEach(([k, v]) => e.setAttribute(k, v));
    if (text != null) e.textContent = text;
    parent.appendChild(e);
    return e;
  };

  [max, 0, -max].forEach((v) => {
    const y = mid - v * scale;
    node(svg, 'line', { x1: left, x2: W, y1: y, y2: y, class: v === 0 ? 'axis' : 'grid' });
    node(svg, 'text', { x: left - 6, y: y + 4, 'text-anchor': 'end', class: 'tick' }, v === 0 ? '0' : `${v > 0 ? '+' : '−'}${Math.abs(v)}`);
  });

  const every = Math.ceil(n / Math.max(3, Math.floor((W - left) / 80)));
  bk.list.forEach((b, i) => {
    const x = left + slot * i + (slot - bw) / 2;
    const g = node(svg, 'g', {});
    node(g, 'title', {}, `${bucketLabel(b, bk.unit)}: +${b.added} new, −${b.unsub} unsubscribed, −${b.removed} bounced or complained`);
    const bar = (y, h, cls) => {
      if (h > 0) node(g, 'rect', { x, width: bw, y, height: h, class: cls, rx: 2 });
    };
    bar(mid - b.added * scale, b.added * scale, 'added');
    bar(mid, b.unsub * scale, 'unsub');
    bar(mid + b.unsub * scale, b.removed * scale, 'removed');
    // Invisible full-height target so the tooltip works on empty days too.
    node(g, 'rect', { x: left + slot * i, width: slot, y: top, height: H - top - bottom, class: 'hit' });
    if (i % every === 0) {
      node(svg, 'text', { x: left + slot * i + slot / 2, y: H - 8, 'text-anchor': 'middle', class: 'tick' }, bucketLabel(b, bk.unit, true));
    }
  });
  return svg;
}

// ---- Mailbox providers (Analytics, and the hub's Analytics) ----

// [key, label, address domains (* is any ending), opens inflated by Apple
// Mail]. The hub's are the same (denmaProviders, cmd/denma_hub_analytics.go).
export const PROVIDERS = [
  ['gmail', 'Gmail', ['gmail.com', 'googlemail.com']],
  ['microsoft', 'Microsoft', ['outlook.*', 'hotmail.*', 'live.*', 'msn.com', 'windowslive.com']],
  ['yahoo', 'Yahoo / AOL', ['yahoo.*', 'ymail.com', 'rocketmail.com', 'aol.*', 'aim.com']],
  ['apple', 'Apple iCloud', ['icloud.com', 'me.com', 'mac.com'], true],
];
export const OTHER_PROVIDER = "Every other address, including organizations' own domains (even when they use Google or Microsoft mail).";
const PROVIDER_MIN_PERIOD = 30; // emails before a rate is judged

export const pct = (n, d, digits) => (d > 0 ? `${(n / d * 100).toFixed(digits)}%` : '—');

// rows: { [provider key, 'other' or 'all']: { subscribers, unsub, received,
// opens, clicks } }. Everyone except this group and Apple (whose automatic
// opens would raise the bar), as picked from each row.
export function providerBaseline(rows, key, pick) {
  const all = pick(rows.all);
  const own = pick(rows[key]);
  const apple = key === 'apple' ? { received: 0, opens: 0 } : pick(rows.apple);
  return { received: all.received - own.received - apple.received, opens: all.opens - own.opens - apple.opens };
}

export const lowVersus = (own, rest, min) => own.received >= min && rest.received >= min && rest.opens > 0
  && own.opens / own.received < 0.5 * (rest.opens / rest.received);

// Why a provider is flagged over the period, or null. Apple is never flagged.
export function providerWarning(rows, key) {
  if (key === 'apple' || key === 'all') return null;
  if (lowVersus(rows[key], providerBaseline(rows, key, (x) => x), PROVIDER_MIN_PERIOD)) {
    return "Open rate is less than half of everyone else's in this period.";
  }
  return null;
}

// Figures for a provider row (or the "All" footer).
export function providerFigures(row) {
  const rate = (n, digits) => (row.received > 0 ? { pct: pct(n, row.received, digits), n: n.toLocaleString() } : null);
  return {
    subscribers: row.subscribers.toLocaleString(),
    received: row.received.toLocaleString(),
    open: rate(row.opens, 1),
    click: rate(row.clicks, 2),
    unsub: row.unsub.toLocaleString(),
  };
}

// ---- Formatting ----

export function fmt(v, kind) {
  if (v == null) {
    return '—';
  }
  if (kind === 'count') {
    return v.toLocaleString();
  }
  return `${(v * 100).toFixed(kind === 'pct2' ? 2 : 1)}%`;
}

export function fmtDate(d) {
  return d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' });
}

export function fmtTime(d) {
  return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

// Change from the previous period: { cls, arrow, text, title }.
//   higherIsGood  whether an increase is good (green) or bad (red)
//   newIfZero     a previous 0 shows as "new" (bounces) rather than "—"
export function change(cur, prev, higherIsGood, kind, newIfZero) {
  if (cur == null || prev == null || (prev === 0 && (!newIfZero || cur === 0))) {
    return { cls: '', arrow: '', text: '—', title: 'No comparable figure for the previous period.' };
  }
  if (prev === 0) {
    return { cls: higherIsGood ? 'good' : 'bad', arrow: '▲', text: 'new', title: 'None in the previous period.' };
  }
  const delta = (cur - prev) / prev;
  const flat = Math.abs(delta) < 0.0005;
  const up = delta > 0;
  return {
    cls: flat ? 'flat' : (up === higherIsGood ? 'good' : 'bad'),
    arrow: flat ? '▬' : (up ? '▲' : '▼'),
    text: `${Math.abs(delta * 100).toFixed(1)}%`,
    title: `Previous period: ${fmt(prev, kind)}`,
  };
}
