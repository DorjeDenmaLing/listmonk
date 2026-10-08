// denma: a center's automations (views/denma-automations.html), with their
// schedule, and an automation's page (views/denma-automation.html);
// cmd/denma_automations.go, cmd/denma_auto_schedule.go.
import Alpine from 'alpinejs';
import { api, urls, ListTag } from '../main.js';
import * as u from '../utils.js';
import { denmaTagAutocomplete } from '../denma-tags-ui.js';

const UNITS = { minutes: 1, hours: 60, days: 1440 };

// The list: turning automations on and off, and deleting them.
function denmaAutomations() {
  return {
    async onToggle(id, name, e) {
      const active = e.target.checked;
      try {
        await api('automations', `/denma/automations/${id}/status`, 'PUT', { active });
        u.toast(`${name} is ${active ? 'on' : 'off'}.`, 'success');
      } catch {
        e.target.checked = !active;
      }
    },

    async onDelete(id, name) {
      if (!(await u.confirm(`Delete ${name}? The record of whom it ran for goes too.`))) {
        return;
      }
      await api('automations', `/denma/automations/${id}`, 'DELETE');
      u.reload({ message: `Deleted ${name}`, variant: 'success' });
    },
  };
}

// A chosen list's ID. A list picked from the suggestions is a ListTag; one
// typed out and entered is its name.
function listID(tag) {
  if (tag && tag.id) {
    return tag.id;
  }
  const name = String(tag).trim().toLowerCase();
  const l = (window._lists || []).find((x) => x.name.toLowerCase() === name);
  return l ? l.id : 0;
}

// The lists chosen in a picker, from their IDs.
function listTags(ids) {
  return (ids || []).map((id) => (window._lists || []).find((l) => l.id === id)).filter(Boolean).map((l) => new ListTag(l));
}

// An automation's page.
function denmaAutomation() {
  const a = window._denmaAutomation || {};
  const templates = window._denmaAutoTemplates || [];
  const mins = a.delay_minutes || 0;
  let unit = 'now';
  if (mins) {
    unit = (mins % 1440 === 0 && 'days') || (mins % 60 === 0 && 'hours') || 'minutes';
  }

  return {
    denmaTagAutocomplete,
    form: {
      name: a.name || '',
      trigger_type: a.trigger_type || 'confirms',
      lists: listTags(a.list_ids),
      trigger_tags: [...(a.trigger_tags || [])],
      trigger_campaign_id: a.trigger_campaign_id || 0,
      if_tags: [...(a.if_tags || [])],
      unless_tags: [...(a.unless_tags || [])],
      add_lists: listTags(a.add_list_ids),
      remove_lists: listTags(a.remove_list_ids),
      add_tags: [...(a.add_tags || [])],
      remove_tags: [...(a.remove_tags || [])],
      send_email: a.id ? a.send_email : true,
      template_id: a.template_id || 0,
      subject: a.subject || '',
      from_email: a.from_email || '',
      active: a.id ? a.active : true,
    },
    unit,
    wait: mins ? mins / UNITS[unit] : 1,
    eachTime: a.each_time ? 'each' : 'once',
    testEmail: window._denmaTestEmail || '',

    data() {
      const delay = this.unit === 'now' ? 0 : Math.round((Number(this.wait) || 0) * UNITS[this.unit]);
      const {
        lists, add_lists: add, remove_lists: remove, ...rest
      } = this.form;
      const ids = (tags) => tags.map(listID).filter(Boolean);
      return {
        ...rest,
        trigger_tags: rest.trigger_tags.map(String),
        if_tags: rest.if_tags.map(String),
        unless_tags: rest.unless_tags.map(String),
        add_tags: rest.add_tags.map(String),
        remove_tags: rest.remove_tags.map(String),
        list_ids: ids(lists),
        add_list_ids: ids(add),
        remove_list_ids: ids(remove),
        delay_minutes: delay,
        each_time: this.eachTime === 'each',
      };
    },

    // The trigger's list picker suggests only lists of its kind: double
    // opt-in ones for "confirms", single opt-in ones for "joins".
    triggerListAutocomplete(el) {
      const optin = this.form.trigger_type === 'confirms' ? 'double' : 'single';
      const chosen = new Set(this.form.lists.map((l) => l.id));
      const q = el.value.toLowerCase();
      el.list.replaceChildren(...(window._lists || [])
        .filter((l) => l.optin === optin && !chosen.has(l.id) && l.name.toLowerCase().includes(q))
        .slice(0, 10)
        .map((l) => {
          const o = new Option(l.name);
          o.data = new ListTag(l);
          return o;
        }));
    },

    // Switching between joins and confirms keeps only the lists that fit.
    onTrigger() {
      const optin = this.form.trigger_type === 'confirms' ? 'double' : 'single';
      this.form.lists = this.form.lists.filter((l) => !l.optin || l.optin === optin);
    },

    // A transactional template has a subject; use it if there's none yet.
    onTemplate() {
      const t = templates.find((x) => x.id === this.form.template_id);
      if (t && t.type === 'tx' && !this.form.subject.trim()) {
        this.form.subject = t.subject;
      }
    },

    // False, with a message, if a typed-in name isn't a list.
    listsOK() {
      const unknown = [...this.form.lists, ...this.form.add_lists, ...this.form.remove_lists]
        .filter((l) => !listID(l)).map(String);
      if (unknown.length) {
        u.toast(`There's no list called ${unknown.join(', ')}. Choose lists from the suggestions.`, 'danger');
      }
      return unknown.length === 0;
    },

    async onSubmit() {
      if (!this.listsOK()) {
        return;
      }
      if (a.id) {
        await api('automation', `/denma/automations/${a.id}`, 'PUT', this.data());
        u.reload({ message: 'Saved', variant: 'success' });
        return;
      }
      const out = await api('automation', '/denma/automations', 'POST', this.data());
      u.redirect(`${urls.admin}/automations/${out.id}`, { message: 'Saved', variant: 'success' });
    },

    async onToggle() {
      const { active } = this.form;
      try {
        await api('automation', `/denma/automations/${a.id}/status`, 'PUT', { active });
        u.toast(active ? 'On: it runs for what happens from now on.' : 'Off.', 'success');
      } catch {
        this.form.active = !active;
      }
    },

    async onTest() {
      if (!this.listsOK()) {
        return;
      }
      await api('test', '/denma/automations/test', 'POST', { ...this.data(), test_email: this.testEmail });
      u.toast(`Sent a test to ${this.testEmail}.`, 'success');
    },
  };
}

