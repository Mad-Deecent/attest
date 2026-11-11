// Connections: what attest leans on. The identity provider (its live state,
// reconnecting or changing it) and attest's own public address. Every change
// says what it affects before it is made, and applies at once.

import { html, raw, put, host, clock } from '../lib/html.js';
import { get, put as putJSON, plain, ApiError } from '../lib/api.js';
import { page, status, loading, loadFailed, problem, announce, narrow, keepFocus } from '../lib/ui.js';
import { href, navigate } from '../lib/router.js';
import { nav } from '../lib/shell.js';
import * as pf from './provider-form.js';

/**
 * @typedef {{ connected: boolean, reachable?: boolean, problem?: string, checkedAt: string }} ProviderStatus
 */

const st = {
  /** @type {any} */ settings: null,
  /** @type {ProviderStatus|null} */ status: null,
  checking: false,
  error: '',
  /** @type {HTMLElement|null} */ main: null,
  /** @type {string|null} */ sel: null,
  pushed: false,
  /** @type {'address'|'provider'|null} */ editing: null,
  addr: '',
  addrBusy: false,
  addrError: '',
  /** @type {{ from: string, to: string }|null} */ moved: null,
  /** @type {pf.FormState} */ form: pf.blank(null),
  /** @type {pf.Change[]} */ lastChanges: [],
};

const NAMES = { keycloak: 'Keycloak', oidc: 'Identity provider', config: 'Identity provider' };
const providerName = (/** @type {any} */ p) => NAMES[/** @type {keyof NAMES} */ (p?.kind)] || 'Identity provider';

/** @type {import('../lib/shell.js').Screen & { check: () => Promise<void> }} */
export const connections = {
  id: 'connections',
  show(main, route) {
    const fresh = st.main !== main || main.dataset.shown !== 'connections';
    st.main = main;
    main.dataset.shown = 'connections';
    st.pushed = !!route.id;
    st.sel = route.id === 'address' || route.id === 'provider' ? route.id : narrow() ? null : 'provider';
    if (st.editing && st.editing !== st.sel) st.editing = null;
    if (fresh || !st.settings) load();
    else draw();
    if (route.id && narrow()) { /** @type {HTMLElement|null} */ (main.querySelector('.detail'))?.focus({ preventScroll: true }); scrollTo(0, 0); }
  },
  items: () => [
    { label: 'Identity provider', href: href('connections', 'provider'), group: 'Connections' },
    { label: 'Public address', href: href('connections', 'address'), group: 'Connections' },
  ],
  attention() {
    if (!st.status) return '';
    if (!st.status.connected) return 'No identity provider is connected';
    return st.status.reachable ? '' : 'The identity provider isn’t answering';
  },
  check,
};

async function load() {
  st.error = '';
  if (st.main) draw();
  try {
    st.settings = await get('/v1/settings');
  } catch (e) {
    st.error = plain(e);
  }
  draw();
  check().then(() => { nav(); draw(); });
}

async function check() {
  st.checking = true;
  try {
    st.status = await get('/v1/settings/provider/status');
  } catch (e) {
    if (!(e instanceof ApiError) || e.status !== 401) st.status = { connected: true, reachable: false, problem: plain(e), checkedAt: new Date().toISOString() };
  }
  st.checking = false;
}

function draw() {
  if (!st.main || st.main.dataset.shown !== 'connections') return;
  keepFocus(() => put(/** @type {HTMLElement} */ (st.main), view()));
  wire(/** @type {HTMLElement} */ (st.main));
}

function view() {
  const s = st.settings;
  let master, detail;
  if (st.error) master = loadFailed(`Couldn’t load settings. ${st.error}`), detail = '';
  else if (!s) master = loading(), detail = '';
  else {
    const p = s.provider;
    const provTail = !p ? status('warn', 'Not connected')
      : st.checking && !st.status ? html`<span class="dim">${providerName(p)} · checking…</span>`
        : st.status && !st.status.reachable ? status('warn', `${providerName(p)} · not answering`)
          : html`<span class="sub">${providerName(p)} · ${host(p.issuer)}</span>`;
    const row = (/** @type {string} */ id, /** @type {string} */ name, /** @type {unknown} */ tail) =>
      html`<a class="row r-two" href="${href('connections', id)}" ${st.sel === id ? raw('aria-current="true"') : ''}><span class="name">${name}</span><span class="end small">${tail}</span></a>`;
    master = html`<div class="group">
      ${row('provider', 'Identity provider', provTail)}
      ${row('address', 'Public address', html`<span class="sub">${s.publicUrl ? host(s.publicUrl) : 'Not set'}</span>`)}
    </div>
    <p class="note">Changes here apply as soon as you make them. Nothing needs restarting.</p>`;
    detail = st.sel === 'address' ? addressDetail() : st.sel === 'provider' ? providerDetail() : html`<p class="empty-detail">Select a connection.</p>`;
  }
  return page({
    title: 'Connections',
    lede: 'What attest leans on: your identity provider, and the address people reach attest at.',
    master, detail, pushed: st.pushed, back: { href: href('connections'), label: 'Connections' },
  });
}

