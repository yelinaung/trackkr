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
const statusEl = document.getElementById("status");

function report(kind, text) {
  statusEl.className = `status status--${kind}`;
  statusEl.textContent = text;
}

async function load() {
  const { daemonUrl, token } = await getSettings();
  urlEl.value = daemonUrl;
  tokenEl.value = token;
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

  await api.storage.local.set({ daemonUrl, token });

  // The click is the user gesture permissions.request() requires.
  let granted = false;
  try {
    granted = await api.permissions.request({ origins: [origin] });
  } catch (err) {
    granted = false;
  }

  if (!granted) {
    report("error", `Saved, but Firefox is still blocking ${origin}. Requests will fail until it is allowed.`);
    return;
  }
  report("ok", "Saved. The daemon should show as connected in the popup.");
});

load();
