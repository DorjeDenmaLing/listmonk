// denma: a subscriber who opted out (views/subscriber.html,
// cmd/denma_optouts.go): sending them, when they've asked, the confirmation
// to subscribe again (cmd/denma_resubscribe.go).
import Alpine from 'alpinejs';
import { api } from '../main.js';
import * as u from '../utils.js';

const RESULTS = {
  sent: ["Sent. They're subscribed once they click it.", 'success'],
  already_sent: ['One was sent to them in the last day, which they can still use.', 'success'],
  complained: ['They marked an e-mail as spam, so they can\'t be subscribed again.', 'danger'],
  bounced: ['Their address bounced or can\'t receive mail, so nothing can be sent to it.', 'danger'],
  not_blocklisted: ['They\'re not blocklisted.', 'success'],
};

function denmaOptOut() {
  return {
    lists: window._lists || [],
    listIDs: [],

    onOpen() {
      this.listIDs = [];
      this.$refs.resubDialog.showModal();
    },

    async onSend() {
      const data = await api('resubscribe', '/denma/subscribers/resubscribe', 'POST', {
        email: window._denmaOptOutEmail,
        list_ids: this.listIDs,
      });
      this.$refs.resubDialog.close();
      const [msg, variant] = RESULTS[data.status] || [`Done (${data.status}).`, 'success'];
      u.toast(msg, variant, 8000);
    },
  };
}

document.addEventListener('alpine:init', () => {
  Alpine.data('denmaOptOut', denmaOptOut);
}, { once: true });
