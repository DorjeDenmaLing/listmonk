// denma: subscriber tags in the admin (cmd/denma_tags.go): the tag picker and the bulk Tags action.
import { api, urls } from './main.js';
import * as u from './utils.js';

// The center's tags, fetched once per page.
let names = null;
let known = null;
function tagNames() {
  if (!names) {
    names = fetch(`${urls.api}/denma/tags`, { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((r) => {
        known = (r.data || []).map((t) => t.tag);
        return known;
      })
      .catch(() => []);
  }
  return names;
}

// onlyKnown stops a picker adding typed text that isn't one of the center's tags.
function onlyKnown(e) {
  if ((e.key !== 'Enter' && e.key !== ',') || e.target.tagName !== 'INPUT' || !known) {
    return;
  }
  const input = e.target;
  const typed = input.value.trim().toLowerCase().replace(/\s+/g, ' ');
  if (!typed) {
    return;
  }
  if (known.includes(typed)) {
    input.value = typed;
    return;
  }
  e.preventDefault();
  e.stopPropagation();
  input.setCustomValidity(`There's no tag ${typed}. Pick one from the list, or add it under Contacts -> Tags.`);
  input.reportValidity();
}

// denmaTagAutocomplete fills a tag picker's suggestions with the center's unchosen tags.
export async function denmaTagAutocomplete(el) {
  el.setCustomValidity('');
  const ti = el.closest('ot-taginput');
  if (ti && !ti.denmaOnlyKnown) {
    ti.denmaOnlyKnown = true;
    ti.addEventListener('keydown', onlyKnown, true);
    el.addEventListener('blur', () => el.setCustomValidity(''));
  }
  const all = await tagNames();
  const chosen = new Set((ti ? ti.value : []).map(String));
  const q = el.value.trim().toLowerCase();
  el.list.replaceChildren(...all
    .filter((t) => !chosen.has(t) && t.includes(q))
    .slice(0, 10)
    .map((t) => new Option(t)));
}

// denmaBulkTags is the Subscribers page's Tags dialog: adds or removes tags.
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
      u.reload({ message: `${verb} ${tags.join(', ')} for ${n} contact${n === 1 ? '' : 's'}`, variant: 'success' });
    },
  };
}

// denmaWithTags puts a subscriber form's tags into its attributes.
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

// denmaSplitTags takes a subscriber's tags out of its attributes.
export function denmaSplitTags(attribs) {
  const a = { ...(attribs || {}) };
  const tags = Array.isArray(a.tags) ? a.tags.map(String) : [];
  delete a.tags;
  return { tags, attribs: a };
}
