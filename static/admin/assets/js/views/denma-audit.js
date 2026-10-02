// denma: the activity log (partials/denma/audit.html, cmd/denma_audit.go):
// a center's own on its Advanced page, every center's on the hub's Activity
// page. Newest first, 50 at a time.
import Alpine from 'alpinejs';
import { api } from '../main.js';

const PAGE = 50;

function denmaAudit(hub = false) {
  return {
    hub,
    rows: [],
    q: '',
    center: '',
    open: {},
    more: false,
    loading: false,

    init() {
      this.load();
    },

    reload() {
      this.rows = [];
      this.open = {};
      this.load();
    },

    async load() {
      const p = new URLSearchParams({ limit: PAGE });
      if (this.rows.length) {
        p.set('before', this.rows[this.rows.length - 1].id);
      }
      if (this.q.trim()) {
        p.set('q', this.q.trim());
      }
      if (this.hub && this.center) {
        p.set('center', this.center);
      }
      this.loading = true;
      try {
        const page = await api('audit', `/denma/audit?${p.toString()}`);
        this.rows = this.rows.concat(page || []);
        this.more = (page || []).length === PAGE;
      } finally {
        this.loading = false;
      }
    },

    when(t) {
      return new Date(t).toLocaleString([], {
        year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', second: '2-digit',
      });
    },

    who(r) {
      if (r.user_name && r.user_name !== r.username) {
        return `${r.user_name} (${r.username})`;
      }
      return r.username || 'Unknown';
    },

    details(r) {
      const d = { request: `${r.method} ${r.path}`, status: r.status, ...(r.details || {}) };
      return JSON.stringify(d, null, 2);
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaAudit', denmaAudit);
}, { once: true });
