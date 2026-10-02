// denma: a center's automations (views/denma-automations.html) and an
// automation's page (views/denma-automation.html); cmd/denma_automations.go.
import Alpine from 'alpinejs';
import { api, urls, ListTag } from '../main.js';
import * as u from '../utils.js';

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
      if (!(await u.confirm(`Delete ${name}? Its record of whom it sent goes too.`))) {
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

// An automation's page.
function denmaAutomation() {
  const a = window._denmaAutomation || {};
  const templates = window._denmaAutoTemplates || [];
  const lists = (a.list_ids || []).map((id) => (window._lists || []).find((l) => l.id === id)).filter(Boolean);
  const mins = a.delay_minutes || 0;
  let unit = 'now';
  if (mins) {
    unit = (mins % 1440 === 0 && 'days') || (mins % 60 === 0 && 'hours') || 'minutes';
  }

  return {
    form: {
      name: a.name || '',
      lists: lists.map((l) => new ListTag(l)),
      template_id: a.template_id || 0,
      subject: a.subject || '',
      from_email: a.from_email || '',
      active: a.id ? a.active : true,
    },
    unit,
    wait: mins ? mins / UNITS[unit] : 1,
    testEmail: window._denmaTestEmail || '',

    data() {
      const delay = this.unit === 'now' ? 0 : Math.round((Number(this.wait) || 0) * UNITS[this.unit]);
      const { lists: chosen, ...rest } = this.form;
      return { ...rest, list_ids: chosen.map(listID).filter(Boolean), delay_minutes: delay };
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
      const unknown = this.form.lists.filter((l) => !listID(l)).map(String);
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
        u.toast(active ? 'On: it sends to people who join from now on.' : 'Off.', 'success');
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

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaAutomations', denmaAutomations);
  Alpine.data('denmaAutomation', denmaAutomation);
}, { once: true });
