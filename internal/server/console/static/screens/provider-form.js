// Connecting an identity provider: the same form in first-run setup and in
// Connections. It shows only adapters that work; the others say so.

import { html, raw, host } from '../lib/html.js';
import { post, plain, sentence, ApiError } from '../lib/api.js';
import { mark, problem } from '../lib/ui.js';

/**
 * @typedef {{ object: string, name: string, action: string, note?: string }} Change
 * @typedef {{ kind: 'keycloak'|'oidc', url: string, realm: string, auth: 'user'|'service', username: string, password: string,
 *   clientId: string, clientSecret: string, adminRealm: string, enrollClient: string, issuer: string, oidcClient: string,
 *   busy: boolean, error: string, changes: Change[], adminRole: string, done: boolean }} FormState
 */

/** @returns {FormState} */
export function blank(/** @type {any} */ current) {
  const st = /** @type {FormState} */ ({
    kind: 'keycloak', url: '', realm: '', auth: 'user', username: '', password: '', clientId: '', clientSecret: '',
    adminRealm: '', enrollClient: '', issuer: '', oidcClient: '', busy: false, error: '', changes: [], adminRole: 'attest-admin', done: false,
  });
  if (current) {
    st.adminRole = current.roles?.admin || st.adminRole;
    if (current.kind === 'keycloak') {
      const m = /^(.*)\/realms\/([^/]+)\/?$/.exec(current.issuer || '');
      if (m) { st.url = m[1]; st.realm = decodeURIComponent(m[2]); }
      st.enrollClient = current.clientId && current.clientId !== 'attest-enroll' ? current.clientId : '';
    } else if (current.kind === 'oidc' || current.kind === 'config') {
      st.kind = 'oidc';
      st.issuer = current.issuer || '';
      st.oidcClient = current.clientId || '';
    }
  }
  return st;
}

/** The issuer this form would connect, where it can tell. @param {FormState} st */
export function issuerOf(st) {
  if (st.kind === 'oidc') return st.issuer.trim().replace(/\/+$/, '');
  if (!st.url.trim() || !st.realm.trim()) return '';
  return `${st.url.trim().replace(/\/+$/, '')}/realms/${encodeURIComponent(st.realm.trim())}`;
}

const field = (/** @type {string} */ label, /** @type {string} */ key, /** @type {string} */ value, /** @type {Record<string,string>} */ a = {}) =>
  html`<label class="field">${label}<input data-f="${key}" value="${value}" ${raw(Object.entries(a).map(([k, v]) => `${k}="${v}"`).join(' '))}>${a['data-hint'] ? html`<span class="hint">${a['data-hint']}</span>` : ''}</label>`;

/**
 * @param {FormState} st
 * @param {{ publicUrl: string }} ctx
 */
