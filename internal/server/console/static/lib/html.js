// Escaping templates. Every interpolation is escaped unless it is itself an
// html`` result or wrapped in raw(): data from the API never becomes markup.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

export class Markup {
  /** @param {string} s */
  constructor(s) { this.s = s; }
  toString() { return this.s; }
}

/** Trusted markup, for fixed strings in this codebase only. */
export const raw = (/** @type {string} */ s) => new Markup(String(s));

/** @param {unknown} v @returns {string} */
function part(v) {
  if (v == null || v === false) return '';
  if (v instanceof Markup) return v.s;
  if (Array.isArray(v)) return v.map(part).join('');
  return String(v).replace(/[&<>"']/g, (c) => ESC[c]);
}

/** @param {TemplateStringsArray} strings @param {...unknown} values */
export function html(strings, ...values) {
  let out = strings[0];
  values.forEach((v, i) => { out += part(v) + strings[i + 1]; });
  return new Markup(out);
}

/** Replaces an element's content. @param {Element} el @param {Markup} markup */
export function put(el, markup) { el.innerHTML = markup.s; }

/** "a", "a and b", "a, b and c" */
export function list(/** @type {string[]} */ items, empty = 'nobody') {
  if (!items.length) return empty;
  if (items.length === 1) return items[0];
  return items.slice(0, -1).join(', ') + ' and ' + items[items.length - 1];
}

/** "1 person", "2 people" */
export function count(/** @type {number} */ n, /** @type {string} */ one, /** @type {string} */ many) {
  return `${n} ${n === 1 ? one : many}`;
}

/** Capitalizes the first letter. */
export const cap = (/** @type {string} */ s) => (s ? s[0].toUpperCase() + s.slice(1) : s);

/** "door.unlock" -> "Door unlock", as the server labels unlabeled permissions. */
export const fromName = (/** @type {string} */ s) => cap(s.replace(/[._-]/g, ' '));

/** 2 January 2026 */
export function day(/** @type {string|Date} */ t) {
  const d = new Date(t);
  return d.toLocaleDateString('en-US', { day: 'numeric', month: 'long', year: 'numeric' });
}

/** 14:05 */
export function clock(/** @type {string|Date} */ t) {
  return new Date(t).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false });
}

/** The host of a URL, or the URL itself if it does not parse. */
export function host(/** @type {string} */ u) {
  try { return new URL(u).host; } catch { return u; }
}
