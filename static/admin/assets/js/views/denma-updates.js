// denma: new commits on the fork's branch, and updating to them (cmd/denma_updates.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';
import { getJSON } from '../denma-stats.js';

const POLL_MS = 15000;
const CHECK_POLL_MS = 3000; // while the server's updater looks for new commits

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaUpdates', () => ({
    info: null,
    loading: false,
    timer: null,

    async init() {
      await this.load(false);
    },

    get checking() {
      return this.loading || !!this.info?.checking;
    },

    get busy() {
      return ['checking', 'building', 'installing'].includes(this.info?.update?.state);
    },

    get newer() {
      return this.info?.status === 'ahead' && !this.info?.error;
    },

    get shown() {
      return this.info?.commits || [];
    },

    short: (sha) => (sha || '').slice(0, 8),

    get stateText() {
      const s = this.info?.update;
      if (!s?.state) {
        return '';
      }
      const what = {
        checking: 'Checking the commit',
        building: 'Testing and building, which takes several minutes',
        installing: 'Installing: a backup, then a restart',
        done: 'The last update finished',
        failed: 'The last update failed',
      }[s.state] || s.state;
      return `${what}${s.commit ? ` (${this.short(s.commit)})` : ''}${s.message ? `: ${s.message}` : ''}`;
    },

    async load(refresh) {
      clearTimeout(this.timer);
      this.loading = true;
      try {
        this.info = await getJSON(`/denma/hub/update${refresh ? '?refresh=1' : ''}`);
      } catch {
        // The server restarts during an update.
      }
      this.loading = false;
      if (this.info?.checking) {
        this.timer = setTimeout(() => this.load(false), CHECK_POLL_MS);
      } else if (this.busy) {
        this.timer = setTimeout(() => this.load(false), POLL_MS);
      }
    },

    async onUpdate() {
      const n = this.info.ahead;
      const what = `${n} new ${n === 1 ? 'commit' : 'commits'}`;
      if (!(await u.confirm(`Update the server with ${what}? They're tested first. Then every center restarts, usually within a minute, after a backup.`))) {
        return;
      }
      await api('update', '/denma/hub/update', 'POST', { commit: this.info.head });
      u.toast('Update asked for', 'success');
      this.info.update = { state: 'checking', commit: this.info.head };
      this.timer = setTimeout(() => this.load(false), POLL_MS);
    },
  }));
}, { once: true });