// ---- The schedule (cmd/denma_auto_schedule.go) ----

const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
const NS = 'http://www.w3.org/2000/svg';

// A YYYY-MM-DD day, n days on.
function plusDays(day, n) {
  const d = new Date(`${day}T12:00:00Z`);
  d.setUTCDate(d.getUTCDate() + n);
  return d.toISOString().slice(0, 10);
}

function dayDate(day) {
  return new Date(`${day}T12:00:00Z`);
}

function fmtDay(day, long) {
  return dayDate(day).toLocaleDateString(undefined, long
    ? { weekday: 'long', day: 'numeric', month: 'long', timeZone: 'UTC' }
    : { day: 'numeric', month: 'short', timeZone: 'UTC' });
}

function fmtTime(iso) {
  return new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

// A clean top for the axis: 1, 2, 5 or 10 times a power of ten.
function niceMax(v) {
  if (v <= 4) {
    return 4;
  }
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 5, 10].map((m) => m * p).find((m) => m >= v);
}

// The chart: a column per day, what ran (from the baseline) and what's
// coming up (above it, past a 2px gap). Each day is a button that chooses it.
function scheduleChart(days, today, selected, width, onPick) {
  const W = Math.max(300, Math.round(width || 900));
  const H = W < 500 ? 170 : 200;
  const top = 12;
  const bottom = 26;
  const left = 40;
  const n = days.length;
  const max = niceMax(Math.max(1, ...days.map((d) => d.ran + d.failed + d.upcoming)));
  const scale = (H - top - bottom) / max;
  const slot = (W - left) / n;
  const bw = Math.max(3, Math.min(24, slot * 0.6));
  const base = H - bottom;

  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('class', 'denma-chart');
  svg.setAttribute('role', 'group');
  svg.setAttribute('aria-label', 'People the automations already ran for and are still to run for, per day');
  const node = (parent, tag, attrs, text) => {
    const e = document.createElementNS(NS, tag);
    Object.entries(attrs).forEach(([k, v]) => e.setAttribute(k, v));
    if (text != null) e.textContent = text;
    parent.appendChild(e);
    return e;
  };
  // A column segment, rounded only at its top (4px).
  const seg = (g, x, y, h, cls, round) => {
    if (h <= 0) return;
    const r = round ? Math.min(4, h, bw / 2) : 0;
    node(g, 'path', {
      class: cls,
      d: `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + bw - r}Q${x + bw},${y} ${x + bw},${y + r}V${y + h}Z`,
    });
  };

  [max / 2, max].forEach((v) => {
    const y = base - v * scale;
    node(svg, 'line', { x1: left, x2: W, y1: y, y2: y, class: 'grid' });
    node(svg, 'text', { x: left - 6, y: y + 4, 'text-anchor': 'end', class: 'tick' }, v.toLocaleString());
  });
  node(svg, 'line', { x1: left, x2: W, y1: base, y2: base, class: 'axis' });
  node(svg, 'text', { x: left - 6, y: base + 4, 'text-anchor': 'end', class: 'tick' }, '0');

  const every = Math.ceil(n / Math.max(3, Math.floor((W - left) / 56)));
  days.forEach((d, i) => {
    const x0 = left + slot * i;
    const x = x0 + (slot - bw) / 2;
    const ran = d.ran + d.failed;
    const g = node(svg, 'g', {
      class: `day${d.day === selected ? ' selected' : ''}${d.day === today ? ' today' : ''}`,
      tabindex: 0,
      role: 'button',
      'aria-label': `${fmtDay(d.day, true)}: already ran for ${ran}, still to run for ${d.upcoming}`,
    });
    const tip = [`${fmtDay(d.day, true)}`, `Already ran: ${d.ran}`];
    if (d.failed) tip.push(`${d.failed} failed`);
    if (d.skipped) tip.push(`${d.skipped} skipped`);
    tip.push(`Still to run: ${d.upcoming}`);
    node(g, 'title', {}, tip.join('\n'));
    node(g, 'rect', { x: x0, width: slot, y: top, height: H - top - bottom, class: 'hit' });
    const hr = ran * scale;
    const hu = d.upcoming * scale;
    seg(g, x, base - hr, hr, 'ran', !hu);
    seg(g, x, base - hr - (hr ? 2 : 0) - hu, hu, 'up', true);
    g.addEventListener('click', () => onPick(d.day));
    g.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        onPick(d.day);
      }
    });
    if (d.day === today || i % every === 0) {
      node(svg, 'text', {
        x: x0 + slot / 2, y: H - 8, 'text-anchor': 'middle', class: d.day === today ? 'tick today' : 'tick',
      }, d.day === today ? 'Today' : fmtDay(d.day));
    }
  });
  return svg;
}