/* ---------- Identity provider ---------- */

function providerDetail() {
  const p = st.settings.provider;
  if (!p) {
    return html`<div class="dh"><div><div class="kicker">Identity provider</div><h2>Not connected</h2></div>${status('warn', 'Not connected')}</div>
      <p class="sub">Without one, nobody can sign in or enroll.</p>${st.editing === 'provider' ? providerEdit() : html`<div class="acts"><button class="btn primary" type="button" data-edit="provider">Connect</button></div>`}`;
  }
  const s = st.status;
  const state = st.checking ? status('skip', 'Checking…')
    : !s ? '' : s.reachable ? status('ok', 'Answering') : status('warn', 'Not answering');
  const realm = /\/realms\/([^/]+)\/?$/.exec(p.issuer)?.[1];
  const head = html`<div class="dh"><div><div class="kicker">Identity provider</div><h2>${providerName(p)}</h2></div>${state}</div>
    <p class="sub">People sign in here. It says who they are and which roles they hold; attest never keeps passwords.</p>`;
  const down = s && !s.reachable && !st.checking
    ? html`<p class="consequence">${unreachable(s.problem || '')} Until it answers, people can’t sign in or enroll.</p>` : '';
  if (st.editing === 'provider') return html`${head}${providerEdit()}`;
  return html`${head}${down}
    <dl class="kv sec first">
      <dt>Address</dt><dd>${host(p.issuer)}${realm ? html`<span class="dim">, realm ${decodeURIComponent(realm)}</span>` : ''}</dd>
      <dt>Sign-in client</dt><dd>${p.clientId ? html`<code>${p.clientId}</code>` : html`None. Enrollment and console sign-in are off.`}</dd>
      <dt>Administrators</dt><dd>Accounts with <code>${p.roles?.admin}</code></dd>
      <dt>Sensors</dt><dd>Clients with <code>${p.roles?.producer}</code></dd>
      <dt>Assistants</dt><dd>Clients with <code>${p.roles?.agent}</code></dd>
      ${s ? html`<dt>Last checked</dt><dd>${clock(s.checkedAt)} <button class="linkbtn small" type="button" data-check>Check again</button></dd>` : ''}
    </dl>
    ${st.lastChanges.length ? pf.changesView(st.lastChanges, p.roles?.admin, 'What connecting did') : ''}
    <details class="more"><summary>Show the details</summary><dl class="kv">
      <dt>Issuer</dt><dd><code>${p.issuer}</code></dd>
      ${s && !s.reachable && s.problem ? html`<dt>Last error</dt><dd><code>${s.problem}</code></dd>` : ''}
      <dt>Roles read from</dt><dd><code>${p.rolesClaim}</code></dd>
      <dt>Audience</dt><dd>${p.audience ? html`<code>${p.audience}</code>` : 'Any'}</dd>
      <dt>Connected by</dt><dd>${p.kind === 'keycloak' ? 'attest, with an administrator’s credential' : p.kind === 'config' ? 'A config file' : 'Hand, with a client made at the provider'}</dd>
    </dl></details>
    <div class="acts"><button class="btn" type="button" data-edit="provider">Connect again…</button></div>`;
}

function providerEdit() {
  return html`<form data-form="provider" novalidate>
    ${pf.form(st.form, { publicUrl: st.settings.publicUrl })}
    <div id="provider-effects" aria-live="polite">${providerEffects()}</div>
    <div class="acts"><button class="btn quiet" type="button" data-cancel>Cancel</button><span class="end"></span><button class="btn primary" type="submit" ${st.form.busy ? raw('aria-busy="true"') : ''}>${st.form.busy ? 'Connecting…' : 'Connect'}</button></div>
  </form>`;
}

