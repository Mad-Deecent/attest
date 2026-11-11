// Shared drawing: status marks, the page anatomy (header, master, detail),
// and small helpers screens use the same way.

import { html, raw } from './html.js';

/** @typedef {'ok'|'no'|'warn'|'skip'} Kind */

const MARKS = {
  ok: '<circle cx="7" cy="7" r="6" fill="currentColor"/><path d="M4.3 7.2 6.1 9l3.6-3.8" fill="none" stroke="var(--pane)" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
  no: '<circle cx="7" cy="7" r="5.25" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M4.6 7h4.8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  warn: '<path d="M7 1.6 13 12.2H1z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/><path d="M7 5.6v3M7 10.2v.1" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>',
  skip: '<circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 2"/>',
};

/** A status glyph. Never alone: a word sits beside it. @param {Kind} kind */
export function mark(kind) {
  return raw(`<svg class="mk ${kind}" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">${MARKS[kind]}</svg>`);
}

/** A glyph and its word. @param {Kind} kind @param {unknown} text */
export function status(kind, text) {
  return html`<span class="oc">${mark(kind)}<span>${text}</span></span>`;
}

/** Needs attention. @param {unknown} text */
export function attention(text) {
  return html`<span class="att">${mark('warn')}<span>${text}</span></span>`;
}

/** A failure the person can act on. @param {unknown} text */
export function problem(text, kind = /** @type {Kind} */ ('no')) {
  return html`<p class="problem" role="alert">${mark(kind)}<span>${text}</span></p>`;
}

export const chevron = raw('<svg class="chev" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true"><path d="M2.5 4 5 6.5 7.5 4" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>');
const backArrow = raw('<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"><path d="M11 4 6 9l5 5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>');

/**
 * The page anatomy every domain shares: a header with one primary action, a
 * master list and, when something is selected, its detail.
 * @param {{ title: string, lede: unknown, action?: unknown, extra?: unknown, master: unknown, detail: unknown, pushed: boolean, back: { href: string, label: string } }} p
 */
export function page(p) {
  return html`<header class="ph"><div><h1 tabindex="-1" id="h1">${p.title}</h1><p class="lede">${p.lede}</p></div>${p.action ? html`<div class="ph-act">${p.action}</div>` : ''}</header>${p.extra || ''}
  <div class="md${p.pushed ? ' has-detail' : ''}"><section class="master" aria-labelledby="h1">${p.master}</section><aside class="detail" aria-label="Detail" tabindex="-1"><a class="back" href="${p.back.href}">${backArrow}${p.back.label}</a>${p.detail}</aside></div>`;
}

/** A quiet line while something loads. */
export const loading = (what = 'Loading…') => html`<p class="note lead" aria-busy="true">${what}</p>`;

/** A failed load, with a way to try again. @param {string} text */
export function loadFailed(text) {
  return html`<div class="note lead" role="alert"><span class="att">${mark('warn')}<span>${text}</span></span> <button type="button" class="linkbtn" data-retry>Try again</button></div>`;
}

/** Announces a sentence to screen readers. */
export function announce(/** @type {string} */ text) {
  const el = document.getElementById('announce');
  if (!el) return;
  el.textContent = '';
  setTimeout(() => { el.textContent = text; }, 30);
}

/** Phones show master or detail, not both. */
export const narrow = () => matchMedia('(max-width: 760px)').matches;

/**
 * Re-renders while keeping focus on the "same" control, found by a selector
 * built from the focused element's data attributes or id.
 * @param {() => void} draw
 */
export function keepFocus(draw) {
  const ae = /** @type {HTMLElement|null} */ (document.activeElement);
  let sel = '';
  if (ae && ae !== document.body) {
    if (ae.id) sel = `#${CSS.escape(ae.id)}`;
    else {
      const attrs = [...ae.attributes].filter((a) => a.name.startsWith('data-'));
      if (attrs.length) sel = attrs.map((a) => `[${a.name}="${CSS.escape(a.value)}"]`).join('');
    }
  }
  const scrolls = [...document.querySelectorAll('.master, .detail, .impact')].map((el) => el.scrollTop);
  draw();
  [...document.querySelectorAll('.master, .detail, .impact')].forEach((el, i) => { if (scrolls[i] != null) el.scrollTop = scrolls[i]; });
  if (sel) /** @type {HTMLElement|null} */ (document.querySelector(sel))?.focus({ preventScroll: true });
}
