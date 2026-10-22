// denma: the visual e-mail builder (listmonk's email-builder, in an iframe):
// on phones (assets/static/denma-email-builder.css), and for the Text block's
// Markdown: on in new blocks, a help link, and a formatting bar with its
// shortcuts. Called by visual-editor.js as its frame loads. The builder's bundle is unchanged:
// these find its panel by its labels ("Content", "Markdown").

// The width up to which the builder's settings panel opens over the e-mail
// (the stylesheet's media query).
const PHONE = 600;

// GitHub's guide to Markdown, for the Text block's Markdown switch.
const MARKDOWN_HELP = 'https://docs.github.com/en/get-started/writing-on-github/getting-started-with-writing-and-formatting-on-github/basic-writing-and-formatting-syntax';

const svg = (body) => `<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>`;

const MAC = /Mac|iPhone|iPad/.test(navigator.platform);
const KEY = MAC ? '⌘' : 'Ctrl+';

// The bar's buttons: what each does (FORMATS), its icon and shortcut key.
const BUTTONS = [
  { f: 'bold', label: 'Bold', key: 'b', icon: svg('<path d="M6 4h8a4 4 0 0 1 0 8H6z"/><path d="M6 12h9a4 4 0 0 1 0 8H6z"/>') },
  { f: 'italic', label: 'Italic', key: 'i', icon: svg('<line x1="19" y1="4" x2="10" y2="4"/><line x1="14" y1="20" x2="5" y2="20"/><line x1="15" y1="4" x2="9" y2="20"/>') },
  { f: 'link', label: 'Link', key: 'k', icon: svg('<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>') },
  { f: 'bullets', label: 'Bulleted list', icon: svg('<line x1="9" y1="6" x2="20" y2="6"/><line x1="9" y1="12" x2="20" y2="12"/><line x1="9" y1="18" x2="20" y2="18"/><circle cx="4.5" cy="6" r="1"/><circle cx="4.5" cy="12" r="1"/><circle cx="4.5" cy="18" r="1"/>') },
  { f: 'numbers', label: 'Numbered list', icon: svg('<line x1="10" y1="6" x2="21" y2="6"/><line x1="10" y1="12" x2="21" y2="12"/><line x1="10" y1="18" x2="21" y2="18"/><path d="M4 6h1v4"/><path d="M4 10h2"/><path d="M6 18H4c0-1 2-2 2-3s-1-1.5-2-1"/>') },
];

// replace puts text in place of the textarea's start-end, as typing does (so
// React sees it, and undo works), then selects selStart-selEnd.
function replace(ta, start, end, text, selStart, selEnd) {
  const doc = ta.ownerDocument;
  ta.focus();
  ta.setSelectionRange(start, end);
  if (!doc.execCommand('insertText', false, text)) {
    const set = Object.getOwnPropertyDescriptor(doc.defaultView.HTMLTextAreaElement.prototype, 'value').set;
    set.call(ta, ta.value.slice(0, start) + text + ta.value.slice(end));
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  }
  ta.setSelectionRange(selStart, selEnd);
}

// stars counts the *s running back from i (dir -1) or on from i (dir 1).
function stars(v, i, dir) {
  let n = 0;
  for (let j = i; j >= 0 && j < v.length && v[j] === '*'; j += dir) n += 1;
  return n;
}

// has tells whether runs of l and r *s on either side hold bold (n 2) or
// italic (n 1): two or more is bold, an odd number italic (*** is both).
const has = (l, r, n) => (n === 2 ? Math.min(l, r) >= 2 : Math.min(l, r) % 2 === 1);

// emphasis makes the selection bold (n 2) or italic (n 1) with *s, or takes
// that away if it has it, inside or just outside the selection. Its spaces
// stay outside; with nothing selected, the cursor is left between the marks.
function emphasis(ta, n) {
  const v = ta.value;
  let s = ta.selectionStart;
  let e = ta.selectionEnd;
  while (s < e && /\s/.test(v[s])) s += 1;
  while (e > s && /\s/.test(v[e - 1])) e -= 1;
  const sel = v.slice(s, e);
  const mark = '*'.repeat(n);
  const inL = stars(sel, 0, 1);
  const inR = stars(sel, sel.length - 1, -1);
  if (inL < sel.length && has(inL, inR, n)) {
    const inner = sel.slice(n, -n);
    replace(ta, s, e, inner, s, s + inner.length);
  } else if (has(stars(v, s - 1, -1), stars(v, e, 1), n)) {
    replace(ta, s - n, e + n, sel, s - n, e - n);
  } else {
    replace(ta, s, e, mark + sel + mark, s + n, e + n);
  }
}

