// Identities: the people attest can recognize, read-only. Accounts and roles
// live in the identity provider; people add themselves by enrolling.

import { html, raw, put, list, count, day, host, fromName } from '../lib/html.js';
import { get, plain } from '../lib/api.js';
import { page, loading, loadFailed, mark, narrow, keepFocus } from '../lib/ui.js';
import { href } from '../lib/router.js';

/**
 * @typedef {{ id: string, displayName: string, roles: string[], provider?: { issuer: string, subject: string }, voiceEnrolledAt?: string, createdAt: string }} Person
 * @typedef {{ id: string, identityId: string, agent: string, permissions: string[], grantedBy: string, grantedAt: string, revokedAt?: string, expiresAt?: string, zones?: string[], ceiling?: string }} Delegation
 */

const st = {
  /** @type {Person[]|null} */ people: null,
  /** @type {Delegation[]} */ delegations: [],
  /** @type {Record<string,string>} */ labels: {},
  publicUrl: '',
  error: '',
  /** @type {HTMLElement|null} */ main: null,
  /** @type {string|null} */ sel: null,
  pushed: false,
};

const nameOf = (/** @type {Person} */ x) => x.displayName || 'Someone unnamed';
const voice = raw('<svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><path d="M6 2v8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>');
const noVoice = raw('<svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><path d="M6 2v8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" opacity=".4"/></svg>');

/** @type {import('../lib/shell.js').Screen} */
export const identities = {
  id: 'identities',
  show(main, route) {
    const fresh = main.dataset.shown !== 'identities';
    st.main = main;
    main.dataset.shown = 'identities';
    st.pushed = !!route.id;
    st.sel = route.id;
    if (fresh || !st.people) load();
    else draw();
    if (route.id && narrow()) { /** @type {HTMLElement|null} */ (main.querySelector('.detail'))?.focus({ preventScroll: true }); scrollTo(0, 0); }
  },
  items: () => (st.people || []).map((x) => ({ label: nameOf(x), href: href('identities', x.id), group: 'Identities' })),
};

async function load() {
  st.error = '';
  draw();
  try {
    const [people, delegations, policy, pub] = await Promise.all([
      get('/v1/identities'), get('/v1/delegations'), get('/v1/settings/policy'), get('/v1/settings/public-url'),
    ]);
    st.people = (people || []).sort((/** @type {Person} */ a, /** @type {Person} */ b) => nameOf(a).localeCompare(nameOf(b)));
    st.delegations = delegations || [];
    st.labels = policy?.labels || {};
    st.publicUrl = pub?.publicUrl || '';
  } catch (e) {
    st.error = plain(e);
  }
  draw();
}

/** @param {string} id */
function live(id) {
  const now = Date.now();
  return st.delegations.filter((d) => d.identityId === id && !d.revokedAt && !(d.expiresAt && Date.parse(d.expiresAt) <= now));
}

function draw() {
  if (!st.main || st.main.dataset.shown !== 'identities') return;
  keepFocus(() => put(/** @type {HTMLElement} */ (st.main), view()));
  /** @type {HTMLElement} */ (st.main).onclick = (e) => { if (/** @type {Element} */ (e.target).closest('[data-retry]')) load(); };
}

