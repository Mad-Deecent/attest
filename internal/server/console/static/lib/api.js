// The one way the console talks to attest: same origin, JSON, and the
// session's CSRF token on every request that changes something.

let csrf = '';
/** @type {((e: ApiError) => void) | null} */
let onSignedOut = null;

/** @param {string} token */
export function setCSRF(token) { csrf = token || ''; }

/** Called when a signed-in request comes back 401: the session ended. */
export function whenSignedOut(/** @type {(e: ApiError) => void} */ fn) { onSignedOut = fn; }

export class ApiError extends Error {
  /** @param {number} status @param {string} message @param {any} [body] */
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

/**
 * @param {'GET'|'PUT'|'POST'|'DELETE'} method
 * @param {string} path
 * @param {unknown} [body]
 * @returns {Promise<any>}
 */
export async function api(method, path, body) {
  /** @type {Record<string,string>} */
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
  let res;
  try {
    res = await fetch(path, {
      method, headers, credentials: 'same-origin', cache: 'no-store',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "attest didn\u2019t answer. Check that it\u2019s running, then try again.");
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = null; }
  if (!res.ok) {
    const err = new ApiError(res.status, (data && data.error) || `attest answered ${res.status}`, data);
    if (res.status === 401 && csrf && onSignedOut) onSignedOut(err);
    throw err;
  }
  return data;
}

export const get = (/** @type {string} */ p) => api('GET', p);
export const put = (/** @type {string} */ p, /** @type {unknown} */ b) => api('PUT', p, b);
export const post = (/** @type {string} */ p, /** @type {unknown} */ b) => api('POST', p, b);

/**
 * A sentence for a failed request, for people rather than developers.
 * @param {unknown} e
 */
export function plain(e) {
  if (!(e instanceof ApiError)) return 'Something went wrong. Try again.';
  if (e.status === 0) return e.message;
  if (e.status === 403 && /CSRF/.test(e.message)) return 'This page is out of date. Reload it and try again.';
  if (e.status === 401) return 'Your session ended. Sign in again.';
  if (e.status >= 500 && e.status !== 502 && e.status !== 503) return `attest couldn\u2019t do that: ${e.message}.`;
  return sentence(e.message);
}

/** Capitalizes and ends a server message with a period. */
export function sentence(/** @type {string} */ s) {
  if (!s) return '';
  s = s[0].toUpperCase() + s.slice(1);
  return /[.!?)]$/.test(s) ? s : s + '.';
}
