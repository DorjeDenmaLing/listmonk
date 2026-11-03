// Adds to the visual builder's bundle in dist/: the "System sans" font (each
// device's own) for the e-mail and every block, a Border radius and Border
// color for Image blocks, and the e-mail's own font shown in its Styles. Fails
// the build if the bundle doesn't have the places they're changed in.
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

const KEY = 'SYSTEM_SANS';
const FONTS = '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Arial, sans-serif';
const MONO = `case"MONOSPACE":return'"Nimbus Mono PS", "Courier New", "Cutive Mono", monospace'`;
const MONO_OPTION = `{key:"MONOSPACE",label:"Monospace",value:'"Nimbus Mono PS", "Courier New", "Cutive Mono", monospace'}`;
const IMG_STYLE = 'outline:"none",border:"none",textDecoration:"none",verticalAlign:null!=(c=null==t?void 0:t.contentAlignment)?c:"middle",display:"inline-block",maxWidth:"100%"';
const COLOR = 'Pb.string().regex(/^#[0-9a-fA-F]{6}$/).optional().nullable()';

// [found, added, how many].
const EDITS = [
  // The font: the schemas' font lists, the functions naming each font's families, the dropdown.
  ['"MODERN_SERIF","BOOK_SERIF","MONOSPACE"]', `"MODERN_SERIF","BOOK_SERIF","MONOSPACE","${KEY}"]`, 6],
  [MONO, `${MONO};case"${KEY}":return'${FONTS}'`, 6],
  [MONO_OPTION, `${MONO_OPTION},{key:"${KEY}",label:"System sans",value:'${FONTS}'}`, 1],
  // The e-mail's Styles show its font (not always Modern sans).
  ['v.jsx(EC,{label:"Font family",defaultValue:"MODERN_SANS",onChange:t=>r({...e,fontFamily:t})})',
    'v.jsx(EC,{label:"Font family",defaultValue:e.fontFamily??"MODERN_SANS",onChange:t=>r({...e,fontFamily:t})})', 1],
  // Image borders: the Image block's style allows borderRadius and borderColor, its <img> has them (a 1px
  // border, following the corners), its panel has their controls.
  ['Ey=Pb.object({style:Pb.object({padding:yy,', `Ey=Pb.object({style:Pb.object({borderRadius:Pb.number().optional().nullable(),borderColor:${COLOR},padding:yy,`, 1],
  [IMG_STYLE, IMG_STYLE.replace('border:"none"', 'border:null!=e&&e.borderColor?`1px solid ${e.borderColor}`:"none"')
    + ',borderRadius:null==e?void 0:e.borderRadius', 1],
  ['names:["backgroundColor","textAlign","padding"]', 'names:["backgroundColor","borderColor","borderRadius","textAlign","padding"]', 1],
];

export async function patch(dist) {
  const file = path.join(dist, 'email-builder/email-builder.umd.js');
  let js = await readFile(file, 'utf8');
  for (const [from, to, count] of EDITS) {
    const found = js.split(from).length - 1;
    if (found !== count) throw new Error(`email-builder: expected ${count} of ${from.slice(0, 60)}, found ${found}`);
    js = js.replaceAll(from, to);
  }
  await writeFile(file, js);
  console.log('email-builder: added the System sans font and image borders');
}
