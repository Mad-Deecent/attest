// History routing under /console/. A route is a domain and an optional item:
// /console/policy/door.unlock -> { domain: 'policy', id: 'door.unlock' }.

/** @typedef {{ domain: string, id: string|null, query: URLSearchParams }} Route */

/** @type {(r: Route) => void} */
let listener = () => {};

/** @returns {Route} */
export function current() {
  const rest = location.pathname.replace(/^\/console\/?/, '');
  const [domain = '', ...id] = rest.split('/');
  return {
    domain: decodeURIComponent(domain),
    id: id.length && id.join('/') ? decodeURIComponent(id.join('/')) : null,
    query: new URLSearchParams(location.search),
  };
}

/** /console/<domain>[/<id>] */
export function href(/** @type {string} */ domain, /** @type {string|null|undefined} */ id) {
  return `/console/${encodeURIComponent(domain)}${id ? '/' + encodeURIComponent(id) : ''}`;
}

export function navigate(/** @type {string} */ path, replace = false) {
  if (path === location.pathname + location.search) {
    listener(current());
    return;
  }
  history[replace ? 'replaceState' : 'pushState'](null, '', path);
  listener(current());
}

/** @param {(r: Route) => void} fn */
export function start(fn) {
  listener = fn;
  addEventListener('popstate', () => listener(current()));
  document.addEventListener('click', (e) => {
    const a = /** @type {Element} */ (e.target).closest('a[href]');
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (a.hasAttribute('download') || a.getAttribute('target') || a.hasAttribute('data-full')) return;
    const u = new URL(/** @type {HTMLAnchorElement} */ (a).href, location.href);
    if (u.origin !== location.origin || !u.pathname.startsWith('/console/') || u.pathname === '/console/login') return;
    e.preventDefault();
    navigate(u.pathname + u.search);
  });
}
