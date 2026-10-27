// denma: asks before leaving an editor with unsaved changes. A link in the
// admin opens a dialog (save, don't save, or stay); closing the tab or going
// back gets the browser's own question. What an editor does to the body as it
// loads (the visual editor rewrites it) isn't a change. Spread into
// views/templates.js and views/campaign.js; utils.js's redirect and reload
// (the page's own buttons) leave without asking, but Unschedule and Resend
// ask first (denmaUnsavedFirst).

const BODY = ['body', 'body_source', 'altbody'];

// The dialog: resolves 'save', 'discard' or 'stay'.
function ask(what = 'leaving') {
  const tpl = document.getElementById('confirm-dialog-template');
  if (!tpl) {
    return Promise.resolve(window.confirm('Leave without saving your changes?') ? 'discard' : 'stay');
  }
  return new Promise((resolve) => {
    const dialog = tpl.content.firstElementChild.cloneNode(true);
    dialog.dataset.testid = 'unsaved-dialog';
    dialog.querySelector('[data-confirm-message]').textContent = `You have unsaved changes. Save them before ${what}?`;
    const foot = dialog.querySelector('footer');
    foot.replaceChildren();
    const button = (text, variant, value) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = text;
      b.dataset.unsaved = value;
      if (variant) {
        b.dataset.variant = variant;
      } else {
        b.className = 'outline';
      }
      b.addEventListener('click', () => done(value));
      foot.appendChild(b);
      return b;
    };
    let resolved = false;
    const done = (v) => {
      if (resolved) {
        return;
      }
      resolved = true;
      resolve(v);
      dialog.close();
      dialog.remove();
    };
    const save = button('Save', 'primary', 'save');
    button("Don't save", 'danger', 'discard');
    button('Cancel', '', 'stay');
    dialog.addEventListener('cancel', (e) => {
      e.preventDefault();
      done('stay');
    });
    dialog.addEventListener('close', () => done('stay'));
    document.body.appendChild(dialog);
    dialog.showModal();
    save.focus();
  });
}

// A link click that leaves the page, or null.
function leavingTo(e) {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
    return null;
  }
  const a = e.target.closest && e.target.closest('a[href]');
  if (!a || (a.target && a.target !== '_self') || a.hasAttribute('download')) {
    return null;
  }
  const url = new URL(a.href, window.location.href);
  if (!/^https?:$/.test(url.protocol)) {
    return null;
  }
  if (url.origin === window.location.origin && url.pathname === window.location.pathname && url.search === window.location.search) {
    return null;
  }
  return url.href;
}

// skip: the form's fields that aren't saved; save: saves the page (this is the component).
export function denmaUnsaved({ skip = [], save }) {
  let saved = null;
  let readyAt = 0;
  let touched = false;

  return {
    _denmaSnapshot() {
      const f = { ...this.form };
      skip.forEach((k) => delete f[k]);
      return JSON.parse(JSON.stringify(f));
    },

    denmaDirty() {
      return saved !== null && JSON.stringify(this._denmaSnapshot()) !== JSON.stringify(saved);
    },

    // Called once a save has gone through.
    denmaSaved() {
      saved = this._denmaSnapshot();
    },

    // Before a button that reloads the page (WHAT: "unscheduling"): true to go
    // on, having saved or not as chosen. Unchanged, it's OTHERWISE's answer.
    async denmaUnsavedFirst(what, otherwise = () => true) {
      if (!this.denmaDirty()) {
        return otherwise();
      }
      const choice = await ask(what);
      if (choice === 'save') {
        try {
          await save.call(this);
        } catch (err) {
          return false;
        }
        return !this.denmaDirty();
      }
      return choice === 'discard';
    },

    denmaUnsavedInit() {
      this.$nextTick(() => this.denmaSaved());

      // An editor's own rewrite of an unchanged body is taken as saved: until
      // the person does something, and for a second after it's ready.
      this.$root.addEventListener('editor-ready', () => {
        readyAt = Date.now();
        touched = false;
      });
      const touch = () => { touched = true; };
      ['pointerdown', 'keydown'].forEach((t) => document.addEventListener(t, touch, true));
      window.addEventListener('blur', touch);
      BODY.forEach((k) => this.$watch(`form.${k}`, (now, before) => {
        const settling = readyAt && (!touched || Date.now() < readyAt + 1000);
        if (settling && saved && JSON.stringify(saved[k] ?? null) === JSON.stringify(before ?? null)) {
          saved[k] = JSON.parse(JSON.stringify(now ?? null));
        }
      }));

      window.addEventListener('beforeunload', (e) => {
        if (!window.denmaLeaving && this.denmaDirty()) {
          e.preventDefault();
          e.returnValue = '';
        }
      });

      document.addEventListener('click', async (e) => {
        const to = leavingTo(e);
        if (!to || window.denmaLeaving || !this.denmaDirty()) {
          return;
        }
        e.preventDefault();
        const choice = await ask();
        if (choice === 'stay') {
          return;
        }
        if (choice === 'save') {
          try {
            await save.call(this);
          } catch (err) {
            return;
          }
          if (this.denmaDirty() && !window.denmaLeaving) {
            return;
          }
        }
        window.denmaLeaving = true;
        window.location.href = to;
      });
    },
  };
}