function view() {
  const lede = 'The people attest can recognize. Accounts and roles live in your identity provider; each person enrolls and chooses what assistants may do for them.';
  const back = { href: href('identities'), label: 'Identities' };
  if (st.error) return page({ title: 'Identities', lede, master: loadFailed(`Couldn’t load people. ${st.error}`), detail: '', pushed: false, back });
  if (!st.people) return page({ title: 'Identities', lede, master: loading(), detail: '', pushed: false, back });
  const people = st.people;
  if (!st.sel && !narrow() && people.length) st.sel = people[0].id;
  const enroll = st.publicUrl ? `${host(st.publicUrl)}/enroll` : '/enroll';
  const row = (/** @type {Person} */ x) => {
    const n = live(x.id).length;
    return html`<a class="row r-id" href="${href('identities', x.id)}" ${st.sel === x.id ? raw('aria-current="true"') : ''}><span class="name ellipsis">${nameOf(x)}</span><span class="sub ellipsis">${x.roles.length ? x.roles.join(', ') : html`<span class="dim">No role</span>`}</span><span class="mod ${x.voiceEnrolledAt ? '' : 'off'}">${x.voiceEnrolledAt ? voice : noVoice}${x.voiceEnrolledAt ? 'Voice enrolled' : 'No voice'}</span><span class="sub end small">${n ? `${count(n, 'assistant', 'assistants')} allowed` : html`<span class="dim">Nothing allowed</span>`}</span></a>`;
  };
  const master = people.length
    ? html`<div class="colhead r-id" aria-hidden="true"><span>Person</span><span>Roles</span><span>Voice</span><span class="end">Has allowed</span></div>${people.map(row)}
      <p class="note">People appear here after they sign in at ${enroll}. Their roles are refreshed from your identity provider each time they do. To add someone, add them there and send them that address.</p>`
    : html`<p class="note lead">Nobody has enrolled yet. People add themselves: send them to <a class="x" href="/enroll" data-full>${enroll}</a>, where they sign in with their account at your identity provider.</p>`;
  const x = people.find((p) => p.id === st.sel);
  const detail = x ? personDetail(x) : html`<p class="empty-detail">${people.length ? 'Select someone to see their enrollment and what they have allowed.' : ''}</p>`;
  return page({ title: 'Identities', lede, master, detail, pushed: st.pushed && !!x, back });
}

/** @param {Person} x */
function personDetail(x) {
  const ds = live(x.id);
  const what = (/** @type {Delegation} */ d) => d.permissions.includes('*') ? 'anything they may do' : list(d.permissions.map((p) => (st.labels[p] || fromName(p)).toLowerCase()), 'nothing');
  const limits = (/** @type {Delegation} */ d) => [
    d.zones?.length ? `only in ${list(d.zones.map(fromName))}` : '',
    d.ceiling ? `up to ${d.ceiling}` : '',
    d.expiresAt ? `until ${day(d.expiresAt)}` : '',
  ].filter(Boolean).join(', ');
  return html`<div class="dh"><div><div class="kicker">Person</div><h2>${nameOf(x)}</h2></div>${x.voiceEnrolledAt ? html`<span class="oc">${mark('ok')}Voice enrolled</span>` : html`<span class="oc">${mark('skip')}No voice</span>`}</div>
    <dl class="kv">
      <dt>Roles</dt><dd>${x.roles.length ? x.roles.join(', ') : 'None that policy uses'}</dd>
      <dt>Voice</dt><dd>${x.voiceEnrolledAt ? `Enrolled ${day(x.voiceEnrolledAt)}` : 'Not enrolled'}</dd>
      <dt>Account</dt><dd>${x.provider ? html`At ${host(x.provider.issuer)}` : 'Not linked to an account'}</dd>
      <dt>Since</dt><dd>${day(x.createdAt)}</dd>
    </dl>
    <div class="sec first"><h3>What they’ve allowed</h3>
      ${ds.length ? html`<ul class="lines">${ds.map((d) => html`<li class="block"><b class="strong">${d.agent}</b> may ${what(d)}${d.grantedBy !== x.id ? html`<span class="dim small">. Set by an administrator${limits(d) ? `: ${limits(d)}` : ''}.</span>` : limits(d) ? html`<span class="dim small"> (${limits(d)})</span>` : ''}</li>`)}</ul>`
        : html`<p class="note">Nothing yet. They choose at /enroll, after they sign in.</p>`}
    </div>
    <details class="more"><summary>Show the details</summary><dl class="kv">
      <dt>attest ID</dt><dd><code>${x.id}</code></dd>
      ${x.provider ? html`<dt>Account ID</dt><dd><code>${x.provider.subject}</code></dd>` : ''}
    </dl></details>
    <p class="small dim">Roles come from your identity provider, and the rules for them from <a class="x" href="${href('policy')}">Policy</a>.</p>`;
}
