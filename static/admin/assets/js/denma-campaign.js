// denma: the campaign editor's plain-text version when the center makes it
// automatically (cmd/denma_features.go, denma.plain_text_auto; the page sets
// window._denmaPlainAuto). It's made from the email on every save unless
// "Edit the plain-text version by hand" is ticked, which sets
// "plain_text_manual": true in the campaign's attributes (Attributes tab).
// Mixed into listmonk's campaign view (views/campaign.js), with
// partials/denma/plain-text.html in place of listmonk's plain-text switch.

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
