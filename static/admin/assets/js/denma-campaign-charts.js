// denma: Analytics -> Explore's views, clicks and bounces charts, in place
// of listmonk's (views/analytics.js, one hook; links are a table,
// partials/denma/analytics.html): bars per day or hour from zero, with every
// day or hour of the range on the axis, dates as the server buckets them, the
// admin's theme colours (redrawn when the theme changes), and a legend and
// the totals donut only for more than one campaign or automation. Automations
// chosen (denma-explore.js) are drawn as campaigns are.
import Chart from 'chart.js/auto';

const SERIES = 8;
const HOUR = 3600e3;
const DAY = 24 * HOUR;
const MAX_BUCKETS = 400;

// The page's theme colours, resolved (they're light-dark() tokens).
function themeColors() {
  const probe = document.createElement('span');
  probe.hidden = true;
  document.querySelector('section.analytics').appendChild(probe);
  const get = (v) => {
    probe.style.color = `var(${v})`;
    return getComputedStyle(probe).color;
  };
  const c = {
    text: get('--foreground'),
    muted: get('--muted-foreground'),
    grid: get('--border'),
    card: get('--card'),
    series: Array.from({ length: SERIES }, (_, i) => get(`--denma-series-${i + 1}`)),
  };
  probe.remove();
  return c;
}

// The bucket a count is in: hours for a range under 7 days, else days, as
// the server's query (get-campaign-analytics-counts) does.
function bucketing(d) {
  const from = document.querySelector('input[name=from]')?.value;
  const to = document.querySelector('input[name=to]')?.value;
  const rows = [...(d.views || []), ...(d.clicks || []), ...(d.bounces || [])];
  // The inputs are in the server's time zone; the timestamps carry its offset.
  const zone = (rows[0]?.timestamp.match(/(Z|[+-]\d\d:\d\d)$/) || ['Z'])[0];
  const m = zone.match(/([+-])(\d\d):(\d\d)/);
  const off = m ? (m[1] === '-' ? -1 : 1) * (m[2] * HOUR + m[3] * 60e3) : 0;
  const start = Date.parse(`${from}:00${zone}`);
  const end = Date.parse(`${to}:00${zone}`);
  const unit = end - start >= 7 * DAY ? DAY : HOUR;
  const floor = (t) => Math.floor((t + off) / unit) * unit - off;
  let keys = [];
  if (Number.isFinite(start) && Number.isFinite(end)) {
    for (let t = floor(start); t <= end && keys.length <= MAX_BUCKETS; t += unit) keys.push(t);
  }
  // A long range: only from the first count to the last.
  if (keys.length > MAX_BUCKETS || !keys.length) {
    const ts = rows.map((r) => floor(Date.parse(r.timestamp)));
    keys = [];
    for (let t = Math.min(...ts); t <= Math.max(...ts); t += unit) keys.push(t);
  }
  return { unit, keys, off, floor };
}