function denmaAutoSchedule() {
  return {
    automation: 0,
    from: '',
    to: '',
    today: '',
    selected: '',
    days: [],
    later: 0,
    groups: [],
    open: {},
    canSeePeople: !!window._denmaSchedPeople,
    fmtDay,
    fmtTime,

    init() {
      this.load();
      let w = 0;
      new ResizeObserver(() => {
        const nw = this.$refs.chart.clientWidth;
        if (nw && Math.abs(nw - w) > 20) {
          w = nw;
          this.draw();
        }
      }).observe(this.$refs.chart);
    },

    params(extra) {
      const p = new URLSearchParams({ tz: TZ, automation: this.automation, ...extra });
      return p.toString();
    },

    async load() {
      const out = await api('sched', `/denma/automations/schedule?${this.params(this.from ? { from: this.from } : {})}`);
      Object.assign(this, { days: out.days, later: out.later, today: out.today, from: out.from, to: out.to });
      if (!this.selected || this.selected < this.from || this.selected > this.to) {
        this.selected = this.today >= this.from && this.today <= this.to ? this.today : this.from;
      }
      this.draw();
      this.loadDay();
    },

    draw() {
      if (!this.days.length) return;
      this.$refs.chart.replaceChildren(scheduleChart(this.days, this.today, this.selected, this.$refs.chart.clientWidth, (day) => {
        this.selected = day;
        this.draw();
        this.loadDay();
      }));
    },

    async loadDay() {
      this.open = {};
      this.groups = await api('sched', `/denma/automations/schedule/day?${this.params({ day: this.selected })}`);
    },

    shift(n) {
      this.from = plusDays(this.from, n);
      this.selected = plusDays(this.selected, n);
      this.load();
    },

    goToday() {
      this.from = '';
      this.selected = '';
      this.load();
    },

    dayTitle() {
      if (!this.selected) return '';
      const name = fmtDay(this.selected, true);
      return this.selected === this.today ? `Today, ${name}` : name;
    },

    async fetchPeople(g, kind, page) {
      const p = new URLSearchParams({
        tz: TZ, automation: g.id, day: this.selected, kind, page,
      });
      return api('sched', `/denma/automations/schedule/people?${p}`);
    },

    async togglePeople(g) {
      if (this.open[g.id]) {
        delete this.open[g.id];
        return;
      }
      const [up, ran] = await Promise.all([
        g.upcoming ? this.fetchPeople(g, 'upcoming', 1) : { results: [], total: 0 },
        g.ran + g.failed + g.skipped ? this.fetchPeople(g, 'ran', 1) : { results: [], total: 0 },
      ]);
      this.open[g.id] = {
        upcoming: { rows: up.results, total: up.total, page: 1 },
        ran: { rows: ran.results, total: ran.total, page: 1 },
      };
    },

    async morePeople(g, kind) {
      const o = this.open[g.id][kind];
      const out = await this.fetchPeople(g, kind, o.page + 1);
      o.page += 1;
      o.rows.push(...out.results);
    },

    personNote(p) {
      switch (p.status) {
        case 'upcoming': return '';
        case 'failed': return `failed: ${p.error}`;
        case 'skipped': return p.did || 'skipped';
        default: return p.did;
      }
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaAutomations', denmaAutomations);
  Alpine.data('denmaAutomation', denmaAutomation);
  Alpine.data('denmaAutoSchedule', denmaAutoSchedule);
}, { once: true });
