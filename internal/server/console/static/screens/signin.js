// Signing in, and being told plainly when you may not.

import { html, put } from '../lib/html.js';
import { problem } from '../lib/ui.js';

/** Fixed reasons the server may send back; nothing from a URL is shown. */
const PROBLEMS = {
  failed: 'Sign-in didn’t finish. Try again.',
  unavailable: 'Your identity provider isn’t answering right now. Try again in a few minutes.',
  ended: 'Your session ended. Sign in again to carry on.',
  'no-client': 'Sign-in isn’t available: the identity provider is connected without a sign-in client.',
};

/**
 * @param {HTMLElement} app
 * @param {{ signInAvailable: boolean }} info
 * @param {string} back where to return after signing in
 * @param {string|null} reason
 * @param {boolean} signedOut
 */
export function signIn(app, info, back, reason, signedOut) {
  document.title = 'Sign in · attest';
  const why = reason && PROBLEMS[/** @type {keyof PROBLEMS} */ (reason)];
  const available = info.signInAvailable && reason !== 'no-client';
  put(app, html`<div class="flow"><div class="flow-in">
    <div class="brand"><b>attest</b><span>Console</span></div><div class="flow-head"></div>
    <h1 tabindex="-1">${signedOut ? 'You’re signed out' : 'Sign in to manage attest'}</h1>
    <p class="lede">${available
      ? 'You sign in with your account at your identity provider. The console is for administrators.'
      : 'The console can’t send you to your identity provider to sign in.'}</p>
    ${why ? problem(why, reason === 'ended' ? 'warn' : 'no') : ''}
    ${available
      ? html`<div class="flow-foot"><span></span><a class="btn primary" href="/console/login?return=${encodeURIComponent(back)}" data-full>Sign in</a></div>`
      : html`<p class="consequence plain">Connect the identity provider again with a sign-in client, using an administrator token with attest’s API, or restart attest once with <code>-reset-setup</code> to run setup again.</p>`}
    <p class="after">Not an administrator? Enroll, and choose what assistants may do for you, at <a class="x" href="/enroll" data-full>${location.host}/enroll</a>.</p>
  </div></div>`);
  /** @type {HTMLElement|null} */ (app.querySelector('h1'))?.focus();
}

/** @param {HTMLElement} app @param {{ signedIn: boolean }} info */
export function noAccess(app, info) {
  document.title = 'No access · attest';
  put(app, html`<div class="flow"><div class="flow-in">
    <div class="brand"><b>attest</b><span>Console</span></div><div class="flow-head"></div>
    <h1 tabindex="-1">You don’t have access to the console</h1>
    <p class="lede">You signed in, but your account isn’t an attest administrator. If it should be, ask whoever runs your identity provider to give it attest’s administrator role, then sign in again.</p>
    <div class="flow-foot">${info.signedIn ? html`<a class="btn quiet" href="/console/">Back to the console</a>` : html`<a class="btn quiet" href="/console/login" data-full>Sign in again</a>`}<a class="btn primary" href="/enroll" data-full>Go to enrollment</a></div>
    <p class="after">Enrollment is where you record your voice and choose what assistants may do for you.</p>
  </div></div>`);
  /** @type {HTMLElement|null} */ (app.querySelector('h1'))?.focus();
}
