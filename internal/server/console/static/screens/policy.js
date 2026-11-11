// Policy: actions × roles, the level each action needs, and which assistants
// may ask for it. Edits are staged; each one is restated as what it does to
// real people before anything is applied.

import { html, raw, put, list, count, fromName } from '../lib/html.js';
import { get, put as putJSON, plain } from '../lib/api.js';
import { page, loading, loadFailed, mark, problem, announce, narrow, keepFocus, chevron } from '../lib/ui.js';
import { levels, level, tag, ladder, ANYONE } from '../lib/level.js';
import { href, navigate } from '../lib/router.js';
import { nav } from '../lib/shell.js';

/**
 * @typedef {{ bands: import('../lib/level.js').Band[], permissions: Record<string,string>, floor?: string[],
 *   agents: Record<string,string[]>, roles?: Record<string,string[]>, labels?: Record<string,string> }} Policy
 * @typedef {{ id: string, displayName: string, roles: string[] }} Person
 * @typedef {{ identityId: string, agent: string, permissions: string[], revokedAt?: string, expiresAt?: string }} Delegation
 * @typedef {{ name: string, label: string, level: string, roles: string[], agents: string[] }} Action
 * @typedef {{ warn: boolean, text: import('../lib/html.js').Markup, action?: string }} Change
 */

const DRAFT_KEY = 'attest.policy.draft';

const st = {
  /** @type {Policy|null} */ base: null,
  /** @type {Policy|null} */ draft: null,
  /** @type {Person[]} */ people: [],
  /** @type {Delegation[]} */ delegations: [],
  error: '',
  applyError: '',
  applied: '',
  busy: false,
  /** @type {HTMLElement|null} */ main: null,
  /** @type {string|null} */ sel: null,
  pushed: false,
  /** @type {null|'action'|'role'|'agent'} */ adding: null,
  add: { label: '', name: '', /** @type {string|null} */ level: null, role: '', agent: '', error: '' },
};

/** @type {import('../lib/shell.js').Screen} */
export const policy = {
  id: 'policy',
  show(main, route) {
    const fresh = main.dataset.shown !== 'policy';
    st.main = main;
    main.dataset.shown = 'policy';
    st.pushed = !!route.id;
    st.sel = route.id;
    if (fresh || !st.base) load();
    else draw();
    if (route.id && narrow()) { /** @type {HTMLElement|null} */ (main.querySelector('.detail'))?.focus({ preventScroll: true }); scrollTo(0, 0); }
  },
  items() {
    return st.base ? actions(cur()).map((a) => ({ label: a.label, href: href('policy', a.name), group: 'Policy' })) : [];
  },
  pending: () => changes().length > 0,
};

/* ---------- Data ---------- */

const cur = () => /** @type {Policy} */ (st.draft || st.base);

/** Sorted, stable JSON, to compare policies. @param {unknown} v @returns {string} */
function stable(v) {
  if (Array.isArray(v)) return `[${v.map(stable).join(',')}]`;
  if (v && typeof v === 'object') {
    return `{${Object.keys(v).filter((k) => /** @type {any} */ (v)[k] !== undefined).sort().map((k) => `${JSON.stringify(k)}:${stable(/** @type {any} */ (v)[k])}`).join(',')}}`;
  }
  return JSON.stringify(v);
}

/** Exactly the policy fields attest accepts. @param {Policy} p @returns {Policy} */
function clean(p) {
  return {
    bands: p.bands || [], permissions: p.permissions || {}, floor: p.floor || [], agents: p.agents || {},
    roles: p.roles || {}, labels: p.labels || {},
  };
}

/** @param {Policy} p */
const clone = (p) => /** @type {Policy} */ (JSON.parse(JSON.stringify(clean(p))));

async function load() {
  st.error = '';
  draw();
  try {
    const [p, people, delegations] = await Promise.all([get('/v1/settings/policy'), get('/v1/identities'), get('/v1/delegations')]);
    st.base = clean(p);
    st.people = people || [];
    st.delegations = delegations || [];
    restoreDraft();
  } catch (e) {
    st.error = plain(e);
  }
  draw();
  nav();
}

function saveDraft() {
  try {
    if (st.draft && st.base && changes().length) sessionStorage.setItem(DRAFT_KEY, JSON.stringify({ base: stable(st.base), draft: st.draft }));
    else sessionStorage.removeItem(DRAFT_KEY);
  } catch { /* private mode */ }
}

function restoreDraft() {
  if (st.draft) {
    // Edits made earlier this visit stay, as long as they still apply.
    return;
  }
  try {
    const saved = JSON.parse(sessionStorage.getItem(DRAFT_KEY) || 'null');
    if (saved && saved.base === stable(st.base)) st.draft = saved.draft;
  } catch { /* nothing saved */ }
}

/** Every action a policy names, strongest requirement first. @param {Policy} p @returns {Action[]} */
function actions(p) {
  const names = new Set([...Object.keys(p.permissions || {}), ...(p.floor || [])]);
  const lv = levels(p.bands);
  return [...names].map((name) => ({
    name,
    label: (p.labels || {})[name] || fromName(name),
    level: (p.floor || []).includes(name) ? ANYONE : p.permissions[name],
    roles: Object.keys(p.roles || {}).filter((r) => (p.roles || {})[r].includes(name)).sort(),
    agents: Object.keys(p.agents || {}).filter((a) => p.agents[a].includes(name) || p.agents[a].includes('*')).sort(),
  })).sort((a, b) => level(lv, b.level).rank - level(lv, a.level).rank || a.label.localeCompare(b.label));
}

