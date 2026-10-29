// denma: the Subscribers page's Move to list, Remove from list and Delete
// subscriber, and Manage lists' "Run the lists' automations" there and on a
// subscriber's page (cmd/denma_listmove.go, partials/denma/lists-move.html).
// Spread into the page's component (so no getters: spreading copies their
// values); uses its filters and _queryBody().
import { api, urls } from './main.js';
import * as u from './utils.js';

const subs = (n) => `${n} contact${n === 1 ? '' : 's'}`;

// The center's active automations that joining a list starts, fetched once
// (null for users who can't read them: the option then names none).
let autos = null;
let asked = false;
function joinAutomations() {
  asked = true;
  if (!autos) {
    autos = fetch(`${urls.api}/denma/automations`, { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((r) => (r.data || []).filter((a) => a.active && ['joins', 'confirms'].includes(a.trigger_type)))
      .catch(() => null);
  }
  return autos;
}

// denmaAddRun is what Manage lists sends for its automations option (m, the
// dialog's state): for Add only; other actions don't join anyone.
export function denmaAddRun(m) {
  return m.action === 'add' ? { run_automations: !!m.runAutos } : {};
}

export function denmaListMoves() {
  return {
    mv: {
      mode: 'move', id: null, email: '', allSelected: false, selected: [], num: 0,
      fromLists: [], from: null, to: null, preconfirm: false, runAutos: false,
    },
    mvAutos: null,

    mvAllLists() {
      return window._lists || [];
    },

    mvToLists() {
      // Not an API campaign's list (cmd/denma_api.go): only its campaign sends to it.
      return this.mvAllLists().filter((l) => l.id !== this.mv.from && !(l.tags || []).includes('api-send'));
    },

    mvToDouble() {
      const l = this.mvAllLists().find((x) => x.id === this.mv.to);
      return !!l && l.optin === 'double';
    },

    // The names of the automations joining lists (IDs) starts: null if
    // unknown or no list is picked.
    denmaAutosFor(ids) {
      if (!asked) {
        joinAutomations().then((a) => { this.mvAutos = a; });
      }
      const want = (ids || []).filter(Boolean).map(Number);
      if (this.mvAutos === null || want.length === 0) {
        return null;
      }
      return this.mvAutos.filter((a) => (a.list_ids || []).some((id) => want.includes(id))).map((a) => a.name);
    },

    // The automations option's hint, for denmaAutosFor's names.
    denmaAutosHint(names) {
      if (names === null) {
        return "Off, this doesn't count as joining the list, so its automations (welcome e-mails and the like) don't start for them.";
      }
      if (names.length === 0) {
        return 'No active automations start when someone joins this.';
      }
      return `Starts ${names.join(', ')} for them, as if they'd joined. Off, they don't start.`;
    },

    mvWho() {
      return this.mv.email || subs(this.mv.num);
    },

    _mvName(id) {
      const l = this.mvAllLists().find((x) => x.id === id) || this.mv.fromLists.find((x) => x.id === id);
      return l ? l.name : '';
    },

    _mvOpen(mv) {
      const cur = this.subFilters.listID;
      const from = mv.fromLists.some((l) => l.id === cur) ? cur : (mv.fromLists[0] || {}).id || null;
      this.mv = {
        allSelected: false, selected: [], id: null, email: '', to: null, preconfirm: false, runAutos: false, ...mv, from,
      };
      this.$refs.moveDialog.showModal();
    },

    // One subscriber's Move to list or Remove from list (mode), from their lists.
    onOpenMove(mode, id, email, lists) {
      const fromLists = (lists || []).filter((l) => !l.restricted).map((l) => ({ id: l.id, name: l.name }));
      this._mvOpen({
        mode, id, email, selected: [id], num: 1, fromLists,
      });
    },

    // The bulk Move to list, from any list.
    onOpenBulkMove(allSelected, selected, total) {
      this._mvOpen({
        mode: 'move',
        allSelected,
        selected: selected.map(Number),
        num: allSelected ? total : selected.length,
        fromLists: this.mvAllLists().map((l) => ({ id: l.id, name: l.name })),
      });
    },

    async onRemoveFromList(id, email, listID, name) {
      if (!(await u.confirm(`Remove ${email} from ${name}? They stay on their other lists.`))) {
        return;
      }
      await api('subscribers', '/subscribers/lists', 'PUT', { ids: [id], action: 'remove', target_list_ids: [listID] });
      u.reload({ message: `Removed ${email} from ${name}`, variant: 'success' });
    },

    async onSubmitMove() {
      const { mv } = this;
      const from = this._mvName(mv.from);
      if (mv.mode === 'remove') {
        await api('subscribers', '/subscribers/lists', 'PUT', { ids: mv.selected, action: 'remove', target_list_ids: [mv.from] });
        this.$refs.moveDialog.close();
        u.reload({ message: `Removed ${this.mvWho()} from ${from}`, variant: 'success' });
        return;
      }

      const data = {
        from_list_id: mv.from,
        to_list_id: mv.to,
        preconfirm: this.mvToDouble() && mv.preconfirm,
        run_automations: mv.runAutos,
      };
      if (mv.allSelected) {
        Object.assign(data, this._queryBody());
      } else {
        data.ids = mv.selected;
      }
      const res = await api('subscribers', '/denma/subscribers/move', 'PUT', data);
      this.$refs.moveDialog.close();

      const to = this._mvName(mv.to);
      let message = `Moved ${this.mvWho()} from ${from} to ${to}`;
      if (res && typeof res.moved === 'number' && res.moved < mv.num) {
        message = `Moved ${res.moved} of ${subs(mv.num)} from ${from} to ${to}. The others weren't on ${from}, or can't join ${to} without confirming.`;
      }
      u.reload({ message, variant: 'success' });
    },

    async onDeleteForGood(id, email) {
      const msg = `Delete ${email} from the center? This takes them off every list and erases their record and history. To take them off one list, use Remove from list.`;
      if (!(await u.confirm(msg))) {
        return;
      }
      await api('subscribers', `/subscribers/${id}`, 'DELETE');
      u.reload({ message: `Deleted ${email}`, variant: 'success' });
    },
  };
}