/** What connecting this form will change, before it does. */
function providerEffects() {
  const cur = st.settings.provider;
  const next = pf.issuerOf(st.form);
  if (!cur || !next) return html``;
  const same = next === String(cur.issuer).replace(/\/+$/, '');
  if (same) {
    return html`<p class="consequence plain">attest checks its roles and sign-in client at ${host(next)} and adds anything missing there, such as the current public address. Nothing is removed, and nothing changes for people.</p>`;
  }
  return html`<div class="consequence"><p><b>This switches attest to another identity provider, at ${host(next)}.</b> From the moment it connects:</p>
    <p>People sign in through the new one. Their accounts there are new to attest, so they enroll again and choose again what assistants may do.</p>
    <p>Sensors and assistants need credentials from the new provider. The ones they hold now stop working.</p>
    <p>You stay signed in here until this session ends. After that, you need the administrator role at the new provider.</p></div>`;
}

/** The provider's failure in plain words; the raw text stays under the details. */
function unreachable(/** @type {string} */ raw) {
  if (/deadline|timeout|Timeout/.test(raw)) return 'attest asked, and it didn’t answer within 5 seconds.';
  if (/refused/.test(raw)) return 'Nothing is answering at its address.';
  if (/no such host|lookup/.test(raw)) return 'Its address doesn’t resolve: the name isn’t known on attest’s network.';
  if (/different issuer/.test(raw)) return 'It answers, but as a different provider, so its sign-ins would be refused.';
  if (/redirect/.test(raw)) return 'It answers with a redirect. Its address may have changed.';
  const code = /status (\d+)/.exec(raw)?.[1];
  if (code) return `It answers, but not as an identity provider (status ${code}).`;
  return 'attest can’t reach it.';
}

/* ---------- Public address ---------- */

function addressDetail() {
  const u = st.settings.publicUrl;
  const head = html`<div class="dh"><div><div class="kicker">Public address</div><h2>${u ? host(u) : 'Not set'}</h2></div></div>
    <p class="sub">Where people enroll, and where your identity provider sends them back after they sign in.</p>`;
  if (st.editing === 'address') {
    return html`${head}<form data-form="address" novalidate>
      <label class="field">New address<input data-f="addr" type="url" value="${st.addr}" autocomplete="off" spellcheck="false" ${st.addrError ? raw('aria-invalid="true" aria-describedby="addr-err"') : ''}></label>
      <div id="address-effects" aria-live="polite">${addressEffects()}</div>
      ${st.addrError ? html`<div id="addr-err">${problem(st.addrError)}</div>` : ''}
      <div class="acts"><button class="btn quiet" type="button" data-cancel>Cancel</button><span class="end"></span><button class="btn primary" type="submit" data-apply-address ${st.addrBusy ? raw('aria-busy="true"') : ''} ${changedAddress() ? '' : raw('disabled')}>Change address</button></div>
    </form>`;
  }
  const p = st.settings.provider;
  const moved = st.moved
    ? html`<p class="consequence ok">Changed from ${host(st.moved.from)}.</p>${p?.clientId
      ? html`<div class="consequence"><p>Your identity provider still sends people back to ${host(st.moved.from)}. ${p.kind === 'keycloak'
        ? 'Connect Keycloak again now, and attest adds the new address there.'
        : html`Add <code>${st.moved.to}/enroll/callback</code> to the sign-in client’s redirect addresses at your provider.`}</p>
        ${p.kind === 'keycloak' ? html`<div class="acts"><a class="btn primary" href="${href('connections', 'provider')}" data-edit-provider>Connect Keycloak again</a></div>` : ''}</div>` : ''}`
    : '';
  return html`${head}${moved}
    <dl class="kv sec first">
      <dt>Address</dt><dd>${u || 'Not set'}</dd>
      ${u ? html`<dt>Enrollment</dt><dd><a class="x" href="${u}/enroll" data-full>${host(u)}/enroll</a></dd>
      <dt>This console</dt><dd>${host(u)}/console</dd>` : ''}
    </dl>
    ${u && insecure(u) ? html`<p class="consequence">This address doesn’t use https, so sign-in cookies cross your network unencrypted.</p>` : ''}
    <div class="acts"><button class="btn" type="button" data-edit="address">Change address…</button></div>`;
}

function changedAddress() {
  try {
    const u = new URL(st.addr.trim());
    return /^https?:$/.test(u.protocol) && u.origin !== st.settings.publicUrl;
  } catch { return false; }
}

const insecure = (/** @type {string} */ u) => { try { const x = new URL(u); return x.protocol === 'http:' && !/^(localhost|127\.|\[::1\])/.test(x.hostname); } catch { return false; } };