// list makes the selected lines a list, or plain lines again if they're all
// already that list.
function list(ta, numbered) {
  const v = ta.value;
  const s = v.lastIndexOf('\n', ta.selectionStart - 1) + 1;
  // A selection ending at a line's start doesn't take that line.
  let end = ta.selectionEnd;
  if (end > ta.selectionStart && v[end - 1] === '\n') end -= 1;
  let e = v.indexOf('\n', end);
  if (e < 0) e = v.length;
  const lines = v.slice(s, e).split('\n');
  const item = numbered ? /^\s*\d+[.)]\s+/ : /^\s*[-*+]\s+/;
  const any = /^\s*(?:[-*+]|\d+[.)])\s+/;
  const filled = lines.filter((l) => l.trim());
  let out;
  if (filled.length && filled.every((l) => item.test(l))) {
    out = lines.map((l) => l.replace(item, ''));
  } else {
    let i = 0;
    out = lines.map((l) => (l.trim() ? `${numbered ? `${i += 1}.` : '-'} ${l.replace(any, '')}` : l));
  }
  const text = out.join('\n');
  replace(ta, s, e, text, s, s + text.length);
}

// link makes the selection a link, asking for its address.
function link(ta) {
  const win = ta.ownerDocument.defaultView;
  const s = ta.selectionStart;
  const e = ta.selectionEnd;
  const url = (win.prompt('Link address', 'https://') || '').trim();
  if (!url || url === 'https://') {
    ta.focus();
    ta.setSelectionRange(s, e);
    return;
  }
  const text = ta.value.slice(s, e) || 'link text';
  replace(ta, s, e, `[${text}](${url})`, s + 1, s + 1 + text.length);
}

const FORMATS = {
  bold: (ta) => emphasis(ta, 2),
  italic: (ta) => emphasis(ta, 1),
  link,
  bullets: (ta) => list(ta, false),
  numbers: (ta) => list(ta, true),
};

// markdownLabels are the Markdown switch's labels in the settings panel (one,
// while a Text block is selected).
function markdownLabels(doc) {
  return [...doc.querySelectorAll('.MuiFormControlLabel-root')]
    .filter((l) => l.querySelector('.MuiFormControlLabel-label')?.textContent.trim() === 'Markdown');
}

// contentBox is the Content textarea above a Markdown switch that's on.
function contentBox(label) {
  const field = label.previousElementSibling;
  if (!label.querySelector('input')?.checked || !field?.classList.contains('MuiTextField-root')) {
    return null;
  }
  return field.querySelector('textarea:not([aria-hidden])');
}

function makeBar(doc) {
  const bar = doc.createElement('div');
  bar.className = 'denma-md-bar';
  bar.setAttribute('role', 'toolbar');
  bar.setAttribute('aria-label', 'Markdown formatting');
  BUTTONS.forEach((b) => {
    const btn = doc.createElement('button');
    btn.type = 'button';
    btn.dataset.format = b.f;
    btn.title = b.key ? `${b.label} (${KEY}${b.key.toUpperCase()})` : b.label;
    btn.setAttribute('aria-label', b.label);
    btn.innerHTML = b.icon;
    // The textarea keeps the focus, and its selection.
    btn.addEventListener('mousedown', (ev) => ev.preventDefault());
    btn.addEventListener('click', () => {
      const ta = bar.nextElementSibling?.querySelector('textarea:not([aria-hidden])');
      if (ta) {
        FORMATS[b.f](ta);
      }
    });
    bar.appendChild(btn);
  });
  return bar;
}

