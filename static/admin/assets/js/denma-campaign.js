// denma: the campaign editor's automatic plain-text version (denma.plain_text_auto).

const FLAG = 'plain_text_manual';

export function denmaPlainText() {
  return {
    // true or false, or null when the Attributes JSON can't be read.
    denmaPlainManual() {
      try {
        const a = JSON.parse(this.form.attribsStr || '{}');
        return !!(a && typeof a === 'object' && a[FLAG] === true);
      } catch {
        return null;
      }
    },

    denmaSetPlainManual(on) {
      let a;
      try {
        a = JSON.parse(this.form.attribsStr || '{}');
      } catch {
        return;
      }
      if (!a || typeof a !== 'object' || Array.isArray(a)) {
        a = {};
      }
      if (on) {
        a[FLAG] = true;
      } else {
        delete a[FLAG];
      }
      this.form.attribsStr = JSON.stringify(a, null, 4);
      // Editing starts from the text made last; the box shows it already.
      if (on && this.form.altbody == null) {
        this.form.altbody = '';
      }
    },

    // After a save, show the plain text the server made from the email.
    denmaAfterSave(d) {
      if (window._denmaPlainAuto && d && this.denmaPlainManual() === false) {
        this.form.altbody = d.altbody ?? null;
      }
    },
  };
}
