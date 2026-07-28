// Settings: the daemon URL, the token, and the host permission for
// whichever origin the URL names.
//
// Requesting the permission here rather than declaring a fixed origin in
// the manifest is what lets the daemon port stay configurable. A pinned
// http://127.0.0.1:7600/* would break silently the moment someone
// changed extension_addr.

const form = document.getElementById("form");
const urlEl = document.getElementById("daemonUrl");
const tokenEl = document.getElementById("token");
const ignoredEl = document.getElementById("ignored");
const statusEl = document.getElementById("status");

function report(kind, text) {
  statusEl.className = `status status--${kind}`;
  statusEl.textContent = text;
}

async function load() {
  const { daemonUrl, token, ignored } = await getSettings();
  urlEl.value = daemonUrl;
  tokenEl.value = token;
  ignoredEl.value = ignored.join("\n");
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();

  const daemonUrl = normalizeDaemonUrl(urlEl.value);
  const token = tokenEl.value.trim();

  let origin;
  try {
    origin = originFor(daemonUrl);
  } catch (err) {
    report("error", "That does not look like a URL.");
    return;
  }

  // Request first, before anything is awaited.
  //
  // permissions.request() must be called while the user gesture is
  // still in scope, and awaiting even a storage write consumes it --
  // the request then resolves false without ever prompting, which
  // looks exactly like the user declining.
  let granted = false;
  try {
    granted = await api.permissions.request({ origins: [origin] });
  } catch (err) {
    granted = false;
  }

  const { patterns, invalid } = parseIgnoreList(ignoredEl.value);
  await api.storage.local.set({ daemonUrl, token, ignored: patterns });

  if (!granted) {
    report("error", `Saved, but Firefox is still blocking ${origin}. Requests will fail until it is allowed.`);
    return;
  }
  if (invalid.length > 0) {
    // Saying so beats storing a rule that can never match.
    report("error", `Saved, but these are not hostnames and were dropped: ${invalid.join(", ")}`);
    return;
  }
  report("ok", "Saved. The daemon should show as connected in the popup.");
});

load();
