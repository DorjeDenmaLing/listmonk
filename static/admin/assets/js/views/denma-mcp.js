// denma: the MCP pages (cmd/denma_mcp.go): a center's Config -> MCP, its
// keys for Claude, each a user's, saved as they're changed and shown once
// when made; and the hub's MCP page, the claude.ai connector's secret.

import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

// How long ago an ISO time was: "4 minutes ago".
function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  for (const [n, name] of [[86400, 'day'], [3600, 'hour'], [60, 'minute']]) {
    if (s >= n) {
      const v = Math.floor(s / n);
      return `${v} ${name}${v === 1 ? '' : 's'} ago`;
    }
  }
  return 'just now';
}

// A date as YYYY-MM-DD (UTC).
const ymd = (d) => d.toISOString().slice(0, 10);

// Permissions that only read.
const reads = (p) => /:get(_all|_analytics)?$/.test(p) || p === 'list:get';

const notes = {
  'list:get': 'the lists their list role lets them see',
  'list:manage': 'the lists their list role lets them manage',
  'center:manage': 'Config and Activity (not keys or webhooks)',
};

function denmaMCPKeys() {
  return {
    keys: window._denmaMCPKeys || [],
    users: window._denmaMCPUsers || [],
    groups: window._denmaMCPGroups || [],
    mine: window._denmaMCPMine || [],
    edit: { id: 0 },
    newKey: '',
    ago,

    init() {
      this.edit = this.blank();
    },

    blank() {
      const me = this.users.find((x) => x.id === window._denmaMCPMe) || this.users[0];
      return {
        id: 0, name: '', user_id: me ? me.id : 0, permissions: [], personal_data: false, connector: false, expires_on: '',
      };
    },

    tomorrow() {
      return ymd(new Date(Date.now() + 86400000));
    },

    user() {
      return this.users.find((x) => x.id === this.edit.user_id);
    },

    // Whether the one editing can give a permission, and the key's user has it.
    givable(p) {
      if (!this.mine.includes('*') && !this.mine.includes(p)) {
        return false;
      }
      const usr = this.user();
      if (!usr) {
        return !this.edit.id;
      }
      if (p === 'list:get' || p === 'list:manage') {
        return usr.lists || usr.permissions.includes('*');
      }
      return usr.permissions.includes('*') || usr.permissions.includes(p);
    },

    permNote(p) {
      if (notes[p]) {
        return notes[p];
      }
      return this.user() && !this.givable(p) && (this.mine.includes('*') || this.mine.includes(p)) ? "their role doesn't give it" : '';
    },

    all() {
      return this.groups.flatMap((g) => g.permissions).filter((p) => this.givable(p));
    },

    pickReading() {
      this.edit.permissions = this.all().filter(reads);
    },

    pickAll() {
      this.edit.permissions = this.all();
    },

    optionText(x) {
      const name = x.name && x.name !== x.username ? `${x.name} (${x.username})` : x.username;
      return x.role ? `${name}, ${x.role}` : name;
    },

    userText(k) {
      if (!k.username) {
        return 'A user no longer in the center';
      }
      return k.user_name && k.user_name !== k.username ? `${k.user_name} (${k.username})` : k.username;
    },

    permsText(k) {
      if (k.permissions.every(reads)) {
        return `Reads only: ${k.permissions.join(', ')}`;
      }
      return k.permissions.join(', ');
    },

    seesText(k) {
      const s = [k.personal_data ? 'Claude sees personal data' : 'Stand-ins for personal data'];
      if (k.connector) {
        s.push('usable from claude.ai');
      }
      return s.join('; ');
    },

    madeText(k) {
      let s = `Made ${new Date(k.created_at).toLocaleDateString()}${k.created_by ? ` by ${k.created_by}` : ''}`;
      if (k.expires_at) {
        const at = new Date(k.expires_at);
        s += at < new Date() ? '; expired' : `; expires ${at.toLocaleDateString()}`;
      }
      return s;
    },

    canSave() {
      return this.edit.name.trim() && this.edit.user_id && this.edit.permissions.length;
    },

    copy(text) {
      u.copyToClipboard(text);
    },

    put(k) {
      const i = this.keys.findIndex((x) => x.id === k.id);
      if (i < 0) {
        this.keys.push(k);
      } else {
        this.keys.splice(i, 1, k);
      }
    },

    showKey(k) {
      this.newKey = k.key || '';
      delete k.key;
      if (this.newKey) {
        this.$refs.newKeyDialog.showModal();
      }
    },

    onAdd() {
      this.edit = this.blank();
      this.$refs.keyDialog.showModal();
      this.$nextTick(() => this.$refs.keyName.focus());
    },

    onEdit(k) {
      this.edit = {
        id: k.id,
        name: k.name,
        user_id: k.user_id,
        permissions: [...k.permissions],
        personal_data: k.personal_data,
        connector: k.connector,
        expires_on: k.expires_at ? ymd(new Date(k.expires_at)) : '',
      };
      this.$refs.keyDialog.showModal();
    },

    async onSubmit() {
      const e = this.edit;
      const body = {
        name: e.name.trim(),
        user_id: e.user_id,
        permissions: e.permissions,
        personal_data: e.personal_data,
        connector: e.connector,
        expires_on: e.expires_on,
      };
      const k = e.id
        ? await api('mcp', `/denma/center/mcp-keys/${e.id}`, 'PUT', body)
        : await api('mcp', '/denma/center/mcp-keys', 'POST', body);
      this.$refs.keyDialog.close();
      if (k.key) {
        this.showKey(k);
      } else {
        u.toast(`Saved ${k.name}.`, 'success');
      }
      this.put(k);
    },

    async onNewKey(k) {
      if (!(await u.confirm(`Make a new key for ${k.name}? The old one stops working at once.`))) {
        return;
      }
      const nk = await api('mcp', `/denma/center/mcp-keys/${k.id}/key`, 'POST');
      this.showKey(nk);
      this.put(nk);
    },

    async onDelete(k) {
      if (!(await u.confirm(`Delete ${k.name}? It stops working at once.`))) {
        return;
      }
      await api('mcp', `/denma/center/mcp-keys/${k.id}`, 'DELETE');
      this.keys = this.keys.filter((x) => x.id !== k.id);
      u.toast(`Deleted ${k.name}.`, 'success');
    },
  };
}

