// Popup status. Four states, and the fourth is the one that makes a
// fresh install diagnosable: a declined or revoked host permission
// otherwise presents as "daemon unreachable" and sends the user hunting
// a daemon that is running perfectly.

const stateEl = document.getElementById("state");
const detailEl = document.getElementById("detail");
const grantEl = document.getElementById("grant");
const queueEl = document.getElementById("queue");

function show(kind, text, detail) {
  stateEl.className = `state state--${kind}`;
  stateEl.textContent = text;
  detailEl.textContent = detail || "";
  grantEl.hidden = kind !== "permission";
}

async function refresh() {
  const { daemonUrl, token } = await getSettings();

  const queue = (await api.storage.local.get("queue")).queue || [];
  queueEl.textContent = String(queue.length);

  if (!token) {
    show("setup", "No token set", `Run: trackkrd -print-extension-token, then paste it in Settings.`);
    return;
  }

  if (!(await hasHostPermission(daemonUrl))) {
    show("permission", "Permission not granted", `Firefox is blocking requests to ${daemonUrl}.`);
    return;
  }

  try {
    const resp = await fetch(`${daemonUrl}/extension/status`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    if (resp.status === 401) {
      show("error", "Token rejected", "The daemon is running, but this token does not match its config.");
      return;
    }
    if (!resp.ok) {
      show("error", `Daemon returned ${resp.status}`, daemonUrl);
      return;
    }
    show("ok", "Connected", daemonUrl);
  } catch (err) {
    show("error", "Daemon unreachable", `Nothing is listening at ${daemonUrl}.`);
  }
}

grantEl.addEventListener("click", async () => {
  const { daemonUrl } = await getSettings();
  try {
    await api.permissions.request({ origins: [originFor(daemonUrl)] });
  } catch (err) {
    // Declined, or not triggered by a gesture Firefox accepts.
  }
  await refresh();
});

document.getElementById("options").addEventListener("click", (event) => {
  event.preventDefault();
  api.runtime.openOptionsPage();
});

refresh();