/** @param {Policy} p @param {string} name */
const actionOf = (p, name) => actions(p).find((a) => a.name === name);
const roleNames = (/** @type {Policy} */ p) => Object.keys(p.roles || {}).sort((a, b) => a.localeCompare(b));
const agentNames = (/** @type {Policy} */ p) => Object.keys(p.agents || {}).sort((a, b) => a.localeCompare(b));
const personName = (/** @type {Person} */ x) => x.displayName || 'Someone unnamed';

/** People who hold a role. @param {string} role */
const holders = (role) => st.people.filter((x) => x.roles.includes(role));

/** Whether a person holds an action through some role in p. @param {Policy} p @param {Person} x @param {string} name */
const holds = (p, x, name) => x.roles.some((r) => ((p.roles || {})[r] || []).includes(name));

/** The assistants a person has allowed to do an action. @param {Person} x @param {string} name */
function allowedBy(x, name) {
  const now = Date.now();
  return [...new Set(st.delegations.filter((d) => d.identityId === x.id && !d.revokedAt && !(d.expiresAt && Date.parse(d.expiresAt) <= now) &&
    (d.permissions.includes(name) || d.permissions.includes('*'))).map((d) => d.agent))];
}

/* ---------- Edits ---------- */

/** @param {(p: Policy) => void} fn */
function edit(fn) {
  if (!st.base) return;
  if (!st.draft) st.draft = clone(st.base);
  fn(st.draft);
  st.draft.floor ||= []; st.draft.roles ||= {}; st.draft.labels ||= {};
  st.applied = '';
  st.applyError = '';
  if (!changes().length) st.draft = null;
  saveDraft();
  nav();
}

/** @param {Policy} p @param {string} name @param {string} key */
function setLevel(p, name, key) {
  p.floor = (p.floor || []).filter((x) => x !== name);
  delete p.permissions[name];
  if (key === ANYONE) p.floor.push(name);
  else p.permissions[name] = key;
}

/** @param {Policy} p @param {string} name @param {string} role */
function toggleRole(p, name, role) {
  const l = (p.roles ||= {})[role] || [];
  p.roles[role] = l.includes(name) ? l.filter((x) => x !== name) : [...l, name];
}

/** @param {Policy} p @param {string} name @param {string} agent */
function toggleAgent(p, name, agent) {
  let l = p.agents[agent] || [];
  if (l.includes('*')) l = actions(p).map((a) => a.name).filter((x) => x !== name);
  else l = l.includes(name) ? l.filter((x) => x !== name) : [...l, name];
  p.agents[agent] = l;
}

/** @param {Policy} p @param {string} name */
function removeAction(p, name) {
  delete p.permissions[name];
  p.floor = (p.floor || []).filter((x) => x !== name);
  if (p.labels) delete p.labels[name];
  for (const r of Object.keys(p.roles || {})) /** @type {any} */ (p.roles)[r] = /** @type {any} */ (p.roles)[r].filter((/** @type {string} */ x) => x !== name);
  for (const a of Object.keys(p.agents)) if (!p.agents[a].includes('*')) p.agents[a] = p.agents[a].filter((x) => x !== name);
}

/** Puts a removed action back as it was. @param {Policy} p @param {string} name */
function keepAction(p, name) {
  const was = /** @type {Action} */ (actionOf(/** @type {Policy} */ (st.base), name));
  setLevel(p, name, was.level);
  const lbl = /** @type {Policy} */ (st.base).labels?.[name];
  if (lbl) (p.labels ||= {})[name] = lbl;
  for (const r of was.roles) if (p.roles?.[r] && !p.roles[r].includes(name)) p.roles[r].push(name);
  for (const a of was.agents) if (p.agents[a] && !p.agents[a].includes('*') && !p.agents[a].includes(name)) p.agents[a].push(name);
}

/* ---------- Consequences ---------- */

