// denma: the tags a template can use, under the template editor
// (partials/denma/template-tags.html), those every campaign and automation
// e-mail must have (cmd/denma_footer.go) marked with whether the template has
// them. The page sets window._denmaContact, the center's details. The check on
// save is the one that counts; this one follows it. Spread into views/templates.js.

const CAMPAIGN = ['campaign', 'campaign_visual'];
const tagRe = (name) => new RegExp(`\\{\\{-?\\s*${name}\\b`);

// A website or e-mail address, or a word naming it, to compare (as the server's
// denmaSiteWord).
function siteWord(s) {
  let w = (s || '').toLowerCase();
  ['https://', 'http://', 'mailto:', 'www.'].forEach((p) => { if (w.startsWith(p)) w = w.slice(p.length); });
  return w.replace(/[/.:!?)]+$/, '');
}

// Text (or HTML's text) for comparing: lowercase, without commas, spaces collapsed.
function plainWords(s, isHTML) {
  let t = s || '';
  if (isHTML) {
    t = new DOMParser().parseFromString(t.replace(/<[^>]*>/g, ' $& '), 'text/html').body.textContent || '';
  }
  return t.toLowerCase().replace(/[,;·|]/g, ' ').split(/\s+/).filter(Boolean).join(' ');
}

// The tags, the template types they're for, and for those it must have, how
// to tell it has them (t: the template, below).
const TAGS = [
  {
    tag: '{{ template "content" . }}', types: ['campaign'],
    what: "Where each campaign's own content goes.",
    need: (t) => /\{\{-?\s*template\s+"content"/.test(t.body),
  },
  {
    tag: '{{ CenterFooter }}', types: CAMPAIGN, gives: true,
    what: 'The unsubscribe and "view in browser" links, already styled: the two below in one.',
  },
  {
    tag: '{{ UnsubscribeURL }}', types: CAMPAIGN,
    what: "The address of the link to unsubscribe from the center's e-mails, for a link's href.",
    need: (t) => t.has('CenterFooter') || t.has('UnsubscribeURL'),
  },
  {
    tag: '{{ MessageURL }}', types: CAMPAIGN,
    what: 'The address of the link to see the e-mail in a browser, for a link\'s href.',
    need: (t) => t.has('CenterFooter') || t.has('MessageURL'),
  },
  {
    tag: '{{ CenterAddress }}',
    what: "The center's postal address (Config → General), a line for each of its lines.",
    need: (t) => t.has('CenterAddress') || t.shows('address'),
  },
  {
    tag: '{{ CenterWebsite }}',
    what: "The center's website (Config → General), as a link.",
    need: (t) => t.has('CenterWebsite') || t.shows('website'),
  },
  {
    tag: '{{ CenterEmail }}',
    what: "The center's contact e-mail (Config → General), as a link.",
    need: (t) => t.has('CenterEmail') || t.shows('email'),
  },
  {
    tag: '{{ ManageURL }}', types: CAMPAIGN,
    what: 'The address of the page where they choose which lists they get, for a link\'s href.',
  },
  { tag: '{{ .Subscriber.FirstName }}', types: [...CAMPAIGN, 'tx'], what: 'Their first name.' },
  { tag: '{{ .Subscriber.LastName }}', types: [...CAMPAIGN, 'tx'], what: 'Their last name.' },
  { tag: '{{ .Subscriber.Email }}', types: [...CAMPAIGN, 'tx'], what: 'Their e-mail address.' },
  { tag: '{{ .Subscriber.Attribs.city }}', types: [...CAMPAIGN, 'tx'], what: 'One of their attributes (here "city").' },
  { tag: '{{ .Campaign.Subject }}', types: CAMPAIGN, what: "The e-mail's subject." },
  { tag: '{{ .Tx.Data.order_id }}', types: ['tx'], what: 'A value the sending system gave (here "order_id").' },
  { tag: '{{ Date "January 2, 2006" }}', what: "Today's full date. For just the current year: {{ Date \"2006\" }}" },
  {
    tag: '{{ TrackView }}', types: CAMPAIGN, auto: true,
    what: 'Counts opens. Every campaign gets it automatically, so it needn\'t be added.',
  },
];

export function denmaTemplateTags(contact) {
  const c = contact || {};
  let memo = { body: null };

  // The template, for the rows' checks; worked out once per change.
  function tpl(body) {
    if (memo.body === body) {
      return memo.t;
    }
    let words = null;
    const text = () => {
      if (words === null) {
        words = plainWords(body, true);
      }
      return words;
    };
    const hasWord = (want) => !!want && text().split(' ').some((w) => siteWord(w.replace(/^\(+/, '')) === want);
    const t = {
      body,
      has: (name) => tagRe(name).test(body),
      shows: (what) => {
        if (what === 'address') {
          const want = plainWords(c.Address, false);
          return !!want && text().includes(want);
        }
        if (what === 'website') {
          return hasWord(siteWord((c.Website || '').replace(/^[a-z]+:\/\//, '').replace(/\/$/, '')));
        }
        return hasWord(siteWord(c.Email));
      },
    };
    memo = { body, t };
    return t;
  }

  return {
    // Whether this type of template must have some of them.
    denmaTagsRequired() {
      return CAMPAIGN.includes(this.form.type);
    },

    // The rows for this type of template, each with its badge.
    denmaTagRows() {
      const t = tpl(this.form.body || '');
      const required = this.denmaTagsRequired();
      return TAGS.filter((r) => !r.types || r.types.includes(this.form.type)).map((r) => {
        let badge = null;
        if (required && r.need) {
          badge = r.need(t) ? { text: 'Required · in it', variant: 'success', missing: false } : { text: 'Required · missing', variant: 'danger', missing: true };
        } else if (required && r.gives) {
          badge = { text: 'Both links', variant: '' };
        } else if (r.auto) {
          badge = { text: 'Automatic', variant: '' };
        }
        return { ...r, badge };
      });
    },

    denmaTagsMissing() {
      return this.denmaTagRows().filter((r) => r.badge && r.badge.missing).length;
    },
  };
}