function denmaMCPHub() {
  return {
    connector: window._denmaMCPConnector || null,
    secret: '',
    from: ((window._denmaMCPConnector || {}).allowed_from || []).join('\n'),

    connectorText() {
      const c = this.connector;
      const used = c.last_used_at ? `used ${c.uses.toLocaleString()} times, last ${ago(c.last_used_at)}` : 'not used yet';
      return `Made ${new Date(c.created_at).toLocaleString()}${c.created_by ? ` by ${c.created_by}` : ''}; ${used}.`;
    },

    copy(text) {
      u.copyToClipboard(text);
    },

    async onNewSecret() {
      if (this.connector && !(await u.confirm('Make a new connector secret? The old one stops working at once: put the new one in the MCP server\'s settings.'))) {
        return;
      }
      const c = await api('connector', '/denma/mcp/connector', 'POST');
      this.secret = c.secret;
      delete c.secret;
      this.connector = c;
      this.$refs.secretDialog.showModal();
    },

    async onSaveFrom() {
      const c = await api('connector', '/denma/mcp/connector', 'PUT', {
        allowed_from: this.from.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean),
      });
      this.connector = c;
      this.from = (c.allowed_from || []).join('\n');
      u.toast(c.allowed_from && c.allowed_from.length ? 'The secret works only from those addresses.' : 'The secret works from anywhere.', 'success');
    },

    async onDelete() {
      if (!(await u.confirm('Delete the connector secret? claude.ai stops working with listmonk; Claude Code\'s keys go on working.'))) {
        return;
      }
      await api('connector', '/denma/mcp/connector', 'DELETE');
      this.connector = null;
      u.toast('Deleted the connector secret.', 'success');
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaMCPKeys', denmaMCPKeys);
  Alpine.data('denmaMCPHub', denmaMCPHub);
}, { once: true });