/** Every staged change, each restated as what it does to people. @returns {Change[]} */
function changes() {
  if (!st.draft || !st.base) return [];
  const b = st.base, d = st.draft;
  const lvB = levels(b.bands), lvD = levels(d.bands);
  /** @type {Change[]} */
  const out = [];
  const names = (/** @type {Person[]} */ xs) => list(xs.map(personName));

  for (const r of roleNames(d)) if (!(r in (b.roles || {}))) {
    out.push({ warn: false, text: html`<b>New role ${r}.</b> People who have it at your identity provider get it here the next time they sign in at /enroll.` });
  }
  for (const r of roleNames(b)) if (!(r in (d.roles || {}))) {
    const hs = holders(r);
    out.push({ warn: hs.length > 0, text: html`<b>Remove the role ${r}.</b> ${hs.length ? html`${names(hs)} ${hs.length === 1 ? 'loses' : 'lose'} it, and everything it allowed them.` : 'Nobody holds it now.'}` });
  }
  for (const a of agentNames(d)) if (!(a in b.agents)) {
    out.push({ warn: false, text: html`<b>New assistant ${a}.</b> attest knows it by the client ID it signs in with.` });
  }
  for (const a of agentNames(b)) if (!(a in d.agents)) {
    out.push({ warn: true, text: html`<b>Remove the assistant ${a}.</b> Everything it asks for is refused from now on.` });
  } else if (b.agents[a].includes('*') && !d.agents[a].includes('*')) {
    const only = d.agents[a].map((n) => actionOf(d, n)?.label || n);
    out.push({ warn: true, text: html`<b>${a}</b> could ask for anything, including actions added later. From now on, only: ${list(only, 'nothing')}.` });
  }

  const before = new Map(actions(b).map((a) => [a.name, a]));
  const after = new Map(actions(d).map((a) => [a.name, a]));
  for (const [name, a] of after) {
    const o = before.get(name);
    if (!o) {
      out.push({ warn: false, action: name, text: html`<b>New action ${a.label}</b> (<code>${name}</code>), needs ${level(lvD, a.level).name}. ${a.level === ANYONE ? 'Anyone may have it done by an assistant allowed to ask.' : 'Nobody can have it done until a role includes it, an assistant may ask, and people allow it.'}` });
      continue;
    }
    const lead = html`<b>${a.label}</b>`;
    if (o.label !== a.label) out.push({ warn: false, action: name, text: html`<b>${o.label}</b> is now called “${a.label}”.` });
    if (o.level !== a.level) {
      const was = level(lvB, o.level), now = level(lvD, a.level);
      let why;
      if (a.level === ANYONE) why = 'Anyone, recognized or not, may have it done by an assistant allowed to ask. Roles and what people allowed no longer apply.';
      else if (o.level === ANYONE) why = 'From now on only people who hold a role with it, and have allowed an assistant to, can have it done.';
      else if (now.rank > was.rank) why = `${now.desc}. Stricter: assistants are refused more often when attest is less sure.`;
      else why = `${now.desc}. Looser: attest acts on less certainty.`;
      out.push({ warn: a.level === ANYONE || now.rank < was.rank, action: name, text: html`${lead} needs ${now.name}, was ${was.name}. ${why}` });
    }
    if (a.level !== ANYONE) {
      for (const r of a.roles.filter((r) => !o.roles.includes(r))) {
        const gain = holders(r).filter((x) => !holds(b, x, name));
        out.push({ warn: false, action: name, text: html`${lead}: the role ${r} now includes it. ${gain.length ? html`${names(gain)} can have it done once they allow an assistant to.` : holders(r).length ? 'Everyone with that role could already.' : 'Nobody holds that role yet.'}` });
      }
      for (const r of o.roles.filter((r) => !a.roles.includes(r) && r in (d.roles || {}))) {
        const lose = holders(r).filter((x) => !holds(d, x, name));
        const had = lose.filter((x) => allowedBy(x, name).length);
        out.push({ warn: lose.length > 0, action: name, text: html`${lead}: the role ${r} no longer includes it. ${lose.length ? html`${names(lose)} ${lose.length === 1 ? 'loses' : 'lose'} it.${had.length ? html` ${names(had)} had allowed an assistant to do it; that stops working.` : ''}` : 'Nobody loses it.'}` });
      }
    }
    for (const ag of a.agents.filter((x) => !o.agents.includes(x))) {
      out.push({ warn: false, action: name, text: html`${lead}: <b>${ag}</b> may now ask for it.${a.level === ANYONE ? '' : ' It still acts only for people who allowed it.'}` });
    }
    for (const ag of o.agents.filter((x) => !a.agents.includes(x) && x in d.agents && !(b.agents[x].includes('*') && !d.agents[x].includes('*')))) {
      const had = st.people.filter((x) => allowedBy(x, name).includes(ag));
      out.push({ warn: had.length > 0, action: name, text: html`${lead}: <b>${ag}</b> can no longer ask for it.${had.length ? html` ${names(had)} had allowed it.` : ''}` });
    }
  }
  for (const [name, o] of before) if (!after.has(name)) {
    const had = st.people.filter((x) => allowedBy(x, name).length);
    out.push({ warn: true, action: name, text: html`<b>Remove ${o.label}</b> (<code>${name}</code>). Assistants that ask for it are refused.${had.length ? html` ${names(had)} had allowed it.` : ''}` });
  }
  return out;
}

/* ---------- Drawing ---------- */

function draw() {
  if (!st.main || st.main.dataset.shown !== 'policy') return;
  keepFocus(() => put(/** @type {HTMLElement} */ (st.main), view()));
  wire(/** @type {HTMLElement} */ (st.main));
}

