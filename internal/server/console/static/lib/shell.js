// The signed-in frame: the rail (desktop), the tab bar and account sheet
// (phone), the command palette, and keyboard shortcuts. Only destinations
// that work are listed.

import { html, put, raw } from './html.js';
import { navigate, href } from './router.js';

/**
 * @typedef {{ id: string, name: string, key: string, icon: string }} Domain
 * @typedef {{ label: string, href: string, group: string }} Item
 * @typedef {{ id: string, show: (main: HTMLElement, route: import('./router.js').Route) => void, items?: () => Item[], pending?: () => boolean, attention?: () => string }} Screen
 */

/** @type {Domain[]} */
export const DOMAINS = [
  { id: 'identities', name: 'Identities', key: 'i', icon: '<circle cx="10" cy="7.5" r="3"/><path d="M4 16.5c1.2-3 3.4-4.3 6-4.3s4.8 1.3 6 4.3"/>' },
  { id: 'policy', name: 'Policy', key: 'o', icon: '<path d="M4 4h12v12H4zM4 10h12M10 4v12"/>' },
];
/** @type {Domain[]} */
export const SECONDARY = [
  { id: 'connections', name: 'Connections', key: 'c', icon: '<path d="M7.5 12.5 12.5 7.5"/><path d="M9 5.5l1.6-1.6a3 3 0 0 1 4.3 4.3L13.4 9.8M10.6 14.5 9 16.1a3 3 0 0 1-4.3-4.3l1.6-1.6"/>' },
];
const ALL = [...DOMAINS, ...SECONDARY];

/** @type {{ name: string, place: string, screens: Record<string, Screen>, active: string, signOut: () => void }} */
const state = { name: '', place: '', screens: {}, active: '', signOut: () => {} };

const search = raw('<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5 14 14"/></svg>');
const accountIcon = '<circle cx="10" cy="10" r="6.5"/><circle cx="10" cy="8.5" r="2.2"/><path d="M6 14.6c1-1.4 2.4-2 4-2s3 .6 4 2"/>';

/**
 * @param {{ name: string, place: string, screens: Record<string, Screen>, signOut: () => void }} opts
 * @returns {HTMLElement} the element screens draw into
 */
export function mount(opts) {
  Object.assign(state, opts);
  const root = /** @type {HTMLElement} */ (document.getElementById('app'));
  put(root, html`<div class="shell">
    <aside class="rail" aria-label="attest">
      <div class="brand"><b>attest</b><span>${state.place}</span></div>
      <button class="jump" type="button" data-palette>${search}Jump to…<kbd>⌘K</kbd></button>
      <nav aria-label="Domains"><ul class="nav" id="nav"></ul></nav>
      <div class="rail-foot">
        <nav aria-label="Settings"><ul class="nav" id="nav2"></ul></nav>
        <div class="me"><span class="who" title="${state.name}">${state.name || 'Signed in'}</span><span class="me-acts"><button class="linkbtn" type="button" data-theme-toggle></button><button class="linkbtn" type="button" data-signout>Sign out</button></span></div>
      </div>
    </aside>
    <main class="main" id="main"></main>
  </div>
  <nav class="tabbar" id="tabbar" aria-label="Domains"></nav>
  <div class="sheet" id="sheet" role="dialog" aria-modal="true" aria-label="Account"></div>
  <div class="scrim" id="scrim" hidden></div>
  <div class="palette" id="palette" role="dialog" aria-modal="true" aria-label="Jump to" hidden>
    <input id="pq" type="text" placeholder="Jump to a person, an action or a place…" aria-label="Jump to" aria-controls="pl" autocomplete="off" spellcheck="false">
    <ul id="pl" role="listbox" aria-label="Results"></ul>
    <div class="foot"><span><kbd>↑</kbd> <kbd>↓</kbd> move</span><span><kbd>↵</kbd> open</span><span><kbd>esc</kbd> close</span><span class="jump-m"><kbd>g</kbd> then <kbd>i</kbd> <kbd>o</kbd> <kbd>c</kbd></span></div>
  </div>`);
  wire();
  return /** @type {HTMLElement} */ (document.getElementById('main'));
}

