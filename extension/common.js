// Shared helpers for the background script, popup, and options page.
//
// Firefox exposes the promise-based `browser` namespace; the fallback
// keeps the file usable if it is ever loaded under a chrome-only shim.
globalThis.api = globalThis.browser ?? globalThis.chrome;

// DEFAULT_DAEMON_URL matches extension_addr's default in the daemon's
// config. Both are configurable, which is why the host permission is
// requested at runtime for whatever URL is stored rather than pinned in
// the manifest.
globalThis.DEFAULT_DAEMON_URL = "http://127.0.0.1:7600";

// settings returns the configured daemon URL and token.
globalThis.getSettings = async function getSettings() {
  const stored = await api.storage.local.get(["daemonUrl", "token"]);
  return {
    daemonUrl: (stored.daemonUrl || DEFAULT_DAEMON_URL).replace(/\/+$/, ""),
    token: stored.token || "",
  };
};

// originFor turns a daemon URL into the origin pattern a host
// permission is granted against.
globalThis.originFor = function originFor(daemonUrl) {
  const url = new URL(daemonUrl);
  return `${url.protocol}//${url.hostname}/*`;
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
