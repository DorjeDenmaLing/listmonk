// denma: the hub's Maintenance page acts on one center or on all of them
// (cmd/denma_maintenance.go). Mixed into listmonk's maintenance view
// (views/maintenance.js); centers come from the page (window._denmaCenters),
// and there are none without centers, where it changes nothing.
import { centerPicker } from './denma-hub-ui.js';

export function denmaMaintenance() {
  const centers = window._denmaCenters || [];
  return {
    ...centerPicker([{ slug: '', name: 'All centers' }]),
    centers,
    center: '',

    onCenter() {},

    // The center= parameter, after sep ("?" or "&").
    denmaQ(sep) {
      return centers.length && this.center ? `${sep}center=${encodeURIComponent(this.center)}` : '';
    },

    // The confirmation's question, naming where it applies.
    denmaConfirm() {
      if (!centers.length) {
        return undefined;
      }
      return this.center
        ? `Delete this in ${this.pickName()}?`
        : `Delete this in all ${centers.length} running centers?`;
    },
  };
}
