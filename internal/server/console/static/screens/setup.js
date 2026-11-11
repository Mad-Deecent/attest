// First run: the setup code from the log, the public address, the identity
// provider, finish. One page that only fetches until the end, so a change of
// address mid-way never strands the setup session on another host.

import { html, raw, put, host } from '../lib/html.js';
import { get, post, put as putJSON, setCSRF, plain, ApiError } from '../lib/api.js';
import { problem, announce } from '../lib/ui.js';
import * as pf from './provider-form.js';

const KEY_CSRF = 'attest.setup.csrf';
const KEY_CHANGES = 'attest.setup.changes';

/**
 * @typedef {{ step: number, code: string, address: string, busy: boolean, error: string, settings: any,
 *   form: pf.FormState, reconnect: boolean, finished: boolean }} SetupState
 */

const store = {
  get: (/** @type {string} */ k) => { try { return sessionStorage.getItem(k) || ''; } catch { return ''; } },
  set: (/** @type {string} */ k, /** @type {string} */ v) => { try { sessionStorage.setItem(k, v); } catch { /* private mode */ } },
  clear: () => { try { sessionStorage.removeItem(KEY_CSRF); sessionStorage.removeItem(KEY_CHANGES); } catch { /* private mode */ } },
};

/** @param {HTMLElement} app */
export async function setupFlow(app) {
  /** @type {SetupState} */
  const s = { step: 1, code: '', address: '', busy: false, error: '', settings: null, form: pf.blank(null), reconnect: false, finished: false };
  document.title = 'Set up · attest';

  // A reload keeps the session: the cookie survives and the token is kept
  // for this tab only.
  const csrf = store.get(KEY_CSRF);
  if (csrf) {
    setCSRF(csrf);
    try {
      await load(s);
      s.step = !s.settings.publicUrl ? 2 : !s.settings.provider ? 3 : 4;
      try { s.form.changes = JSON.parse(store.get(KEY_CHANGES) || '[]'); } catch { s.form.changes = []; }
    } catch {
      store.clear();
      setCSRF('');
    }
  }

  const draw = (focus = true) => {
    put(app, view(s));
    if (focus) /** @type {HTMLElement|null} */ (app.querySelector('h1'))?.focus({ preventScroll: true });
    if (focus && s.step === 1) /** @type {HTMLElement|null} */ (app.querySelector('[data-f="code"]'))?.focus();
  };

  app.oninput = (e) => {
    const t = /** @type {HTMLInputElement} */ (e.target);
    if (t.dataset.f === 'code') { s.code = t.value; return; }
    if (t.dataset.f === 'address') {
      s.address = t.value;
      const out = app.querySelector('#address-effects');
      if (out) put(out, addressEffects(s.address));
      return;
    }
    if (pf.input(s.form, e)) { draw(false); /** @type {HTMLElement|null} */ (app.querySelector(`[data-${t.dataset.kind ? 'kind' : 'auth'}="${t.dataset.kind || t.dataset.auth}"]`))?.focus(); }
  };
  app.onchange = app.oninput;

  app.onclick = async (e) => {
    const t = /** @type {Element} */ (e.target);
    if (t.closest('[data-back]')) { s.error = ''; s.step = Math.max(2, s.step - 1); draw(); return; }
    if (t.closest('[data-reconnect]')) { s.reconnect = true; s.form = pf.blank(s.settings.provider); draw(false); app.querySelector('input')?.focus(); return; }
    if (t.closest('[data-next]')) { s.error = ''; s.step += 1; s.reconnect = false; draw(); }
  };

  app.onsubmit = async (e) => {
    e.preventDefault();
    if (s.busy || s.form.busy) return;
    const step = Number(/** @type {HTMLFormElement} */ (e.target).dataset.step);
    s.error = '';
    try {
      if (step === 1) await openSession(s);
      else if (step === 2) await saveAddress(s);
      else if (step === 3) await connect(s, draw);
      else if (step === 4) await finish(s);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401 && step > 1) {
        // The 30-minute session lapsed. Asking for status makes attest
        // print a fresh code.
        store.clear(); setCSRF('');
        try { await get('/v1/setup/status'); } catch { /* shown below either way */ }
        s.step = 1;
        s.error = 'Your setup session ended after 30 minutes. attest has printed a new code in its log: enter it to carry on where you left off.';
      } else if (err instanceof ApiError && err.status === 410) {
        location.assign('/console/');
        return;
      } else if (step !== 3) {
        s.error = step === 1 && err instanceof ApiError && err.status === 401
          ? 'That code didn’t work. Use the most recent code in attest’s log, exactly as printed. Each code works once.'
          : plain(err);
      }
    }
    s.busy = false;
    draw(step !== 3 || s.step !== 3 || s.error !== '');
    if (s.step === 3 && !s.error) announce(s.form.error || 'Connected.');
  };
  draw();
}

