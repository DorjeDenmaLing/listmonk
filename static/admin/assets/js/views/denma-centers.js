// denma: the hub's new center form (views/denma-center-new.html,
// cmd/denma_hub.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

// A center's address from its name: "Karmê Chöling" -> "karme-choling".
function slugify(name) {
  return name.normalize('NFKD').replace(/[\u0300-\u036f]/g, '').toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 50)
    .replace(/-+$/, '');
}

// The shared sender address with the center's name, as the server defaults it.
function fromEmail(name) {
  const base = window._denmaFromEmail || '';
  const m = base.match(/<([^>]+)>/);
  return `"${name || 'Center name'}" <${m ? m[1] : base}>`;
}

// The new center form.
function denmaNewCenter() {
  return {
    form: {
      name: '', slug: '', from_email: '', admin_name: '', admin_email: '', admin_username: '',
    },
    slugEdited: false,
    created: null,

    get fromPlaceholder() {
      return fromEmail(this.form.name);
    },

    onName() {
      if (!this.slugEdited) {
        this.form.slug = slugify(this.form.name);
      }
    },

    async onSubmit() {
      this.created = await api('centers', '/denma/centers', 'POST', this.form);
    },

    copy() {
      u.copyToClipboard(this.created.invite_url);
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaNewCenter', denmaNewCenter);
}, { once: true });
