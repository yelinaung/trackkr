// Reports the active tab to the local trackkrd daemon.
//
// Two pieces of state, deliberately in different stores:
//
//   session  the tab currently being timed. A focus left over from a
//            previous browser run is stale and *should* disappear.
//   local    the queue of finalized records not yet delivered. Session
//            storage evaporates on exit, and "daemon unreachable while
//            the browser is quitting" is exactly when a queue earns its
//            keep -- the daemon persists its own queue for the same
//            reason.
//
// This is an MV3 event page, so it is unloaded between events and can
// hold nothing in module scope.

// The decision rules -- trackable, shouldSwitch, buildRecord,
// idleEndedAt, trimQueue -- come from logic.js, which is free of
// browser APIs so node:test can cover them. This file is the plumbing
// around them: storage, events, and delivery.

const CURRENT_KEY = "current";
const QUEUE_KEY = "queue";

api.idle.setDetectionInterval(IDLE_SECONDS);

async function readCurrent() {
  const stored = await api.storage.session.get(CURRENT_KEY);
  return stored[CURRENT_KEY] || null;
}

async function writeCurrent(current) {
  if (current === null) {
    await api.storage.session.remove(CURRENT_KEY);
    return;
  }
  await api.storage.session.set({ [CURRENT_KEY]: current });
}

async function readQueue() {
  const stored = await api.storage.local.get(QUEUE_KEY);
  return stored[QUEUE_KEY] || [];
}

async function writeQueue(queue) {
  await api.storage.local.set({ [QUEUE_KEY]: trimQueue(queue) });
}

// startTracking begins timing a tab, replacing whatever was current.
async function startTracking(tab, startedAt = Date.now()) {
  if (!trackable(tab)) {
    await writeCurrent(null);
    return;
  }
  await writeCurrent({
    tabId: tab.id,
    windowId: tab.windowId,
    url: tab.url,
    title: tab.title || "",
    startedAt,
  });
}

// finalize closes the current segment and queues it. endedAt is a
// parameter because idle transitions end the segment when the user
// stopped, not when the browser noticed.
async function finalize(endedAt = Date.now()) {
  const current = await readCurrent();
  await writeCurrent(null);

  const record = buildRecord(current, endedAt);
  if (record === null) {
    return;
  }

  const queue = await readQueue();
  queue.push(record);
  await writeQueue(queue);
}

// deliver sends the queue as one batch, mirroring the daemon's own
// ingest shape so draining a backlog costs a single request. The queue
// is only cleared once the daemon has accepted it.
async function deliver() {
  const queue = await readQueue();
  if (queue.length === 0) {
    return true;
  }

  const { daemonUrl, token } = await getSettings();
  if (!token || !(await hasHostPermission(daemonUrl))) {
    return false;
  }

  try {
    const resp = await fetch(`${daemonUrl}/extension/activity`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify({ records: queue }),
    });
    if (!resp.ok) {
      return false;
    }
  } catch (err) {
    // Daemon down, permission revoked, or the browser is quitting.
    // The records stay queued for the next event.
    return false;
  }

  // Re-read rather than assuming: a handler may have appended while the
  // request was in flight.
  const latest = await readQueue();
  await writeQueue(latest.slice(queue.length));
  return true;
}

// switchTo finalizes the current segment and starts a new one in a
// single step, which is what every event below actually wants.
async function switchTo(tab) {
  const now = Date.now();
  await finalize(now);
  await startTracking(tab, now);
  await deliver();
}

async function activeTabIn(windowId) {
  const tabs = await api.tabs.query({ active: true, windowId });
  return tabs[0] || null;
}

api.tabs.onActivated.addListener(async ({ tabId }) => {
  try {
    await switchTo(await api.tabs.get(tabId));
  } catch (err) {
    await finalize();
  }
});

// A tab keeps its id across navigations, so a URL or title change is a
// new segment for the same tab.
api.tabs.onUpdated.addListener(async (tabId, changeInfo, tab) => {
  if (!tab.active || (!changeInfo.url && !changeInfo.title)) {
    return;
  }
  if (!shouldSwitch(await readCurrent(), tab)) {
    return;
  }
  await switchTo(tab);
});

api.windows.onFocusChanged.addListener(async (windowId) => {
  // Focus left Firefox entirely: stop timing, or switching to a
  // terminal would credit the browser with the next hour.
  if (windowId === api.windows.WINDOW_ID_NONE) {
    await finalize();
    await deliver();
    return;
  }
  // Focus moved to another Firefox window, which a WINDOW_ID_NONE-only
  // rule would silently get wrong.
  await switchTo(await activeTabIn(windowId));
});

api.idle.onStateChanged.addListener(async (state) => {
  if (state === "idle" || state === "locked") {
    // The browser reports idleness one interval after it began, so the
    // segment ended then, not now.
    await finalize(idleEndedAt(Date.now()));
    await deliver();
    return;
  }
  const current = await api.windows.getLastFocused({ populate: false });
  await startTracking(await activeTabIn(current.id));
});

api.runtime.onSuspend.addListener(async () => {
  // Enqueue before attempting delivery: a fetch started during
  // suspension is not guaranteed to finish, so the record has to be
  // durable first.
  await finalize();
  await deliver();
});

// Deliver anything left over from a previous run as soon as the event
// page wakes.
api.runtime.onStartup.addListener(deliver);
api.runtime.onInstalled.addListener(deliver);
