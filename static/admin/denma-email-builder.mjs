// Adds the "System sans" font (each device's own) to the visual builder's
// bundle in dist/, for the e-mail and every block. Fails the build if the
// bundle doesn't have the places it's added to.
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

const KEY = 'SYSTEM_SANS';
const FONTS = '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Arial, sans-serif';
const MONO = `case"MONOSPACE":return'"Nimbus Mono PS", "Courier New", "Cutive Mono", monospace'`;
const MONO_OPTION = `{key:"MONOSPACE",label:"Monospace",value:'"Nimbus Mono PS", "Courier New", "Cutive Mono", monospace'}`;

// [found, added, how many]: the schemas' font lists, the functions naming the fonts, the dropdown.
const EDITS = [
  ['"MODERN_SERIF","BOOK_SERIF","MONOSPACE"]', `"MODERN_SERIF","BOOK_SERIF","MONOSPACE","${KEY}"]`, 6],
  [MONO, `${MONO};case"${KEY}":return'${FONTS}'`, 6],
  [MONO_OPTION, `${MONO_OPTION},{key:"${KEY}",label:"System sans",value:'${FONTS}'}`, 1],
];

export async function patchFonts(dist) {
  const file = path.join(dist, 'email-builder/email-builder.umd.js');
  let js = await readFile(file, 'utf8');
  for (const [from, to, count] of EDITS) {
    const found = js.split(from).length - 1;
    if (found !== count) throw new Error(`email-builder: expected ${count} of ${from.slice(0, 60)}, found ${found}`);
    js = js.replaceAll(from, to);
  }
  await writeFile(file, js);
  console.log('email-builder: added the System sans font');
}