function view() {
  const lede = 'What assistants may do for whom, and how sure attest must be first. Roles come from your identity provider; this is where the rules change.';
  const back = { href: href('policy'), label: 'Policy' };
  if (st.error) return page({ title: 'Policy', lede, master: loadFailed(`Couldn’t load the policy. ${st.error}`), detail: '', pushed: false, back });
  if (!st.base) return page({ title: 'Policy', lede, master: loading(), detail: '', pushed: false, back });

  const p = cur();
  const acts = actions(p);
  const gone = actions(st.base).filter((a) => !acts.some((x) => x.name === a.name));
  if (!st.sel && !narrow() && acts.length) st.sel = acts[0].name;
  const ch = changes();

  const action = html`${ch.length ? html`<button type="button" class="btn quiet" data-discard>Discard</button>` : ''}<button type="button" class="btn primary" data-apply ${ch.length && !st.busy ? '' : raw('disabled')} ${st.busy ? raw('aria-busy="true"') : ''}>${ch.length ? `Apply ${count(ch.length, 'change', 'changes')}` : 'Apply changes'}</button>`;
  const extra = html`<div class="impact${ch.length || st.applyError || st.applied ? '' : ' idle'}" aria-live="polite">
    ${st.applyError ? html`<div class="err" role="alert">${mark('no')}<span>${st.applyError}</span></div>` : ''}
    ${ch.length ? ch.map((c) => html`<div class="chg${c.warn ? ' warn' : ''}"><span class="tag">Unapplied</span><span>${c.text}</span></div>`)
      : st.applied ? html`<div class="chg"><span class="tag">${mark('ok')}</span><span>${st.applied}</span></div>`
        : html`<span>Choose a level, or a dot, to change who may have something done. Nothing changes until you apply it.</span>`}
  </div>`;

  const master = html`${matrix(p, acts, gone)}${rolesGroup(p)}${agentsGroup(p)}${levelsGroup(p)}`;
  const selected = acts.find((a) => a.name === st.sel) || gone.find((a) => a.name === st.sel);
  const detail = selected ? actionDetail(p, selected, !acts.includes(/** @type {Action} */ (selected)))
    : html`<p class="empty-detail">${acts.length ? 'Select an action to see who can have it done.' : 'Add an action to start.'}</p>`;
  return page({ title: 'Policy', lede, action, extra, master, detail, pushed: st.pushed && !!selected, back });
}

/** @param {Policy} p @param {Action[]} acts @param {Action[]} gone */
function matrix(p, acts, gone) {
  const roles = roleNames(p);
  const lv = levels(p.bands);
  const base = /** @type {Policy} */ (st.base);
  const was = (/** @type {string} */ n) => actionOf(base, n);
  const head = html`<thead><tr><th scope="col">Action</th><th scope="col" class="needh">Needs</th>${roles.map((r) => {
    const hs = holders(r).map(personName);
    return html`<th scope="col" class="role" title="${hs.length ? list(hs) : 'Nobody holds it yet'}"><b>${r}</b><span>${hs.length ? hs.join(', ') : 'Nobody yet'}</span></th>`;
  })}<th scope="col" class="wh">Assistants that may ask</th></tr></thead>`;
  const cols = roles.length + 3;
  const row = (/** @type {Action} */ a, removed = false) => {
    const o = was(a.name);
    const l = level(lv, a.level);
    const selected = st.sel === a.name;
    const changedLevel = o && o.level !== a.level;
    const needCell = removed ? html`<td class="needc">${tag(l)}</td>`
      : html`<td class="needc"><button type="button" class="need${changedLevel ? ' changed' : ''}" data-need="${a.name}" aria-haspopup="listbox" aria-expanded="false" aria-label="${a.label} needs ${l.name}. Change">${ladder(l.glyph)}<span>${l.name}</span>${chevron}</button></td>`;
    const cells = a.level === ANYONE
      ? html`<td class="floor" colspan="${Math.max(1, roles.length)}">Anyone, recognized or not</td>`
      : roles.length ? roles.map((r, i) => {
        const on = a.roles.includes(r), before = !!o && o.roles.includes(r);
        return html`<td class="cell${i === 0 ? ' first' : ''}${i === roles.length - 1 ? ' last' : ''}"><button type="button" class="hold${o && on !== before ? ' changed' : ''}" data-hold="${a.name}" data-role="${r}" aria-pressed="${on}" aria-label="${r} ${on ? 'holds' : 'doesn’t hold'}: ${a.label}" ${removed ? raw('disabled') : ''}><svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><circle cx="6" cy="6" r="4.5" fill="none" stroke-width="1.3"/></svg><span class="lbl">${r}</span></button></td>`;
      }) : html`<td class="floor">No roles yet</td>`;
    const agentsChanged = o && o.agents.join() !== a.agents.join();
    return html`<tr class="${selected ? 'cur' : ''}${removed ? ' gone' : ''}"><td class="act"><a href="${href('policy', a.name)}" ${selected ? raw('aria-current="true"') : ''}><b>${a.label}</b><code>${a.name}</code></a></td>${needCell}${cells}<td class="who"><span class="${agentsChanged ? 'changed' : ''}">${a.agents.length ? a.agents.join(', ') : html`<span class="dim">None</span>`}</span></td></tr>`;
  };
  const recognized = acts.filter((a) => a.level !== ANYONE);
  const anyone = acts.filter((a) => a.level === ANYONE);
  const addForm = st.adding === 'action' ? addActionForm(p) : html`<div class="addrow"><button type="button" class="btn" data-add="action">Add an action</button></div>`;
  if (!acts.length && !gone.length) {
    return html`<div class="group"><div class="ghead"><h2>Actions</h2></div>
      <p class="note lead">No actions yet. An action is something an assistant asks to do for someone, like “Unlock the front door”. Add one, then choose which roles hold it and which assistants may ask.</p>${addForm}</div>`;
  }
  return html`<div class="mx-wrap"><table class="mx" aria-label="Who may have each action done for them">${head}<tbody>
    ${recognized.length ? html`<tr class="sec-row"><td colspan="${cols}"><h3>Needs someone recognized</h3></td></tr>${recognized.map((a) => row(a))}` : ''}
    ${anyone.length ? html`<tr class="sec-row"><td colspan="${cols}"><h3>Anyone may</h3></td></tr>${anyone.map((a) => row(a))}` : ''}
    ${gone.length ? html`<tr class="sec-row"><td colspan="${cols}"><h3>Removing</h3></td></tr>${gone.map((a) => row(a, true))}` : ''}
  </tbody></table></div>${addForm}`;
}