// A bucket's label: a day as the server counted it, an hour in local time.
function labeler(b, years) {
  const opts = (long) => (b.unit === DAY
    ? { day: 'numeric', month: 'short', year: years || long ? 'numeric' : undefined, weekday: long ? 'short' : undefined, timeZone: 'UTC' }
    : { day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit', weekday: long ? 'short' : undefined });
  const at = (t) => new Date(b.unit === DAY ? t + b.off : t);
  return {
    short: (t) => at(t).toLocaleString([], opts(false)),
    long: (t) => at(t).toLocaleString([], opts(true)),
    // Hours: the date at midnight, and every 6 hours when there are 2 days or less.
    tick: (t) => {
      if (b.unit === DAY) return at(t).toLocaleString([], opts(false));
      const h = at(t).getHours();
      if (h === 0) return at(t).toLocaleDateString([], { day: 'numeric', month: 'short' });
      if (b.keys.length <= 48 && h % 6 === 0) return at(t).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
      return null;
    },
  };
}

function tooltip(c) {
  return {
    backgroundColor: c.card,
    borderColor: c.grid,
    borderWidth: 1,
    titleColor: c.text,
    bodyColor: c.text,
    footerColor: c.muted,
    padding: 10,
    boxPadding: 4,
    usePointStyle: true,
  };
}

const charts = [];

function draw(canvas, cfg) {
  if (canvas) charts.push(new Chart(canvas, cfg));
}

// A row's campaign or automation, as the series' keys.
const rowKey = (r) => (r.automation_id ? `a${r.automation_id}` : `c${r.campaign_id}`);

// Bars of one figure per bucket, stacked by campaign or automation (camps:
// { key, label }), and its totals donut.
function renderCounts(key, camps, rows, b, c) {
  const canvas = document.getElementById(`chart-${key}`);
  if (!canvas) return;
  const index = new Map(b.keys.map((t, i) => [t, i]));
  const years = new Set(b.keys.map((t) => new Date(t + b.off).getUTCFullYear())).size > 1;
  const lbl = labeler(b, years);
  const many = camps.length > 1;
  const datasets = camps.map((cm, n) => {
    const data = new Array(b.keys.length).fill(0);
    rows.forEach((r) => {
      const i = index.get(b.floor(Date.parse(r.timestamp)));
      if (rowKey(r) === cm.key && i !== undefined) data[i] += r.count;
    });
    return {
      label: cm.label,
      data,
      backgroundColor: c.series[n % SERIES],
      borderRadius: 2,
      maxBarThickness: 32,
    };
  });

  draw(canvas, {
    type: 'bar',
    data: { labels: b.keys, datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 300 },
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { display: many, position: 'bottom', labels: { color: c.text, usePointStyle: true, pointStyle: 'rectRounded' } },
        tooltip: {
          ...tooltip(c),
          callbacks: {
            title: (items) => lbl.long(b.keys[items[0].dataIndex]),
            label: (item) => (many ? ` ${item.dataset.label}: ${item.raw.toLocaleString()}` : ` ${item.raw.toLocaleString()}`),
          },
        },
      },
      scales: {
        x: {
          stacked: true,
          grid: { display: false },
          border: { color: c.grid },
          ticks: { color: c.muted, maxRotation: 0, autoSkip: b.unit === DAY, autoSkipPadding: 16, callback: (v) => lbl.tick(b.keys[v]) },
        },
        y: {
          stacked: true,
          beginAtZero: true,
          grid: { color: c.grid },
          border: { display: false },
          ticks: { color: c.muted, precision: 0 },
        },
      },
    },
  });

  // The totals donut compares campaigns: only for more than one.
  const donut = document.getElementById(`chart-${key}-donut`);
  const col = donut?.closest('.donut-container');
  if (!many) {
    if (col) col.hidden = true;
    canvas.closest('.col-9')?.classList.replace('col-9', 'col-12');
    return;
  }
  draw(donut, {
    type: 'doughnut',
    data: {
      labels: datasets.map((ds) => ds.label),
      datasets: [{
        data: datasets.map((ds) => ds.data.reduce((a, v) => a + v, 0)),
        backgroundColor: datasets.map((ds) => ds.backgroundColor),
        borderColor: c.card,
        borderWidth: 2,
      }],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      cutout: '65%',
      animation: { duration: 300 },
      plugins: {
        legend: { display: false },
        tooltip: {
          ...tooltip(c),
          callbacks: {
            label: (item) => {
              const total = item.dataset.data.reduce((a, v) => a + v, 0);
              return ` ${item.raw.toLocaleString()} (${total ? ((item.raw / total) * 100).toFixed(1) : 0}%)`;
            },
          },
        },
      },
    },
  });
}

function render(d) {
  charts.splice(0).forEach((ch) => ch.destroy());
  const c = themeColors();
  Chart.defaults.font.family = getComputedStyle(document.body).fontFamily;
  Chart.defaults.color = c.muted;
  const a = window._denmaExplore || {};
  const camps = (d.campaigns || []).map((cm) => ({ key: `c${cm.id}`, label: `#${cm.id}: ${cm.name}` }))
    .concat((a.automations || []).map((au) => ({ key: `a${au.id}`, label: `Automation: ${au.name}` })));
  const all = {};
  ['views', 'clicks', 'bounces'].forEach((k) => { all[k] = [...(d[k] || []), ...(a[k] || [])]; });
  const b = bucketing(all);
  renderCounts('views', camps, all.views, b, c);
  renderCounts('clicks', camps, all.clicks, b, c);
  renderCounts('bounces', camps, all.bounces, b, c);
}

// Draws the charts; returns true, so listmonk's aren't drawn.
export function denmaRenderCharts(d) {
  render(d);
  // The theme: the system's, or the admin's switch (main.js sets color-scheme).
  const redraw = () => requestAnimationFrame(() => render(d));
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', redraw);
  new MutationObserver(redraw).observe(document.documentElement, { attributes: true, attributeFilter: ['style'] });
  return true;
}
