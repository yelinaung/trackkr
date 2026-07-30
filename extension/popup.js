// Popup status.
//
// The states are distinct because the fix for each is distinct. A revoked host
// permission otherwise presents as "daemon unreachable" and sends the user
// hunting a daemon that is running perfectly, and under Chrome a Local Network
// Access denial presents the same way again.

// The last settings read, kept so the grant button can call
// permissions.request() without awaiting first: an await consumes the user
// gesture the request needs.
let known = { daemonUrl: DEFAULT_DAEMON_URL, token: "" };

const stateEl = document.getElementById("state");
const detailEl = document.getElementById("detail");
const grantEl = document.getElementById("grant");
const retryEl = document.getElementById("retry");
const queueEl = document.getElementById("queue");

function show(kind, text, detail) {
  stateEl.className = `state state--${kind}`;
  stateEl.textContent = text;
  detailEl.textContent = detail || "";
  grantEl.hidden = kind !== STATUS.PERMISSION;
  if (retryEl) {
    retryEl.hidden = kind !== STATUS.BLOCKED;
  }
}

// probeDaemon issues the authenticated status request from this foreground
// document.
//
// Foreground matters under Chrome: the extension page is what triggers the
// Local Network Access prompt. A service-worker request cannot -- it stays
// blocked until the origin already holds LNA -- so asking the background to
// probe would leave the user with no way to grant it.
async function probeDaemon(daemonUrl, token) {
  try {
    const resp = await fetch(new URL("/extension/status", daemonUrl), {
      headers: { Authorization: `Bearer ${token}` },
    });
    let body = null;
    try {
      body = await resp.json();
    } catch (err) {
      // A 2xx without parseable JSON is an upgrade case, not a crash.
    }
    return { status: resp.status, body };
  } catch (err) {
    // No HTTP response at all: LNA denial, or nothing listening.
    return { status: undefined, body: null };
  }
}

const MESSAGES = {
  [STATUS.NO_TOKEN]: () => [
    "No token set",
    "Run: trackkrd -print-extension-token, then paste it in Settings.",
  ],
  [STATUS.PERMISSION]: (url) => ["Permission not granted", `Requests to ${url} are blocked.`],
  [STATUS.BLOCKED]: (url) => [
    "Local network access blocked or daemon unavailable",
    browserKind === "chrome"
      ? `Check that trackkrd is running at ${url}. If it is, allow local network access for this extension in Chrome site settings, then Retry.`
      : `Nothing is listening at ${url}.`,
  ],
  [STATUS.TOKEN_REJECTED]: () => [
    "Token rejected",
    "The daemon is running, but this token does not match its config.",
  ],
  [STATUS.UPGRADE]: () => [
    "Daemon upgrade required",
    "This daemon does not accept Chrome records. Upgrade trackkrd before using this build, or its activity would be misattributed.",
  ],
  [STATUS.CONNECTED]: (url) => ["Connected", url],
};

async function refresh() {
  const { daemonUrl, token } = await getSettings();
  known = { daemonUrl, token };

  const queue = (await api.storage.local.get("queue")).queue || [];
  queueEl.textContent = String(queue.length);

  // Host permission is tracked separately from network readiness. Only once
  // the permission exists is a failure worth blaming on the network.
  const hasPermission = token ? await hasHostPermission(daemonUrl) : false;
  let probe = { status: undefined, body: null };
  if (token && hasPermission) {
    probe = await probeDaemon(daemonUrl, token);
  }

  const state = classifyStatus({
    hasToken: Boolean(token),
    hasPermission,
    status: probe.status,
    body: probe.body,
    kind: browserKind,
  });

  const message = MESSAGES[state];
  if (message) {
    const [text, detail] = message(daemonUrl);
    show(state, text, detail);
    return;
  }
  show(STATUS.HTTP_ERROR, `Daemon returned ${probe.status}`, daemonUrl);
}

grantEl.addEventListener("click", async () => {
  try {
    // No await before this call: the gesture must still be in scope.
    await api.permissions.request({ origins: [originFor(known.daemonUrl)] });
  } catch (err) {
    // Declined, or not triggered by a gesture the browser accepts.
  }
  // Straight into the foreground probe. Under Chrome this is what raises the
  // Local Network Access prompt, and it has to happen while the user is still
  // here to answer it.
  await refresh();
});

if (retryEl) {
  retryEl.addEventListener("click", () => refresh());
}

document.getElementById("options").addEventListener("click", (event) => {
  event.preventDefault();
  api.runtime.openOptionsPage();
});

refresh();