/** @param {Policy} p */
function addActionForm(p) {
  const lv = levels(p.bands);
  const a = st.add;
  return html`<form class="addform compact" data-form="action" novalidate aria-label="Add an action">
    <div class="fields-3">
      <label class="field">What people see<input data-a="label" value="${a.label}" placeholder="Unlock the front door" autocomplete="off"></label>
      <label class="field">Name assistants use<input class="mono-in" data-a="name" value="${a.name}" placeholder="door.unlock" autocomplete="off" spellcheck="false"></label>
      <label class="field">Needs<select data-a="level">${lv.map((l) => html`<option value="${l.key}" ${l.key === (a.level ?? lv[Math.min(1, lv.length - 1)].key) ? raw('selected') : ''}>${l.name}</option>`)}</select></label>
    </div>
    ${a.error ? problem(a.error) : ''}
    <div class="acts"><button type="button" class="btn quiet" data-add-cancel>Cancel</button><button type="submit" class="btn">Add action</button></div>
  </form>`;
}

/** @param {Policy} p */
function rolesGroup(p) {
  const all = [...new Set([...roleNames(/** @type {Policy} */ (st.base)), ...roleNames(p)])].sort((a, b) => a.localeCompare(b));
  const rows = all.map((r) => {
    const removed = !(r in (p.roles || {}));
    const added = !(r in (/** @type {Policy} */ (st.base).roles || {}));
    const hs = holders(r).map(personName);
    const n = ((p.roles || {})[r] || []).length;
    return html`<div class="row r-role"><span class="name">${removed ? html`<s>${r}</s>` : r}${added ? html` <span class="dim small">· new</span>` : ''}</span><span class="sub small ellipsis">${hs.length ? hs.join(', ') : 'Nobody holds it yet'}</span><span class="end small">${removed
      ? html`<button type="button" class="linkbtn" data-keep-role="${r}">Keep</button>`
      : html`<span class="dim">${count(n, 'action', 'actions')}</span> · <button type="button" class="linkbtn" data-remove-role="${r}" aria-label="Remove the role ${r}">Remove</button>`}</span></div>`;
  });
  return html`<div class="group"><div class="ghead"><h2>Roles</h2><span class="meta">Names must match roles in your identity provider</span><span class="meta end">${st.adding === 'role' ? '' : html`<button type="button" class="linkbtn" data-add="role">Add a role</button>`}</span></div>
    ${st.adding === 'role' ? html`<form class="addform compact" data-form="role" novalidate><div class="inline-add"><label class="field">Role name<input data-a="role" value="${st.add.role}" autocomplete="off" spellcheck="false" placeholder="resident"></label><button type="button" class="btn quiet" data-add-cancel>Cancel</button><button type="submit" class="btn">Add</button></div>${st.add.error ? problem(st.add.error) : ''}</form>` : ''}
    ${rows.length ? rows : html`<p class="note">No roles yet. People get a role here when their account at your identity provider has a role of the same name, the next time they sign in at /enroll.</p>`}</div>`;
}

/** @param {Policy} p */
function agentsGroup(p) {
  const all = [...new Set([...agentNames(/** @type {Policy} */ (st.base)), ...agentNames(p)])].sort((a, b) => a.localeCompare(b));
  const rows = all.map((a) => {
    const removed = !(a in p.agents);
    const added = !(a in /** @type {Policy} */ (st.base).agents);
    const l = p.agents[a] || [];
    return html`<div class="row r-two"><span class="name">${removed ? html`<s>${a}</s>` : a}${added ? html` <span class="dim small">· new</span>` : ''}</span><span class="end small">${removed
      ? html`<button type="button" class="linkbtn" data-keep-agent="${a}">Keep</button>`
      : html`<span class="dim">${l.includes('*') ? 'May ask for anything' : `May ask for ${count(l.length, 'action', 'actions')}`}</span> · <button type="button" class="linkbtn" data-remove-agent="${a}" aria-label="Remove the assistant ${a}">Remove</button>`}</span></div>`;
  });
  return html`<div class="group"><div class="ghead"><h2>Assistants</h2><span class="meta">Known by the client ID they sign in with</span><span class="meta end">${st.adding === 'agent' ? '' : html`<button type="button" class="linkbtn" data-add="agent">Add an assistant</button>`}</span></div>
    ${st.adding === 'agent' ? html`<form class="addform compact" data-form="agent" novalidate><div class="inline-add"><label class="field">Client ID<input class="mono-in" data-a="agent" value="${st.add.agent}" autocomplete="off" spellcheck="false" placeholder="home-assistant"></label><button type="button" class="btn quiet" data-add-cancel>Cancel</button><button type="submit" class="btn">Add</button></div>${st.add.error ? problem(st.add.error) : ''}</form>` : ''}
    ${rows.length ? rows : html`<p class="note">No assistants yet. An assistant asks attest before it acts for someone; add it here by the client ID it signs in with.</p>`}</div>`;
}

