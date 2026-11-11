// attest console: decides what this browser should see (setup, sign-in, or
// the console), then hands each domain its screen.

import { get, post, setCSRF, whenSignedOut } from './lib/api.js';
import { html, put } from './lib/html.js';
import { start, current, navigate, href } from './lib/router.js';
import { mount, nav } from './lib/shell.js';
import { setupFlow } from './screens/setup.js';
import { signIn, noAccess } from './screens/signin.js';
import { connections } from './screens/connections.js';
import { policy } from './screens/policy.js';
import { identities } from './screens/identities.js';

/** @type {Record<string, import('./lib/shell.js').Screen>} */
const SCREENS = { identities, policy, connections };
const HOME = 'policy';

const app = /** @type {HTMLElement} */ (document.getElementById('app'));

try {
  const theme = localStorage.getItem('attest.theme');
  if (theme === 'dark' || theme === 'light') document.documentElement.dataset.theme = theme;
} catch { /* private mode */ }

async function boot() {
  let info;
  try {
    info = await get('/v1/console/session');
  } catch (e) {
    put(app, html`<div class="flow"><div class="flow-in"><div class="brand"><b>attest</b></div><div class="flow-head"></div><h1 tabindex="-1">attest isn\u2019t answering</h1><p class="lede">${/** @type {Error} */ (e).message}</p><div class="flow-foot"><span></span><button class="btn primary" type="button" data-reload>Try again</button></div></div></div>`);
    app.querySelector('[data-reload]')?.addEventListener('click', () => location.reload());
    return;
  }
  const route = current();

  if (!info.setupComplete) {
    if (route.domain !== 'setup') history.replaceState(null, '', '/console/setup');
    setupFlow(app);
    return;
  }
  if (route.domain === 'no-access') { noAccess(app, info); return; }
  if (!info.signedIn) {
    const back = route.domain === 'sign-in' ? route.query.get('return') || '/console/' : location.pathname;
    if (route.domain !== 'sign-in') history.replaceState(null, '', '/console/sign-in');
    signIn(app, info, back, route.query.get('problem'), route.query.has('signedout'));
    return;
  }

  setCSRF(info.csrf);
  // A session that ends while the console is open sends you to sign in,
  // and back here afterwards.
  whenSignedOut(() => location.assign(`/console/sign-in?problem=ended&return=${encodeURIComponent(location.pathname)}`));
  let place = '';
  try { place = new URL((await get('/v1/settings/public-url')).publicUrl).host; } catch { place = ''; }
  const main = mount({
    name: info.name, place, screens: SCREENS,
    signOut: async () => {
      try { await post('/v1/console/sign-out'); } catch { /* signed out either way */ }
      setCSRF('');
      location.assign('/console/sign-in?signedout=1');
    },
  });
  start((r) => show(main, r));
  show(main, current());
  // Ask the provider in the background so the rail can flag it.
  connections.check?.().then(() => nav());
  addEventListener('beforeunload', (e) => { if (policy.pending?.()) e.preventDefault(); });
}

/** @param {HTMLElement} main @param {import('./lib/router.js').Route} r */
function show(main, r) {
  const screen = SCREENS[r.domain];
  if (!screen) { navigate(href(HOME), true); return; }
  const fresh = main.dataset.domain !== r.domain;
  main.dataset.domain = r.domain;
  document.title = `${screen === policy ? 'Policy' : screen === connections ? 'Connections' : 'Identities'} · attest`;
  nav(r.domain);
  screen.show(main, r);
  if (fresh) /** @type {HTMLElement|null} */ (main.querySelector('#h1'))?.focus({ preventScroll: true });
}

boot();