/** @param {SetupState} s */
async function load(s) {
  s.settings = await get('/v1/settings');
  s.address = s.settings.publicUrl || location.origin;
}

/** @param {SetupState} s */
async function openSession(s) {
  if (!s.code.trim()) { s.error = 'Enter the setup code from attest’s log.'; return; }
  s.busy = true;
  const res = await post('/v1/setup/session', { code: s.code.trim() });
  setCSRF(res.csrf);
  store.set(KEY_CSRF, res.csrf);
  await load(s);
  s.step = 2;
}

/** @param {SetupState} s */
async function saveAddress(s) {
  s.busy = true;
  try {
    const res = await putJSON('/v1/settings/public-url', { publicUrl: s.address.trim() });
    s.settings.publicUrl = res.publicUrl;
    s.step = 3;
  } catch (e) {
    if (e instanceof ApiError && e.status === 400) {
      s.error = 'That isn’t an address attest can use. It looks like https://attest.example.com: no path, no query.';
      return;
    }
    throw e;
  }
}

/** @param {SetupState} s @param {(focus?: boolean) => void} draw */
async function connect(s, draw) {
  const missing = pf.missing(s.form);
  if (missing) { s.form.error = missing; return; }
  s.form.busy = true;
  draw(false);
  try {
    s.settings.provider = await pf.connect(s.form);
    s.reconnect = false;
    store.set(KEY_CHANGES, JSON.stringify(s.form.changes));
  } catch (e) {
    if (e instanceof ApiError && (e.status === 401 || e.status === 410)) throw e;
  }
}

/** @param {SetupState} s */
async function finish(s) {
  s.busy = true;
  try {
    await post('/v1/setup/complete');
  } catch (e) {
    if (e instanceof ApiError && e.status === 503) {
      s.error = 'Your identity provider isn’t answering, so attest can’t finish yet. Check that it’s running and that attest can reach it, then try again.';
      return;
    }
    if (e instanceof ApiError && e.status === 409) {
      s.error = 'Set the address and connect an identity provider first.';
      return;
    }
    throw e;
  }
  store.clear();
  setCSRF('');
  s.finished = true;
}

/** @param {string} raw */
function addressEffects(raw) {
  let u;
  try { u = new URL(raw.trim()); } catch { return html``; }
  if (!/^https?:$/.test(u.protocol)) return html``;
  const lines = [];
  if (u.host !== location.host) {
    lines.push(html`<p class="consequence plain">You’re using attest at ${location.host} right now. Once setup is finished, sign-in and this console move to ${u.host}.</p>`);
  }
  if (u.protocol === 'http:' && !/^(localhost|127\.|\[::1\])/.test(u.hostname)) {
    lines.push(html`<p class="consequence">Without https, anyone on your network can read sign-in cookies on their way to attest. Use https if people reach attest over a network you don’t fully trust.</p>`);
  }
  return html`${lines}`;
}

