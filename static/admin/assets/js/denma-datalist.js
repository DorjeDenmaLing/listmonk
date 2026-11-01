// denma: a datalist's options are replaced only when they change.
export function denmaReplaceOptions(list, opts) {
  const same = list.options.length === opts.length && opts.every((o, i) => list.options[i].value === o.value);
  if (!same) {
    list.replaceChildren(...opts);
  }
}
