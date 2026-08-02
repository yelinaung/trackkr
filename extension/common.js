// Shared helpers for the background script, popup, and options page.
//
// Firefox exposes the promise-based `browser` namespace; the fallback
// keeps the file usable if it is ever loaded under a chrome-only shim.
globalThis.api = globalThis.browser ?? globalThis.chrome;

// browserKind drives endpoint selection, nothing else. It is derived from which
// namespace exists rather than from the user agent: the two builds are packaged
// separately, and brand sniffing would invite non-Google Chromium browsers into
// a path this phase does not support.
globalThis.browserKind = globalThis.browser ? "firefox" : "chrome";

// The daemon picks the canonical application name from the route, so the route
// is how a build declares which browser it is. An old daemon 404s the Chrome
// path instead of storing Chrome activity as Firefox.
globalThis.ACTIVITY_PATH =
  globalThis.browserKind === "chrome"
    ? "/extension/activity/chrome"
    : "/extension/activity";

// IDLE_PATH is the same either way. The daemon reports when the user
// stopped touching the machine, which has nothing to do with which
// browser is asking.
globalThis.IDLE_PATH = "/extension/idle";

// The pure helpers -- DEFAULT_DAEMON_URL, normalizeDaemonUrl, originFor
// -- live in logic.js, which loads first. What is left here needs the
// browser.

// getSettings returns the configured daemon URL and token.
globalThis.getSettings = async function getSettings() {
  const stored = await api.storage.local.get(["daemonUrl", "token", "ignored"]);
  const daemonUrl = normalizeDaemonUrl(stored.daemonUrl);
  return {
    daemonUrl: isDaemonUrlAllowed(daemonUrl) ? daemonUrl : DEFAULT_DAEMON_URL,
    token: stored.token || "",
    ignored: stored.ignored || [],
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
