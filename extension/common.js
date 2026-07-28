// Shared helpers for the background script, popup, and options page.
//
// Firefox exposes the promise-based `browser` namespace; the fallback
// keeps the file usable if it is ever loaded under a chrome-only shim.
globalThis.api = globalThis.browser ?? globalThis.chrome;

// The pure helpers -- DEFAULT_DAEMON_URL, normalizeDaemonUrl, originFor
// -- live in logic.js, which loads first. What is left here needs the
// browser.

// getSettings returns the configured daemon URL and token.
globalThis.getSettings = async function getSettings() {
  const stored = await api.storage.local.get(["daemonUrl", "token"]);
  return {
    daemonUrl: normalizeDaemonUrl(stored.daemonUrl),
    token: stored.token || "",
  };
};

// hasHostPermission reports whether the extension may talk to the
// daemon at all.
//
// This check exists because Firefox treats MV3 host permissions as
// optional: they are not granted at install, and can be revoked later
// in about:addons. Without it, a missing permission surfaces as an
// opaque NetworkError that looks exactly like the daemon being down.
globalThis.hasHostPermission = async function hasHostPermission(daemonUrl) {
  try {
    return await api.permissions.contains({ origins: [originFor(daemonUrl)] });
  } catch (err) {
    return false;
  }
};