function addressEffects() {
  if (!changedAddress()) return html``;
  const to = new URL(st.addr.trim()).origin;
  const from = st.settings.publicUrl;
  const p = st.settings.provider;
  return html`<div class="consequence plain"><p>Enrollment and this console move to ${host(to)}. Anyone who opens ${host(from)} is sent there.</p></div>
    ${p?.clientId ? html`<div class="consequence"><p>Your identity provider sends people back to ${host(from)} until you connect it again${p.kind === 'keycloak' ? ' (this page offers it next)' : ''}. Until then, signing in and enrolling at ${host(to)} fail.</p>
      <p>You stay signed in on this page. If you reload it, you’re sent to ${host(to)} and asked to sign in there.</p></div>` : ''}
    ${insecure(to) ? html`<p class="consequence">Without https, sign-in cookies cross your network unencrypted.</p>` : ''}`;
}

/* ---------- Events ---------- */

/** @param {HTMLElement} main */
function wire(main) {
  main.onclick = async (e) => {
    const t = /** @type {Element} */ (e.target);
    if (t.closest('[data-retry]')) { load(); return; }
    if (t.closest('[data-check]')) { st.checking = true; draw(); await check(); nav(); draw(); return; }
    const edit = t.closest('[data-edit]');
    if (edit) {
      st.editing = /** @type {any} */ (edit.getAttribute('data-edit'));
      if (st.editing === 'address') { st.addr = st.settings.publicUrl || location.origin; st.addrError = ''; }
      else { st.form = pf.blank(st.settings.provider); }
      draw();
      /** @type {HTMLElement|null} */ (main.querySelector('.detail input:not([type=radio]), .detail input'))?.focus();
      return;
    }
    if (t.closest('[data-edit-provider]')) {
      e.preventDefault();
      st.editing = 'provider';
      st.form = pf.blank(st.settings.provider);
      navigate(href('connections', 'provider'));
      st.editing = 'provider';
      draw();
      /** @type {HTMLElement|null} */ (main.querySelector('.detail [data-f="username"], .detail input'))?.focus();
      return;
    }
    if (t.closest('[data-cancel]')) { st.editing = null; st.form.error = ''; draw(); /** @type {HTMLElement|null} */ (main.querySelector('[data-edit]'))?.focus(); }
  };
  main.oninput = (e) => {
    const t = /** @type {HTMLInputElement} */ (e.target);
    if (t.dataset.f === 'addr') {
      st.addr = t.value;
      st.addrError = '';
      const out = main.querySelector('#address-effects');
      if (out) put(out, addressEffects());
      main.querySelector('[data-apply-address]')?.toggleAttribute('disabled', !changedAddress());
      return;
    }
    if (st.editing === 'provider') {
      if (pf.input(st.form, e)) { draw(); return; }
      const out = main.querySelector('#provider-effects');
      if (out) put(out, providerEffects());
    }
  };
  main.onchange = (e) => { if (/** @type {HTMLInputElement} */ (e.target).type === 'radio') main.oninput?.(e); };
  main.onsubmit = async (e) => {
    e.preventDefault();
    const which = /** @type {HTMLFormElement} */ (e.target).dataset.form;
    if (which === 'address') await applyAddress();
    if (which === 'provider') await applyProvider();
  };
}

async function applyAddress() {
  if (!changedAddress() || st.addrBusy) return;
  st.addrBusy = true;
  draw();
  const from = st.settings.publicUrl;
  try {
    const res = await putJSON('/v1/settings/public-url', { publicUrl: st.addr.trim() });
    st.settings.publicUrl = res.publicUrl;
    st.moved = { from, to: res.publicUrl };
    st.editing = null;
    announce(`Address changed to ${host(res.publicUrl)}.`);
  } catch (e) {
    st.addrError = e instanceof ApiError && e.status === 400
      ? 'That isn’t an address attest can use. It looks like https://attest.example.com: no path, no query.'
      : plain(e);
  }
  st.addrBusy = false;
  draw();
  /** @type {HTMLElement|null} */ (st.main?.querySelector(st.editing ? '[data-f="addr"]' : '.detail h2'))?.focus();
}

async function applyProvider() {
  if (st.form.busy) return;
  const missing = pf.missing(st.form);
  if (missing) { st.form.error = missing; draw(); return; }
  st.form.busy = true;
  draw();
  try {
    await pf.connect(st.form);
    st.lastChanges = st.form.changes;
    st.settings = await get('/v1/settings');
    st.editing = null;
    st.moved = null;
    announce('Connected.');
    await check();
    nav();
  } catch { /* the form shows what went wrong */ }
  draw();
}
