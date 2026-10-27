// denma: automations on Analytics -> Explore (views/analytics.js, hooks), as
// well as campaigns: picked in the same field, sent as ?auto= rather than
// ?id= (views/analytics.html), and drawn beside campaigns
// (denma-campaign-charts.js). cmd/denma_auto_reports.go adds their figures.
import { getJSON } from './denma-stats.js';

const explore = () => window._denmaExplore || {};

// An automation in the field; its label tells it from a campaign.
export class DenmaAutoTag {
  constructor(a) {
    this.id = a.id;
    this.name = a.name;
    this.denmaAuto = true;
  }

  toString() {
    return `Automation: ${this.name}`;
  }
}

// The automations chosen, for the field.
export const denmaChosenAutos = () => (explore().automations || []).map((a) => new DenmaAutoTag(a));

let all = null;

// The field's suggestions for the automations matching what's typed and not
// chosen (ti, the field), for users who can see automations.
export async function denmaAutoOptions(query, ti) {
  if (!explore().can) return [];
  if (!all) {
    all = getJSON('/denma/automations').catch(() => { all = null; return []; });
  }
  const list = (await all) || [];
  const q = query.toLowerCase();
  const chosen = new Set((ti ? ti.value : []).filter((c) => c.denmaAuto).map((c) => c.id));
  return list
    .filter((a) => a.send_email && !chosen.has(a.id) && a.name.toLowerCase().includes(q))
    .slice(0, 10)
    .map((a) => {
      const tag = new DenmaAutoTag(a);
      const o = new Option(String(tag));
      o.data = tag;
      return o;
    });
}
