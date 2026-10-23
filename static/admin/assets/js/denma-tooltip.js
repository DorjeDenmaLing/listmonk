// denma: the admin's tooltips, in place of Oat's. Oat turns each title into
// data-tooltip and draws it with CSS on the element itself, on one line: a
// table that scrolls sideways or a clipped container cut it off, and long
// ones ran off the screen. This draws the same data-tooltip in one popover
// in the top layer, above everything, wrapped and kept on the screen: above
// the element (below with data-tooltip-placement="bottom", or when there's no
// room above), after a short pause on hover, at once on keyboard focus, with
// its line breaks. Iframes' titles and tooltips that only repeat what's shown
// aren't. Oat's own are hidden (denma.css).

const DELAY = 400;
const GAP = 8;
const EDGE = 8;

let tip;
let current = null;
let timer = null;

function el() {
  if (!tip) {
    tip = document.createElement('div');
    tip.className = 'denma-tip';
    tip.setAttribute('popover', 'manual');
    tip.setAttribute('role', 'tooltip');
    document.body.appendChild(tip);
  }
  return tip;
}

function show(target) {
  const text = target.getAttribute('data-tooltip');
  if (!text || !target.isConnected) return;
  const t = el();
  t.textContent = text;
  if (!t.matches(':popover-open')) t.showPopover();
  current = target;

  const r = target.getBoundingClientRect();
  const w = t.offsetWidth;
  const h = t.offsetHeight;
  const below = target.getAttribute('data-tooltip-placement') === 'bottom' || r.top - GAP - h < EDGE;
  const top = below ? r.bottom + GAP : r.top - GAP - h;
  const mid = r.left + r.width / 2;
  const left = Math.min(Math.max(EDGE, mid - w / 2), window.innerWidth - EDGE - w);
  t.style.top = `${Math.round(top)}px`;
  t.style.left = `${Math.round(left)}px`;
  t.style.setProperty('--denma-tip-arrow', `${Math.round(Math.min(Math.max(10, mid - left), w - 10))}px`);
  t.classList.toggle('below', below);
}

function hide() {
  clearTimeout(timer);
  current = null;
  if (tip && tip.matches(':popover-open')) tip.hidePopover();
}

// Text as shown, without leading symbols (a status's dot).
const plain = (s) => s.replace(/\s+/g, ' ').trim().replace(/^[^\p{L}\p{N}]+/u, '');

// Whether a tooltip only repeats what the element shows: its own text, or
// listmonk's "View <its text>" on filter links. Unless the text is cut off.
function repeats(t, text) {
  const shown = plain(t.innerText || '');
  if (!shown || t.scrollWidth > t.clientWidth) return false;
  const tip = plain(text);
  return tip === shown || (tip.endsWith(` ${shown}`) && tip.split(' ').length === shown.split(' ').length + 1);
}

// The element whose tooltip to show: not an iframe (its title names it for
// screen readers), nor one that repeats itself.
function owner(e) {
  const t = e.target instanceof Element ? e.target.closest('[data-tooltip]') : null;
  if (!t || t.tagName === 'IFRAME' || repeats(t, t.getAttribute('data-tooltip') || '')) return null;
  return t;
}

document.addEventListener('pointerover', (e) => {
  const t = owner(e);
  if (t === current) return;
  hide();
  if (t) timer = setTimeout(() => show(t), e.pointerType === 'touch' ? 0 : DELAY);
});
document.addEventListener('pointerout', (e) => {
  const t = owner(e);
  if (t && !t.contains(e.relatedTarget)) hide();
});
document.addEventListener('focusin', (e) => {
  const t = owner(e);
  if (t && t.matches(':focus-visible')) show(t);
});
document.addEventListener('focusout', hide);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hide(); });
window.addEventListener('scroll', hide, { capture: true, passive: true });
window.addEventListener('resize', hide);