/** @param {Policy} p */
function levelsGroup(p) {
  return html`<div class="group"><div class="ghead"><h2>Levels</h2><span class="meta">How sure attest must be that it’s the right person</span></div>
    <div class="levels">${levels(p.bands).map((l) => html`${tag(l)}<span class="sub">${l.desc}</span>`)}</div></div>`;
}

/** @param {Policy} p @param {Action} a @param {boolean} removed */
function actionDetail(p, a, removed) {
  const l = level(levels(p.bands), a.level);
  const mine = changes().filter((c) => c.action === a.name);
  const head = html`<div class="dh"><div><div class="kicker"><code>${a.name}</code></div><h2>${a.label}</h2></div>${tag(l)}</div>
    <p class="sub">${a.level === ANYONE ? 'Anyone may have this done, even when attest recognizes nobody.' : html`Needs ${l.name}: ${l.desc.charAt(0).toLowerCase() + l.desc.slice(1)}.`}</p>
    ${mine.length ? html`<div class="consequence plain">${mine.map((c) => html`<p>${c.text}</p>`)}</div>` : ''}`;
  if (removed) {
    return html`${head}<p class="consequence">This action will be removed when you apply.</p><div class="acts"><button type="button" class="btn" data-keep-action="${a.name}">Keep this action</button></div>`;
  }
  const people = st.people.map((x) => {
    const role = holds(p, x, a.name);
    const by = allowedBy(x, a.name).filter((ag) => a.agents.includes(ag));
    const ok = role && by.length > 0;
    const why = !role ? 'No role that includes it' : !by.length ? 'Hasn’t allowed an assistant to' : `Allowed ${list(by)}`;
    return html`<li>${mark(ok ? 'ok' : 'skip')}<span>${personName(x)}</span><span class="rt">${why}</span></li>`;
  });
  const who = a.level === ANYONE ? '' : html`<div class="sec first"><h3>Who can have this done</h3>
    ${st.people.length ? html`<ul class="lines">${people}</ul><p class="small dim">A person needs a role that includes it <em>and</em> to have allowed an assistant to do it, and attest must be sure enough it’s them at the time.</p>`
      : html`<p class="note">Nobody has enrolled yet. People appear in <a class="x" href="${href('identities')}">Identities</a> after they sign in at /enroll.</p>`}</div>`;
  const ags = agentNames(p);
  const assistants = html`<div class="sec${a.level === ANYONE ? ' first' : ''}"><h3>Assistants that may ask</h3>
    ${ags.length ? html`<ul class="checks">${ags.map((ag) => {
      const any = p.agents[ag].includes('*');
      const on = any || p.agents[ag].includes(a.name);
      return html`<li><label><input type="checkbox" data-agent="${ag}" data-for="${a.name}" ${on ? raw('checked') : ''}><span>${ag}</span><span class="rt">${any ? 'May ask for anything' : ''}</span></label></li>`;
    })}</ul>` : html`<p class="note">No assistants yet. Add one under Assistants.</p>`}</div>`;
  const naming = html`<div class="sec"><h3>Name</h3><form class="compact" data-form="label" novalidate>
    <label class="field">What people see<input data-label="${a.name}" value="${a.label}" autocomplete="off"></label>
    <p class="small dim">Assistants ask for it as <code>${a.name}</code>. People see this name when they choose what assistants may do for them.</p></form></div>`;
  return html`${head}${who}${assistants}${naming}<div class="sec"><button type="button" class="btn danger" data-remove-action="${a.name}">Remove this action</button></div>`;
}

/* ---------- Level menu ---------- */

/** @type {{ el: HTMLElement, btn: HTMLElement, name: string } | null} */
let menu = null;

function closeMenu(focus = true) {
  if (!menu) return;
  const { btn, el } = menu;
  btn.setAttribute('aria-expanded', 'false');
  el.remove();
  menu = null;
  if (focus) btn.focus();
}

