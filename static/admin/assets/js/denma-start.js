// denma: the confirmation before a campaign is started or scheduled, with
// what's about to be sent and to how many (partials/denma/start-confirm.html,
// cmd/denma_start.go). Mixed into the campaign page (views/campaign.js) and
// the Campaigns list (views/campaigns.js), in place of listmonk's plain
// "Are you sure?".
import { api } from './main.js';

const FORMATS = {
  richtext: 'Rich text', html: 'Raw HTML', markdown: 'Markdown', plain: 'Plain text', visual: 'Visual',
};

export function denmaStartConfirm() {
  return {
    // What the dialog shows while it's open.
    startConfirm: null,
    _startResolve: null,

    // denmaConfirmStart shows campaign c's details (as the API has a
    // campaign; the template's name is c.template_name, if known) before it's
    // started, or scheduled for c.send_at, and resolves to whether it was
    // confirmed.
    denmaConfirmStart(c, schedule) {
      const lists = (c.lists || []).filter((l) => l.id);
      const tags = c.subscriber_tags || [];
      this.startConfirm = {
        schedule,
        name: c.name || '',
        subject: c.subject || '',
        previewText: c.content_type !== 'plain' ? (c.preview_text || '').trim() : '',
        from: c.from_email || '',
        replyTo: (c.reply_to || '').trim(),
        lists: lists.map((l) => l.name || `#${l.id}`),
        tags,
        when: schedule && c.send_at
          ? new Date(c.send_at).toLocaleString(undefined, { dateStyle: 'full', timeStyle: 'short' })
          : 'Now',
        format: [FORMATS[c.content_type] || c.content_type, c.template_name].filter(Boolean).join(', '),
        attachments: (c.media || []).filter((m) => m.id).length,
        check: null, // cmd/denma_start.go's answer
        loading: true,
      };
      this.$refs.startDialog.showModal();

      const q = new URLSearchParams();
      lists.forEach((l) => q.append('list_id', l.id));
      tags.forEach((t) => q.append('tag', t));
      if (schedule && c.send_at) {
        q.append('at', new Date(c.send_at).toISOString());
      }
      const shown = this.startConfirm;
      api('denma.start', `/denma/campaigns/${c.id}/start-check?${q}`)
        .then((d) => { shown.check = d; })
        .catch(() => {})
        .finally(() => { shown.loading = false; });

      return new Promise((resolve) => { this._startResolve = resolve; });
    },

    // The center's reply-to when the campaign has none, or the From.
    denmaStartReplyTo() {
      const s = this.startConfirm;
      if (s.replyTo) {
        return s.replyTo;
      }
      if (s.check?.reply_to) {
        return `${s.check.reply_to} (the center's)`;
      }
      return s.check ? 'The From address' : '…';
    },

    // Whether the center's daily cap holds some of it back.
    denmaStartOverCap() {
      const c = this.startConfirm?.check;
      return c && c.sending.left >= 0 && c.recipients > c.sending.left;
    },

    // When the center's sending hours hold it back, if they do.
    denmaStartGoes() {
      const h = this.startConfirm?.check?.hours;
      if (!h || !h.goes) {
        return '';
      }
      return new Date(h.goes).toLocaleString(undefined, { dateStyle: 'full', timeStyle: 'short' });
    },

    // The dialog closed: confirmed, or not (Cancel, Esc).
    denmaStartDone(ok) {
      const resolve = this._startResolve;
      this._startResolve = null;
      if (this.$refs.startDialog.open) {
        this.$refs.startDialog.close();
      }
      if (resolve) {
        resolve(ok);
      }
    },
  };
}
