// Levels: how sure attest must be that it is the right person. The engine
// calls them bands; people see Anyone / Likely / Sure / Confirmed, drawn by
// one glyph everywhere. A deployment that names its own bands sees its names.

import { html, raw, cap } from './html.js';

/**
 * @typedef {{ name: string, minConfidence?: number, maxAgeSeconds?: number, minModalities?: number, coPresence?: string[] }} Band
 * @typedef {{ key: string, name: string, glyph: number, desc: string, rank: number }} Level
 */

/** The shipped ladder's words. Other band names are shown as they are. */
const WORDS = { presumed: 'Likely', confident: 'Sure', corroborated: 'Confirmed' };

/** The floor: no recognition needed. Its key is not a band name. */
export const ANYONE = '';

/**
 * Every level a policy can require, weakest first: Anyone, then its bands.
 * @param {Band[]} bands
 * @returns {Level[]}
 */
export function levels(bands) {
  const n = bands.length;
  return [
    { key: ANYONE, name: 'Anyone', glyph: 0, rank: 0, desc: 'Anyone, recognized or not' },
    ...bands.map((b, i) => ({
      key: b.name,
      name: WORDS[/** @type {keyof WORDS} */ (b.name)] || b.name,
      // The glyph has three steps; longer or shorter ladders spread over them.
      glyph: Math.min(3, Math.max(1, Math.round(((i + 1) * 3) / n))),
      rank: i + 1,
      desc: describe(b),
    })),
  ];
}

/** @param {Level[]} all @param {string} key */
export function level(all, key) {
  return all.find((l) => l.key === key) || { key, name: key, glyph: 0, rank: -1, desc: 'Not a level this policy defines' };
}

const words = ['zero', 'one', 'two', 'three', 'four', 'five', 'six'];

/** @param {number} s */
function duration(s) {
  const unit = (/** @type {number} */ n, /** @type {string} */ one) => (n === 1 ? `${one}` : `${n} ${one}s`);
  if (s >= 3600 && s % 3600 === 0) return unit(s / 3600, 'hour');
  if (s >= 60 && s % 60 === 0) return unit(s / 60, 'minute');
  return unit(s, 'second');
}

/**
 * A band in words, from its numbers, so tuned thresholds read true.
 * @param {Band} b
 */
export function describe(b) {
  const parts = [];
  if (b.minConfidence) parts.push(`At least ${+(b.minConfidence * 100).toFixed(2)}% sure it\u2019s them`);
  else parts.push("Recognized");
  if (b.minModalities && b.minModalities > 1) parts.push(`recognized ${words[b.minModalities] || b.minModalities} separate ways, such as face and voice`);
  if (b.maxAgeSeconds) parts.push(`in the last ${duration(b.maxAgeSeconds)}`);
  for (const role of b.coPresence || []) parts.push(`with someone who is ${cap(role)} there too`);
  return parts.join(', ');
}

/** The level glyph: three ascending bars, the top split in two. */
export function ladder(/** @type {number} */ l, size = 14) {
  const bar = (/** @type {number} */ x, /** @type {number} */ y, /** @type {number} */ w, /** @type {boolean} */ on) =>
    `<rect class="g${on ? ' on' : ''}" x="${x}" y="${y}" width="${w}" height="${13 - y}" rx="0.8"/>`;
  return raw(`<svg class="ladder" width="${Math.round((size * 16) / 14)}" height="${size}" viewBox="0 0 16 14" aria-hidden="true">${bar(0.5, 9, 3.4, l >= 1)}${bar(5.3, 5.5, 3.4, l >= 2)}${bar(10.1, 1.5, 2.1, l >= 3)}${bar(13.3, 1.5, 2.1, l >= 3)}<path class="r" d="M0 13.5h16"/></svg>`);
}

/** A level with its name beside it, always. @param {Level} l */
export function tag(l, extra = '') {
  return html`<span class="lv" title="${l.desc}">${ladder(l.glyph)}<span>${l.name}${extra}</span></span>`;
}
