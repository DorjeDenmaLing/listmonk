// denma: System's form taking an address off the shared blocklist (cmd/denma_blocklist_admin.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaSharedUnblock', () => ({
    email: '',
    async onSubmit() {
      await api('unblock', '/denma/shared-blocklist/remove', 'POST', { email: this.email });
      u.toast('Taken off the shared blocklist', 'success');
      this.email = '';
    },
  }));
}, { once: true });