/** Redraws the navigation: current place, unapplied work, attention. */
export function nav(/** @type {string} */ active = state.active) {
  state.active = active;
  const tail = (/** @type {Domain} */ d) => {
    const s = state.screens[d.id];
    if (s?.pending?.()) return html`<span class="tail" title="Unapplied changes"><span class="pending" aria-hidden="true"></span><span class="sr">, unapplied changes</span></span>`;
    const att = s?.attention?.();
    if (att) return html`<span class="tail" title="${att}"><svg class="mk warn" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M7 1.6 13 12.2H1z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/><path d="M7 5.6v3M7 10.2v.1" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg><span class="sr">, ${att}</span></span>`;
    return '';
  };
  const li = (/** @type {Domain} */ d) => html`<li><a href="${href(d.id)}" ${active === d.id ? raw('aria-current="page"') : ''}>${d.name}${tail(d)}</a></li>`;
  put(/** @type {Element} */ (document.getElementById('nav')), html`${DOMAINS.map(li)}`);
  put(/** @type {Element} */ (document.getElementById('nav2')), html`${SECONDARY.map(li)}`);
  const tab = (/** @type {Domain} */ d) => html`<a href="${href(d.id)}" ${active === d.id ? raw('aria-current="page"') : ''}><svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">${raw(d.icon)}</svg>${d.name}${state.screens[d.id]?.pending?.() ? html`<span class="pending" aria-hidden="true"></span><span class="sr">, unapplied changes</span>` : ''}</a>`;
  put(/** @type {Element} */ (document.getElementById('tabbar')), html`${ALL.map(tab)}<button type="button" data-sheet aria-haspopup="dialog"><svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">${raw(accountIcon)}</svg>Account</button>`);
  const dark = document.documentElement.dataset.theme === 'dark' ||
    (!document.documentElement.dataset.theme && matchMedia('(prefers-color-scheme: dark)').matches);
  document.querySelectorAll('.me [data-theme-toggle]').forEach((b) => { b.textContent = dark ? 'Light' : 'Dark'; });
  put(/** @type {Element} */ (document.getElementById('sheet')), html`<div class="grab"></div><p class="who">Signed in as ${state.name || 'an administrator'}</p><button type="button" class="sheet-i" data-theme-toggle>${dark ? 'Light appearance' : 'Dark appearance'}</button><button type="button" class="sheet-i" data-signout>Sign out</button>`);
}

export function toggleTheme() {
  const rootEl = document.documentElement;
  const dark = rootEl.dataset.theme === 'dark' || (!rootEl.dataset.theme && matchMedia('(prefers-color-scheme: dark)').matches);
  rootEl.dataset.theme = dark ? 'light' : 'dark';
  try { localStorage.setItem('attest.theme', rootEl.dataset.theme); } catch { /* private mode */ }
}

/* ---------- Palette ---------- */

let pi = 0;
/** @type {Item[]} */
let plist = [];
/** @type {HTMLElement|null} */
let opener = null;

function items() {
  /** @type {Item[]} */
  const out = ALL.map((d) => ({ label: d.name, href: href(d.id), group: 'Go to' }));
  for (const s of Object.values(state.screens)) out.push(...(s.items?.() || []));
  out.push({ label: 'Switch appearance', href: '#theme', group: 'Account' }, { label: 'Sign out', href: '#signout', group: 'Account' });
  return out;
}

function drawPalette() {
  const q = /** @type {HTMLInputElement} */ (document.getElementById('pq')).value.trim().toLowerCase();
  plist = items().filter((x) => !q || x.label.toLowerCase().includes(q) || x.group.toLowerCase().includes(q)).slice(0, 12);
  pi = Math.min(pi, Math.max(0, plist.length - 1));
  const pl = /** @type {HTMLElement} */ (document.getElementById('pl'));
  put(pl, plist.length
    ? html`${plist.map((x, i) => html`<li><a id="po-${i}" href="${x.href}" class="${i === pi ? 'hi' : ''}" role="option" aria-selected="${i === pi}" tabindex="-1">${x.label}<span class="dom">${x.group}</span></a></li>`)}`
    : html`<li class="none">Nothing matches</li>`);
  document.getElementById('pq')?.setAttribute('aria-activedescendant', plist.length ? `po-${pi}` : '');
  document.getElementById(`po-${pi}`)?.scrollIntoView({ block: 'nearest' });
}

