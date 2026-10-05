// denma: the hub's People page (views/denma-people.html, cmd/denma_people.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

function denmaPeople() {
  return {
    q: '',

    // Whether a row (its data-find text) matches the search.
    shows(text) {
      const q = this.q.trim().toLowerCase();
      return !q || text.includes(q);
    },

    async twofaOff(id, name) {
      if (!(await u.confirm(`Turn off two-factor sign-in for ${name}? They then sign in with their password only, until they turn it on again on their Profile page.`))) {
        return;
      }
      await api('people', `/denma/people/${id}/twofa`, 'DELETE');
      u.reload({ message: `Two-factor sign-in is off for ${name}.` });
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaPeople', denmaPeople);
}, { once: true });
