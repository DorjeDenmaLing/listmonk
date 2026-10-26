// denma: Subscribers -> Tags (views/denma-tags.html, cmd/denma_tags.go):
// adding, renaming and deleting a center's subscriber tags, and their other
// names (cmd/denma_tagaliases.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

function denmaTags() {
  return {
    add: { tag: '' },
    rename: { tag: '', to: '' },
    aliases: { tag: '', names: '' },

    onAdd() {
      this.add = { tag: '' };
      this.$refs.addDialog.showModal();
      this.$nextTick(() => this.$refs.addTag.focus());
    },

    async onSubmitAdd() {
      const data = await api('tags', '/denma/tags', 'POST', { tag: this.add.tag });
      this.$refs.addDialog.close();
      u.reload({ message: `Added ${data.tag}`, variant: 'success' });
    },

    onRename(tag) {
      this.rename = { tag, to: tag };
      this.$refs.renameDialog.showModal();
    },

    async onSubmitRename() {
      const { tag, to } = this.rename;
      const data = await api('tags', '/denma/tags', 'PUT', { action: 'rename', tag, to });
      this.$refs.renameDialog.close();
      u.reload({ message: `Renamed ${tag} for ${data.subscribers} subscriber${data.subscribers === 1 ? '' : 's'}`, variant: 'success' });
    },

    onAliases(tag, names) {
      this.aliases = { tag, names: (names || []).join(', ') };
      this.$refs.aliasesDialog.showModal();
      this.$nextTick(() => this.$refs.aliasNames.focus());
    },

    async onSubmitAliases() {
      const { tag, names } = this.aliases;
      const aliases = names.split(',').map((n) => n.trim()).filter((n) => n);
      await api('tags', '/denma/tags/aliases', 'PUT', { tag, aliases });
      this.$refs.aliasesDialog.close();
      u.reload({ message: `Saved ${tag}'s other names`, variant: 'success' });
    },

    async onDelete(tag, num) {
      const who = num > 0 ? `${num} subscriber${num === 1 ? '' : 's'} and campaigns not yet sent lose it.` : 'No subscriber has it.';
      if (!(await u.confirm(`Delete the tag ${tag}? ${who}`))) {
        return;
      }
      await api('tags', '/denma/tags', 'PUT', { action: 'delete', tag });
      u.reload({ message: `Deleted ${tag}`, variant: 'success' });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaTags', denmaTags);
}, { once: true });
