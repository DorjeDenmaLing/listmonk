// denma: subscriber tags in the admin (cmd/denma_tags.go): the tag picker's
// suggestions (partials/denma/tags.html), and the Subscribers page's bulk
// "Tags" action. Spread into the page components that use them.
import { api, urls } from './main.js';
import * as u from './utils.js';

// The center's tags, fetched once per page (without a toast for users who
// can't list them: they get no suggestions).
let names = null;
function tagNames() {
  if (!names) {
    names = fetch(`${urls.api}/denma/tags`, { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : { data: [] }))
      .then((r) => (r.data || []).map((t) => t.tag))
      .catch(() => []);
  }
  return names;
}

// denmaTagAutocomplete fills a tag picker's suggestions: the center's tags
// not already chosen that contain what's typed.
export async function denmaTagAutocomplete(el) {
  const all = await tagNames();
  const ti = el.closest('ot-taginput');
  const chosen = new Set((ti ? ti.value : []).map(String));
  const q = el.value.trim().toLowerCase();
  el.list.replaceChildren(...all
    .filter((t) => !chosen.has(t) && t.includes(q))
    .slice(0, 10)
    .map((t) => new Option(t)));
}

// denmaBulkTags is the Subscribers page's Tags dialog: adding or removing
// tags for the subscribers picked, or every one matching the search. It uses
// the page's _queryBody().
export function denmaBulkTags() {
  return {
    denmaTagAutocomplete,
    tagBulk: {
      action: 'add', tags: [], allSelected: false, selected: [], num: 0,
    },

    onOpenTags(allSelected, selected, total) {
      this.tagBulk = {
        action: 'add',
        tags: [],
        allSelected,
        selected: selected.map(Number),
        num: allSelected ? total : selected.length,
      };
      this.$refs.tagDialog.showModal();
    },

    async onSubmitTags() {
      const tags = this.tagBulk.tags.map(String);
      if (tags.length === 0) {
        return;
      }
      const data = { action: this.tagBulk.action, tags };
      if (this.tagBulk.allSelected) {
        Object.assign(data, this._queryBody());
      } else {
        data.ids = this.tagBulk.selected;
      }
      await api('tags', '/denma/tags/subscribers', 'PUT', data);
      this.$refs.tagDialog.close();
      const verb = this.tagBulk.action === 'add' ? 'Added' : 'Removed';
      const n = this.tagBulk.num;
      u.reload({ message: `${verb} ${tags.join(', ')} for ${n} subscriber${n === 1 ? '' : 's'}`, variant: 'success' });
    },
  };
}

// denmaWithTags puts a subscriber form's tags (form.tags) into its attributes,
// with any typed into the attributes themselves.
export function denmaWithTags(attribs, tags) {
  const a = { ...attribs };
  const typed = Array.isArray(a.tags) ? a.tags : [];
  delete a.tags;
  const all = [...(tags || []).map(String), ...typed.map(String)];
  if (all.length > 0) {
    a.tags = all;
  }
  return a;
}

// denmaSplitTags takes a subscriber's tags out of its attributes, for the form.
export function denmaSplitTags(attribs) {
  const a = { ...(attribs || {}) };
  const tags = Array.isArray(a.tags) ? a.tags.map(String) : [];
  delete a.tags;
  return { tags, attribs: a };
}
