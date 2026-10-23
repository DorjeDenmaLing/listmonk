// denma: picking an image from Media for a field on a page without its own
// picker (the hub's Settings -> General, partials/denma/topnav.html).
// $denmaPickMedia((url) => ...) opens Media's picker in the page's
// #denma-media-picker dialog and gives the chosen file's URL.
import Alpine from 'alpinejs';

let pick = null;

window.addEventListener('message', (e) => {
  const dlg = document.getElementById('denma-media-picker');
  const frame = dlg?.querySelector('iframe');
  if (!pick || e.origin !== window.location.origin || e.source !== frame?.contentWindow) {
    return;
  }
  if (e.data?.type === 'media-select') {
    pick(e.data.media.url);
    dlg.close();
  }
});

Alpine.magic('denmaPickMedia', () => (cb) => {
  const dlg = document.getElementById('denma-media-picker');
  if (!dlg) {
    return;
  }
  pick = cb;
  dlg.addEventListener('close', () => { pick = null; }, { once: true });
  dlg.querySelector('iframe').src = `${dlg.dataset.src}?t=${Date.now()}`;
  dlg.showModal();
});
