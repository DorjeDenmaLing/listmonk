// denma: a datalist's options are replaced only when they change. listmonk
// refills Analytics' campaigns list on every click and focus, and replacing
// it while the browser showed it made it flicker and its options hard to
// click (views/analytics.js).
export function denmaReplaceOptions(list, opts) {
  const same = list.options.length === opts.length && opts.every((o, i) => list.options[i].value === o.value);
  if (!same) {
    list.replaceChildren(...opts);
  }
}