/** @param {HTMLElement} btn */
function openMenu(btn) {
  const name = /** @type {string} */ (btn.dataset.need);
  const p = cur();
  const a = actionOf(p, name);
  if (!a) return;
  const lv = levels(p.bands);
  const el = document.createElement('div');
  el.className = 'menu';
  el.setAttribute('role', 'listbox');
  el.setAttribute('aria-label', `${a.label} needs`);
  put(el, html`${lv.map((l, i) => html`<div role="option" id="lvl-${i}" tabindex="-1" data-l="${l.key}" aria-selected="${a.level === l.key}">${ladder(l.glyph)}<b>${l.name}</b><small>${l.desc}</small></div>`)}`);
  document.body.appendChild(el);
  const r = btn.getBoundingClientRect();
  el.style.left = `${Math.min(r.left + scrollX, innerWidth - 332)}px`;
  el.style.top = `${r.bottom + scrollY + 4}px`;
  btn.setAttribute('aria-expanded', 'true');
  menu = { el, btn, name };
  const opts = /** @type {HTMLElement[]} */ ([...el.querySelectorAll('[role=option]')]);
  let i = Math.max(0, lv.findIndex((l) => l.key === a.level));
  const hi = () => { opts.forEach((o, k) => o.classList.toggle('hi', k === i)); opts[i].focus(); };
  hi();
  el.onkeydown = (e) => {
    if (e.key === 'ArrowDown') { i = Math.min(opts.length - 1, i + 1); hi(); e.preventDefault(); }
    else if (e.key === 'ArrowUp') { i = Math.max(0, i - 1); hi(); e.preventDefault(); }
    else if (e.key === 'Home') { i = 0; hi(); e.preventDefault(); }
    else if (e.key === 'End') { i = opts.length - 1; hi(); e.preventDefault(); }
    else if (e.key === 'Enter' || e.key === ' ') { pick(/** @type {string} */ (opts[i].dataset.l)); e.preventDefault(); }
    else if (e.key === 'Escape' || e.key === 'Tab') { closeMenu(); e.preventDefault(); }
  };
  el.onclick = (e) => {
    const o = /** @type {HTMLElement|null} */ (/** @type {Element} */ (e.target).closest('[role=option]'));
    if (o) pick(/** @type {string} */ (o.dataset.l));
  };
}

/** @param {string} key */
function pick(key) {
  if (!menu) return;
  const name = menu.name;
  closeMenu(false);
  edit((p) => setLevel(p, name, key));
  draw();
  /** @type {HTMLElement|null} */ (st.main?.querySelector(`[data-need="${CSS.escape(name)}"]`))?.focus();
}

document.addEventListener('mousedown', (e) => {
  if (menu && !/** @type {Element} */ (e.target).closest('.menu, [data-need]')) closeMenu(false);
});

/* ---------- Events ---------- */

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._:\-]*$/;

/** @param {HTMLElement} main */
function wire(main) {
  main.onclick = async (e) => {
    const t = /** @type {HTMLElement} */ (e.target);
    if (t.closest('[data-retry]')) { load(); return; }
    const need = /** @type {HTMLElement|null} */ (t.closest('[data-need]'));
    if (need) { if (menu && menu.btn === need) closeMenu(); else { closeMenu(false); openMenu(need); } return; }
    const hold = /** @type {HTMLElement|null} */ (t.closest('[data-hold]'));
    if (hold) { edit((p) => toggleRole(p, /** @type {string} */ (hold.dataset.hold), /** @type {string} */ (hold.dataset.role))); draw(); return; }
    if (t.closest('[data-discard]')) { st.draft = null; st.applyError = ''; saveDraft(); nav(); draw(); announce('Changes discarded.'); /** @type {HTMLElement|null} */ (main.querySelector('#h1'))?.focus(); return; }
    if (t.closest('[data-apply]')) { await apply(); return; }
    const add = t.closest('[data-add]');
    if (add) {
      st.adding = /** @type {any} */ (add.getAttribute('data-add'));
      st.add = { label: '', name: '', level: null, role: '', agent: '', error: '' };
      draw();
      /** @type {HTMLElement|null} */ (main.querySelector('.addform input'))?.focus();
      return;
    }
    if (t.closest('[data-add-cancel]')) { const was = st.adding; st.adding = null; draw(); /** @type {HTMLElement|null} */ (main.querySelector(`[data-add="${was}"]`))?.focus(); return; }
    const rr = t.closest('[data-remove-role]');
    if (rr) { const r = /** @type {string} */ (rr.getAttribute('data-remove-role')); edit((p) => { delete p.roles?.[r]; }); draw(); return; }
    const kr = t.closest('[data-keep-role]');
    if (kr) { const r = /** @type {string} */ (kr.getAttribute('data-keep-role')); edit((p) => { (p.roles ||= {})[r] = [.../** @type {any} */ (st.base).roles[r]].filter((n) => actionOf(p, n)); }); draw(); return; }
    const ra = t.closest('[data-remove-agent]');
    if (ra) { const a = /** @type {string} */ (ra.getAttribute('data-remove-agent')); edit((p) => { delete p.agents[a]; }); draw(); return; }
    const ka = t.closest('[data-keep-agent]');
    if (ka) { const a = /** @type {string} */ (ka.getAttribute('data-keep-agent')); edit((p) => { p.agents[a] = [.../** @type {Policy} */ (st.base).agents[a]].filter((n) => n === '*' || actionOf(p, n)); }); draw(); return; }
    const rm = t.closest('[data-remove-action]');
    if (rm) { const n = /** @type {string} */ (rm.getAttribute('data-remove-action')); edit((p) => removeAction(p, n)); draw(); /** @type {HTMLElement|null} */ (main.querySelector('[data-keep-action]'))?.focus(); return; }
    const keep = t.closest('[data-keep-action]');
    if (keep) { const n = /** @type {string} */ (keep.getAttribute('data-keep-action')); edit((p) => keepAction(p, n)); draw(); /** @type {HTMLElement|null} */ (main.querySelector('[data-remove-action]'))?.focus(); }
  };
  main.onchange = (e) => {
    const t = /** @type {HTMLInputElement} */ (e.target);
    if (t.dataset.agent) { edit((p) => toggleAgent(p, /** @type {string} */ (t.dataset.for), /** @type {string} */ (t.dataset.agent))); draw(); return; }
    if (t.dataset.label) { relabel(t); return; }
    if (t.dataset.a) { /** @type {any} */ (st.add)[t.dataset.a] = t.value; }
  };
  main.oninput = (e) => {
    const t = /** @type {HTMLInputElement} */ (e.target);
    if (!t.dataset.a) return;
    /** @type {any} */ (st.add)[t.dataset.a] = t.value;
    if (t.dataset.a === 'label') {
      // Suggest a name from the words until someone types their own.
      const nameIn = /** @type {HTMLInputElement|null} */ (main.querySelector('[data-a="name"]'));
      if (nameIn && (!nameIn.dataset.touched)) {
        nameIn.value = st.add.name = t.value.trim().toLowerCase().replace(/[^a-z0-9]+/g, '.').replace(/^\.+|\.+$/g, '');
      }
    }
    if (t.dataset.a === 'name') t.dataset.touched = '1';
  };
  main.onsubmit = (e) => {
    e.preventDefault();
    const which = /** @type {HTMLFormElement} */ (e.target).dataset.form;
    if (which === 'label') { const i = /** @type {HTMLInputElement|null} */ (main.querySelector('[data-label]')); if (i) relabel(i); return; }
    submitAdd(/** @type {string} */ (which));
  };
}

