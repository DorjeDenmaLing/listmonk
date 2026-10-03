// denma: Subscribers -> Tags (views/denma-tags.html, cmd/denma_tags.go):
// renaming and deleting a center's subscriber tags.
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

function denmaTags() {
  return {
    rename: { tag: '', to: '' },

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

    async onDelete(tag, num) {
      if (!(await u.confirm(`Remove the tag ${tag} from ${num} subscriber${num === 1 ? '' : 's'}? Campaigns not yet sent lose it too.`))) {
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
