// denma: every admin API request says which center its page was loaded for
// (X-Denma-Center), so that one from a tab left on another center is refused
// rather than served by the center opened since (cmd/denma_place.go).
const center = window._LM_CONFIG?.denma_center ?? '';
const fetchAPI = window.fetch.bind(window);

window.fetch = (input, init = {}) => {
  const url = new URL(input instanceof Request ? input.url : String(input), window.location.href);
  if (url.origin === window.location.origin && /\/api(\/|$)/.test(url.pathname)) {
    const headers = new Headers(init.headers || (input instanceof Request ? input.headers : undefined));
    headers.set('X-Denma-Center', center);
    return fetchAPI(input, { ...init, headers });
  }
  return fetchAPI(input, init);
};