/** @param {SetupState} s */
function view(s) {
  if (s.finished) {
    const role = s.form.adminRole;
    return html`<div class="flow"><div class="flow-in">
      <div class="brand"><b>attest</b><span>Set up</span></div><div class="flow-head"></div>
      <h1 tabindex="-1">attest is set up</h1>
      <p class="lede">From now on you get here by signing in through your identity provider, with an account that has the role <code>${role}</code>. The setup code no longer works.</p>
      <div class="flow-foot"><span></span><a class="btn primary" href="/console/login?return=%2Fconsole%2Fpolicy" data-full>Sign in</a></div>
      <p class="after">People enroll at <a class="x" href="${(s.settings?.publicUrl || '') + '/enroll'}" data-full>${host(s.settings?.publicUrl || location.origin)}/enroll</a>.</p>
    </div></div>`;
  }
  const steps = html`<ol class="steps" aria-label="Step ${s.step} of 4">${[1, 2, 3, 4].map((i) => html`<li class="${i <= s.step ? 'on' : ''}"></li>`)}</ol>`;
  const head = html`<div class="brand"><b>attest</b><span>Set up · ${s.step} of 4</span></div>${steps}`;
  const err = s.error ? problem(s.error) : '';
  const foot = (/** @type {string} */ label, back = true, enabled = true) => html`<div class="flow-foot">${back ? html`<button class="btn quiet" type="button" data-back>Back</button>` : html`<span></span>`}<button class="btn primary" type="submit" ${s.busy ? raw('aria-busy="true"') : ''} ${enabled ? '' : raw('disabled')}>${label}</button></div>`;

  let body;
  if (s.step === 1) {
    body = html`<form data-step="1" novalidate>
      <h1 tabindex="-1">Enter your setup code</h1>
      <p class="lede">When attest started, it printed a one-time setup code in its log. Entering it here proves you run this server.</p>
      <label class="field">Setup code<input class="code" data-f="code" value="${s.code}" placeholder="ABCD-EFGH-JKMN-PQRS" autocomplete="one-time-code" autocapitalize="characters" spellcheck="false" ${s.error ? raw('aria-invalid="true"') : ''}><span class="hint">In the log, look for “One-time setup code”.</span></label>
      ${err}${foot('Continue', false)}</form>`;
  } else if (s.step === 2) {
    body = html`<form data-step="2" novalidate>
      <h1 tabindex="-1">Where will people reach attest?</h1>
      <p class="lede">People enroll at this address, and your identity provider sends them back to it after they sign in. Use the address they will type.</p>
      <label class="field">Public address<input data-f="address" type="url" value="${s.address}" autocomplete="off" spellcheck="false" ${s.error ? raw('aria-invalid="true"') : ''}></label>
      <div id="address-effects" aria-live="polite">${addressEffects(s.address)}</div>
      ${err}${foot('Continue')}</form>`;
  } else if (s.step === 3) {
    const p = s.settings.provider;
    const connected = p && !s.reconnect;
    body = html`<form data-step="3" novalidate>
      <h1 tabindex="-1">Connect your identity provider</h1>
      <p class="lede">People, their accounts and their roles stay in your identity provider. attest signs in to it once and adds what it needs: three roles, and a sign-in client for enrollment and this console.</p>
      ${connected
        ? html`<dl class="kv"><dt>Connected to</dt><dd>${p.kind === 'keycloak' ? 'Keycloak' : 'Identity provider'} at ${host(p.issuer)}</dd><dt>Sign-in client</dt><dd>${p.clientId ? html`<code>${p.clientId}</code>` : 'None'}</dd></dl>
          ${s.form.changes.length ? pf.changesView(s.form.changes, s.form.adminRole, 'What attest did') : ''}
          <div class="flow-foot"><button class="btn quiet" type="button" data-back>Back</button><span class="btns"><button class="btn quiet" type="button" data-reconnect>Connect again</button><button class="btn primary" type="button" data-next>Continue</button></span></div>`
        : html`${pf.form(s.form, { publicUrl: s.settings.publicUrl })}
          <div class="flow-foot"><button class="btn quiet" type="button" data-back>Back</button><button class="btn primary" type="submit" ${s.form.busy ? raw('aria-busy="true"') : ''}>${s.form.busy ? 'Connecting…' : s.form.kind === 'keycloak' ? 'Connect Keycloak' : 'Connect'}</button></div>`}
      </form>`;
  } else {
    const p = s.settings.provider || {};
    const role = p.roles?.admin || s.form.adminRole;
    const known = s.form.changes.length > 0;
    const granted = pf.adminGranted(s.form.changes);
    const who = known && granted
      ? html`<p class="consequence ok">The account you connected with has <code>${role}</code>. Sign in with it once setup is finished.</p>`
      : known
        ? html`<p class="consequence"><b>Nobody can sign in to this console yet.</b> In ${p.kind === 'keycloak' ? 'Keycloak' : 'your identity provider'}, give your own account the role <code>${role}</code>${realmOf(p) ? html` in the realm <b>${realmOf(p)}</b>` : ''}, then finish and sign in.</p>`
        : html`<p class="consequence plain">Sign in with an account that has the role <code>${role}</code> once setup is finished.</p>`;
    body = html`<form data-step="4" novalidate>
      <h1 tabindex="-1">Finish setup</h1>
      <p class="lede">After this, the setup code stops working and this console asks you to sign in.</p>
      <dl class="kv"><dt>Public address</dt><dd>${s.settings.publicUrl}</dd>
        <dt>Identity provider</dt><dd>${p.kind === 'keycloak' ? 'Keycloak' : 'Identity provider'} at ${host(p.issuer || '')}${realmOf(p) ? html`, realm ${realmOf(p)}` : ''}</dd>
        <dt>Administrators</dt><dd>Accounts with the role <code>${role}</code></dd></dl>
      ${who}
      <p class="note">Locked out later? Restart attest once with <code>-reset-setup</code> and it prints a new setup code.</p>
      ${err}${foot('Finish setup')}</form>`;
  }
  return html`<div class="flow"><div class="flow-in">${head}${body}</div></div>`;
}

/** The realm a Keycloak issuer names. @param {any} p */
function realmOf(p) {
  if (p?.kind !== 'keycloak') return '';
  const m = /\/realms\/([^/]+)\/?$/.exec(p.issuer || '');
  return m ? decodeURIComponent(m[1]) : '';
}