export function form(st, ctx) {
  const option = (/** @type {string} */ value, /** @type {string} */ name, /** @type {string} */ sub, on = true) => html`<li><label class="${on ? '' : 'off'}"><input type="radio" name="idp-kind" data-kind="${value}" value="${value}" ${st.kind === value ? raw('checked') : ''} ${on ? '' : raw('disabled aria-describedby="na-' + value + '"')}>${name}<span class="sub" ${on ? '' : raw('id="na-' + value + '"')}>${sub}</span></label></li>`;
  const kc = html`
    <div class="fields-2">${field('Keycloak address', 'url', st.url, { type: 'url', placeholder: 'https://idp.example.com', autocomplete: 'off', spellcheck: 'false', required: '' })}
    ${field('Realm', 'realm', st.realm, { placeholder: 'The realm your people are in', autocomplete: 'off', spellcheck: 'false', required: '' })}</div>
    <ul class="choice tight" aria-label="How attest signs in to Keycloak">
      <li><label><input type="radio" name="idp-auth" data-auth="user" ${st.auth === 'user' ? raw('checked') : ''}>An administrator\u2019s username and password<span class="sub">Also makes that account attest\u2019s first administrator</span></label></li>
      <li><label><input type="radio" name="idp-auth" data-auth="service" ${st.auth === 'service' ? raw('checked') : ''}>A service client with realm-admin</label></li>
    </ul>
    ${st.auth === 'user'
      ? html`<div class="fields-2">${field('Username', 'username', st.username, { autocomplete: 'off', spellcheck: 'false' })}${field('Password', 'password', st.password, { type: 'password', autocomplete: 'off' })}</div>`
      : html`<div class="fields-2">${field('Client ID', 'clientId', st.clientId, { autocomplete: 'off', spellcheck: 'false' })}${field('Client secret', 'clientSecret', st.clientSecret, { type: 'password', autocomplete: 'off' })}</div>`}
    <p class="note">Used once, for this step. attest doesn\u2019t keep it.</p>
    <details class="more"><summary>More options</summary>
      <div class="fields-2">${field('Administrator’s realm', 'adminRealm', st.adminRealm, { placeholder: 'Same as above', autocomplete: 'off', spellcheck: 'false', 'data-hint': 'If the account above lives in another realm, such as master' })}
      ${field('Sign-in client', 'enrollClient', st.enrollClient, { placeholder: 'attest-enroll', autocomplete: 'off', spellcheck: 'false', 'data-hint': 'Created if it is missing' })}</div>
    </details>`;
  const manual = html`
    ${field('Issuer address', 'issuer', st.issuer, { type: 'url', placeholder: 'https://idp.example.com/issuer', autocomplete: 'off', spellcheck: 'false' })}
    ${field('Sign-in client ID', 'oidcClient', st.oidcClient, { autocomplete: 'off', spellcheck: 'false' })}
    <p class="note">Create a public sign-in client that requires PKCE (S256), with this redirect address: <code>${(ctx.publicUrl || 'https://attest.example.com') + '/enroll/callback'}</code>. People and administrators sign in through it. Then give your own account the role <code>${st.adminRole}</code> there.</p>`;
  return html`<fieldset class="bare" ${st.busy ? raw('disabled') : ''}>
    <legend class="sr">Identity provider</legend>
    <ul class="choice">
      ${option('keycloak', 'Keycloak', st.url ? host(st.url) : 'attest sets it up for you')}
      ${option('oidc', 'Another provider', 'You create the sign-in client')}
      ${option('auth0', 'Auth0', 'Not yet available', false)}
      ${option('ory', 'Ory', 'Not yet available', false)}
    </ul>
    ${st.kind === 'keycloak' ? kc : manual}
  </fieldset>
  ${st.error ? problem(st.error) : ''}
  ${st.changes.length ? changesView(st.changes, st.adminRole, st.error ? 'Done before it stopped' : 'What attest did') : ''}`;
}

/**
 * Reads one input into the form state. Returns true when the form must be
 * redrawn (a choice changed what is shown).
 * @param {FormState} st @param {Event} e
 */
export function input(st, e) {
  const t = /** @type {HTMLInputElement} */ (e.target);
  if (t.dataset.kind) { st.kind = /** @type {any} */ (t.dataset.kind); st.error = ''; return true; }
  if (t.dataset.auth) { st.auth = /** @type {any} */ (t.dataset.auth); st.error = ''; return true; }
  const key = t.dataset.f;
  if (key && key in st) { /** @type {any} */ (st)[key] = t.value; }
  return false;
}

/** What is missing before connecting, in words. @param {FormState} st */
export function missing(st) {
  if (st.kind === 'oidc') {
    if (!/^https?:\/\/[^/]/.test(st.issuer.trim())) return 'Enter the issuer address, starting with https://.';
    return '';
  }
  if (!/^https?:\/\/[^/]/.test(st.url.trim())) return 'Enter Keycloak’s address, starting with https://.';
  if (!st.realm.trim()) return 'Enter the realm your people are in.';
  if (st.auth === 'user' && (!st.username || !st.password)) return 'Enter the administrator’s username and password.';
  if (st.auth === 'service' && (!st.clientId || !st.clientSecret)) return 'Enter the service client’s ID and secret.';
  return '';
}

/**
 * Connects. On success the credential is cleared from memory.
 * @param {FormState} st
 * @returns {Promise<any>} the connected provider view
 */
export async function connect(st) {
  const body = st.kind === 'oidc'
    ? { kind: 'oidc', issuer: st.issuer.trim(), clientId: st.oidcClient.trim() || undefined }
    : {
      kind: 'keycloak', url: st.url.trim().replace(/\/+$/, ''), realm: st.realm.trim(),
      adminRealm: st.adminRealm.trim() || undefined, clientId: st.enrollClient.trim() || undefined,
      credential: st.auth === 'user' ? { username: st.username, password: st.password } : { clientId: st.clientId, clientSecret: st.clientSecret },
    };
  st.busy = true; st.error = ''; st.changes = [];
  try {
    const res = await post('/v1/settings/provider', body);
    st.changes = res.changes || [];
    st.adminRole = res.provider?.roles?.admin || st.adminRole;
    st.done = true;
    st.password = ''; st.clientSecret = '';
    return res.provider;
  } catch (e) {
    st.error = connectError(e, st);
    st.changes = (e instanceof ApiError && e.body?.changes) || [];
    throw e;
  } finally {
    st.busy = false;
  }
}