function openPalette() {
  opener = /** @type {HTMLElement} */ (document.activeElement);
  document.getElementById('palette')?.removeAttribute('hidden');
  document.getElementById('scrim')?.removeAttribute('hidden');
  const pq = /** @type {HTMLInputElement} */ (document.getElementById('pq'));
  pq.value = '';
  pi = 0;
  drawPalette();
  pq.focus();
}

function closeOverlays() {
  document.getElementById('palette')?.setAttribute('hidden', '');
  document.getElementById('scrim')?.setAttribute('hidden', '');
  document.getElementById('sheet')?.classList.remove('on');
  opener?.focus?.();
  opener = null;
}

/** @param {Item} item */
function choose(item) {
  closeOverlays();
  if (item.href === '#theme') { toggleTheme(); nav(); return; }
  if (item.href === '#signout') { state.signOut(); return; }
  navigate(item.href);
}

let wired = false;
function wire() {
  if (wired) return;
  wired = true;
  document.addEventListener('click', (e) => {
    const t = /** @type {Element} */ (e.target);
    if (t.closest('[data-palette]')) { openPalette(); return; }
    if (t.closest('[data-theme-toggle]')) { toggleTheme(); nav(); return; }
    if (t.closest('[data-signout]')) { closeOverlays(); state.signOut(); return; }
    if (t.closest('[data-sheet]')) {
      opener = /** @type {HTMLElement} */ (t.closest('[data-sheet]'));
      document.getElementById('sheet')?.classList.add('on');
      document.getElementById('scrim')?.removeAttribute('hidden');
      /** @type {HTMLElement|null} */ (document.querySelector('#sheet button'))?.focus();
      return;
    }
    if (t.closest('#scrim')) { closeOverlays(); return; }
    const opt = t.closest('#pl a');
    if (opt) { e.preventDefault(); e.stopPropagation(); const i = +opt.id.slice(3); if (plist[i]) choose(plist[i]); }
  }, true);
  document.getElementById('pq')?.addEventListener('input', () => { pi = 0; drawPalette(); });
  document.getElementById('pq')?.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { pi = Math.min(plist.length - 1, pi + 1); drawPalette(); e.preventDefault(); }
    else if (e.key === 'ArrowUp') { pi = Math.max(0, pi - 1); drawPalette(); e.preventDefault(); }
    else if (e.key === 'Enter' && plist[pi]) { e.preventDefault(); choose(plist[pi]); }
    else if (e.key === 'Tab') e.preventDefault();
  });
  let gAt = 0;
  document.addEventListener('keydown', (e) => {
    const paletteOpen = !document.getElementById('palette')?.hasAttribute('hidden');
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); paletteOpen ? closeOverlays() : openPalette(); return; }
    if (e.key === 'Escape' && (paletteOpen || document.getElementById('sheet')?.classList.contains('on'))) { closeOverlays(); return; }
    const t = /** @type {HTMLElement} */ (e.target);
    if (paletteOpen || e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|SELECT|TEXTAREA)$/.test(t.tagName) || t.isContentEditable || document.querySelector('.menu')) return;
    if (e.key === 'g') { gAt = Date.now(); return; }
    if (Date.now() - gAt < 900) {
      const d = ALL.find((x) => x.key === e.key);
      gAt = 0;
      if (d) { e.preventDefault(); navigate(href(d.id)); }
      return;
    }
    if (e.key === 'j' || e.key === 'k') {
      const rows = /** @type {HTMLAnchorElement[]} */ ([...document.querySelectorAll('.master a.row, .master td.act a')]);
      if (!rows.length) return;
      let i = rows.findIndex((r) => r.getAttribute('aria-current') === 'true' || r.closest('tr.cur'));
      i = e.key === 'j' ? Math.min(rows.length - 1, i + 1) : Math.max(0, i - 1);
      e.preventDefault();
      rows[i].focus();
      navigate(new URL(rows[i].href).pathname);
    }
  });
}