// markdownTools gives the Markdown switch its help link, and the Content box
// above it its bar while Markdown is on. React reuses the panel's elements for
// other blocks, so a bar that's no longer above such a box goes.
function markdownTools(doc) {
  doc.querySelectorAll('.denma-md-bar').forEach((bar) => {
    const label = bar.nextElementSibling?.nextElementSibling;
    if (!label || !markdownLabels(doc).includes(label) || !contentBox(label)) {
      bar.remove();
    }
  });
  markdownLabels(doc).forEach((label) => {
    if (!label.querySelector('.denma-md-help')) {
      // A link in a <label> doesn't toggle its switch.
      const a = doc.createElement('a');
      a.className = 'denma-md-help';
      a.href = MARKDOWN_HELP;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.title = 'How to write Markdown (GitHub)';
      a.setAttribute('aria-label', a.title);
      a.innerHTML = svg('<circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/>');
      label.appendChild(a);
    }
    const field = label.previousElementSibling;
    if (contentBox(label) && !field.previousElementSibling?.classList.contains('denma-md-bar')) {
      field.before(makeBar(doc));
    }
  });
}

// shortcut runs the bar's Ctrl/⌘+B, I and K in a Content box with Markdown on.
function shortcut(ev) {
  if (!(MAC ? ev.metaKey : ev.ctrlKey) || ev.altKey || ev.shiftKey) {
    return;
  }
  const b = BUTTONS.find((x) => x.key === ev.key.toLowerCase());
  const ta = ev.target;
  if (!b || ta.tagName !== 'TEXTAREA') {
    return;
  }
  const bar = ta.closest('.MuiTextField-root')?.previousElementSibling;
  if (!bar?.classList.contains('denma-md-bar')) {
    return;
  }
  ev.preventDefault();
  FORMATS[b.f](ta);
}

// The text the builder's Add block menu gives a new Text block.
const NEW_TEXT = 'My new text block';

// markdownOn turns Markdown on in Text blocks just added (with the menu's
// text, and no Markdown setting yet; source is the builder's document). Those
// saved, copied or switched off keep theirs.
function markdownOn(win, source) {
  const add = {};
  Object.entries(source || {}).forEach(([id, b]) => {
    if (b?.type === 'Text' && b.data?.props?.text === NEW_TEXT && b.data.props.markdown === undefined) {
      add[id] = { ...b, data: { ...b.data, props: { ...b.data.props, markdown: true } } };
    }
  });
  if (Object.keys(add).length) {
    win.EmailBuilder?.setDocument(add);
  }
}

// denmaEditorFrame gives the builder's frame our stylesheet and the Markdown
// tools and, on a phone, starts it with the settings panel closed (it opens
// when a block is tapped).
export function denmaEditorFrame(iframe, base) {
  const doc = iframe.contentDocument;
  const v = new URL(document.querySelector('script[src*="/js/visual-editor.js"]')?.src || location.href).searchParams.get('v');
  const css = doc.createElement('link');
  css.rel = 'stylesheet';
  css.href = new URL(`${base}/../denma-email-builder.css${v ? `?v=${v}` : ''}`, location.href).href;
  doc.head.appendChild(css);

  // The settings panel is made again for each block selected, and the switch
  // changes only its classes.
  new MutationObserver(() => markdownTools(doc)).observe(doc.body, {
    childList: true, subtree: true, attributes: true, attributeFilter: ['class'],
  });
  doc.addEventListener('keydown', shortcut, true);
  // After the builder's own change is done.
  iframe.parentElement.addEventListener('editor-change', (ev) => {
    setTimeout(() => markdownOn(iframe.contentWindow, JSON.parse(ev.detail.source)));
  });

  if (iframe.contentWindow.innerWidth > PHONE) {
    return;
  }
  // Its button shows "close" (LastPage) while the panel is open.
  const close = () => {
    const btn = doc.querySelector('[data-testid="LastPageOutlinedIcon"]')?.closest('button');
    btn?.click();
    return !!btn;
  };
  const obs = new MutationObserver(() => {
    if (close()) {
      obs.disconnect();
    }
  });
  obs.observe(doc.body, { childList: true, subtree: true });
  setTimeout(() => obs.disconnect(), 10000);
}