/** @param {unknown} e @param {FormState} st */
function connectError(e, st) {
  if (!(e instanceof ApiError) || !e.body?.error) return plain(e);
  const msg = String(e.body.error).replace(/^invalid request: /, '');
  const where = st.kind === 'keycloak' ? host(st.url) : host(st.issuer);
  if (e.status === 501) return 'attest can’t connect this provider by itself yet. Choose “Another provider” and create the sign-in client yourself.';
  if (/refused the admin credential/.test(msg)) {
    const why = /\(\d+: (.*)\)/.exec(msg)?.[1];
    return `Keycloak didn’t accept that ${st.auth === 'user' ? 'username and password' : 'client ID and secret'}${why ? ` (${why})` : ''}. Check them, and the realm the account is in.`;
  }
  if (/^discovery at .*: (status 404|not an OIDC)/.test(msg)) return `${where} answered, but not as ${st.kind === 'keycloak' ? 'a Keycloak realm' : 'an identity provider'}. Check the address${st.kind === 'keycloak' ? ' and the realm name' : ''}.`;
  if (/^discovery at |unreachable/.test(msg)) return `attest couldn’t reach ${where}. Check the address, and that attest’s server can reach it.`;
  if (/may not .* realm-admin/.test(msg)) return 'That account may not change this realm. It needs the realm-admin role there.';
  if (/answered with a redirect/.test(msg)) return `${where} answered with a redirect, which attest doesn’t follow with a password. Check the address: it may need https, or a different path.`;
  if (/public URL must be set/.test(msg)) return 'Set attest’s public address first.';
  if (/already exists and is confidential/.test(msg)) return 'A client with that name already exists in the realm and has a secret, so people can’t sign in through it. Choose another sign-in client name under More options.';
  return sentence(msg);
}

/**
 * What connecting did at the provider, one line each.
 * @param {Change[]} changes @param {string} adminRole @param {string} title
 */
export function changesView(changes, adminRole, title) {
  const line = (/** @type {Change} */ c) => {
    /** @type {[import('../lib/ui.js').Kind, string]} */
    let out;
    if (c.object === 'role') out = ['ok', c.action === 'created' ? `Created the role ${c.name}` : `The role ${c.name} was already there`];
    else if (c.object === 'client') {
      out = ['ok', c.action === 'created' ? `Created the sign-in client ${c.name}`
        : c.action === 'updated' ? `Updated the sign-in client ${c.name}: ${(c.note || '').replace(/redirect URI/g, 'return address')}`
          : `The sign-in client ${c.name} was already set up`];
    } else if (c.object === 'role grant') {
      out = c.action === 'created' ? ['ok', `Gave ${c.name} to the account you connected with`]
        : c.action === 'found' ? ['ok', `The account you connected with already has ${c.name}`]
          : ['warn', grantSkipped(c, adminRole)];
    } else out = ['ok', `${c.object} ${c.name}: ${c.action}`];
    return html`<li>${mark(out[0])}<span>${out[1]}</span></li>`;
  };
  return html`<div class="sec first"><h3>${title}</h3><ul class="lines">${changes.map(line)}</ul></div>`;
}

/** @param {Change} c @param {string} role */
function grantSkipped(c, role) {
  const note = c.note || '';
  const realm = /grant .* to a person in (.+)$/.exec(note)?.[1];
  if (/service client/.test(note)) return `Nobody was given ${role}: you connected with a service client. Give ${role} to your own account in Keycloak.`;
  if (/lives in realm/.test(note)) return `Nobody was given ${role}: the account you connected with is in another realm. Give ${role} to your own account${realm ? ` in the realm ${realm}` : ''}.`;
  if (/not a user of this realm/.test(note)) return `Nobody was given ${role}: the account you connected with isn’t in this realm. Give ${role} to your own account in Keycloak.`;
  return `Nobody was given ${role}. Give it to your own account in Keycloak.`;
}

/** Whether connecting made someone an administrator. @param {Change[]} changes */
export function adminGranted(changes) {
  return changes.some((c) => c.object === 'role grant' && (c.action === 'created' || c.action === 'found'));
}