/** @param {HTMLInputElement} input */
function relabel(input) {
  const name = /** @type {string} */ (input.dataset.label);
  const text = input.value.trim();
  edit((p) => {
    p.labels ||= {};
    if (!text || text === fromName(name)) delete p.labels[name];
    else p.labels[name] = text;
  });
  draw();
}

/** @param {string} which */
function submitAdd(which) {
  const p = cur();
  const a = st.add;
  a.error = '';
  if (which === 'action') {
    const name = a.name.trim();
    const label = a.label.trim();
    const lv = levels(p.bands);
    const key = a.level ?? lv[Math.min(1, lv.length - 1)].key;
    if (!label) a.error = 'Say what it does, in the words people will see.';
    else if (!NAME_RE.test(name)) a.error = 'The name assistants use: letters, digits, dots, dashes, no spaces. For example door.unlock.';
    else if (actionOf(p, name)) a.error = `There is already an action called ${name}.`;
    if (a.error) { draw(); return; }
    edit((d) => { setLevel(d, name, key); if (label !== fromName(name)) (d.labels ||= {})[name] = label; });
    st.adding = null;
    navigate(href('policy', name));
    announce(`Added ${label}. Not applied yet.`);
    return;
  }
  if (which === 'role') {
    const role = a.role.trim();
    if (!role) a.error = 'Enter the role’s name, exactly as your identity provider spells it.';
    else if (role in (p.roles || {})) a.error = `There is already a role called ${role}.`;
    if (a.error) { draw(); return; }
    edit((d) => { (d.roles ||= {})[role] = []; });
    st.adding = null;
    draw();
    announce(`Added the role ${role}. Not applied yet.`);
    return;
  }
  if (which === 'agent') {
    const agent = a.agent.trim();
    if (!NAME_RE.test(agent)) a.error = 'Enter the client ID the assistant signs in with: no spaces.';
    else if (agent in p.agents) a.error = `There is already an assistant called ${agent}.`;
    if (a.error) { draw(); return; }
    edit((d) => { d.agents[agent] = []; });
    st.adding = null;
    draw();
    announce(`Added the assistant ${agent}. Not applied yet.`);
  }
}

async function apply() {
  if (!st.draft || st.busy) return;
  const n = changes().length;
  st.busy = true;
  st.applyError = '';
  draw();
  try {
    const latest = clean(await get('/v1/settings/policy'));
    if (stable(latest) !== stable(st.base)) {
      // Someone applied changes since this page loaded. Keep these edits
      // and show them against what is in force now.
      const mine = st.draft;
      st.base = latest;
      st.draft = mine;
      st.applyError = 'Someone else changed the policy while you were editing. Your changes are still here, now compared with what is in force. Check them, then apply again.';
    } else {
      st.base = clean(await putJSON('/v1/settings/policy', clean(st.draft)));
      st.draft = null;
      st.applied = `Applied ${count(n, 'change', 'changes')}. ${n === 1 ? 'It is' : 'They are'} in force now.`;
      announce(st.applied);
      // Removing a role removes its assignments: refresh who holds what.
      try { st.people = await get('/v1/identities'); } catch { /* the next visit refreshes */ }
    }
  } catch (e) {
    st.applyError = `Nothing was applied. ${plain(e)}`;
  }
  st.busy = false;
  saveDraft();
  nav();
  draw();
}
