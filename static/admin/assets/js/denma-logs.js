// denma: each center's log lines carry its address first ("boulder: ...",
// cmd/denma_logs.go). The hub's Logs page (views/logs.html, views/logs.js)
// shows them all, and can show one center's, or the hub's own.
import { centerPicker } from './denma-hub-ui.js';

const reTag = /^([a-z0-9]+(?:-[a-z0-9]+)*): /;

// Mixed into the Logs page's view; nothing changes without centers.
export function denmaLogs() {
  const centers = window._denmaCenters || [];
  const slugs = new Set(centers.map((c) => c.slug));
  return {
    ...centerPicker([{ slug: '', name: 'Everything' }, { slug: 'hub', name: 'The hub' }]),
    centers,
    center: '',

    onCenter() {
      this.$nextTick(() => this.scrollToBottom());
    },

    // The lines to show: a center's (its own), the hub's (no center's), or all.
    denmaShown(lines) {
      if (!centers.length || !this.center) {
        return lines;
      }
      return lines.filter((l) => {
        const m = l.message.match(reTag);
        const own = m && slugs.has(m[1]) ? m[1] : 'hub';
        return own === this.center;
      });
    },
  };
}
