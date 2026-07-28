// Reports the active tab to the local trackkrd daemon.
//
// The decision rules -- trackable, shouldSwitch, buildRecord,
// idleEndedAt, trimQueue, removeDelivered -- come from logic.js, which
// is free of browser APIs so node:test can cover them. This file is the
// plumbing around them: storage, events, and delivery.
//
// Two pieces of state, deliberately in different stores:
//
//   session  the tab currently being timed. It survives event-page
//            suspension but not a browser restart, which is exactly
//            right: a focus left over from a previous run is stale.
//   local    the queue of finalized records not yet delivered. Session
//            storage would lose them when Firefox exits, and "daemon
//            unreachable while the browser is quitting" is when a queue
//            earns its keep.
//
// This is an MV3 event page, so it is unloaded between events and can
// hold no durable state in module scope.

const CURRENT_KEY = "current";
const QUEUE_KEY = "queue";

api.idle.setDetectionInterval(IDLE_SECONDS);

// Every handler runs through this chain.
//
// Browser events arrive concurrently -- a navigation fires a URL update
// and a title update in quick succession -- and each handler is a
// read-modify-write over the same two keys. Unserialized, two finalizes
// can read the same segment before either clears it, producing a
// duplicate record, or two queue writers can clobber each other. The
// chain lives in module scope, so it covers one wake of the event page,
// which is the window in which those races happen.
let chain = Promise.resolve();

function serialize(work) {
  chain = chain.then(work, work);
  return chain;
}

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

// isFocused reports whether a window currently has the user's
// attention.
//
// tab.active only means "active within its own window", so a background
// window's tab is still active while the user is elsewhere. Acting on
// that alone starts timing a hidden tab, which is how a phantom segment
// spanning another application's hour gets recorded.
async function isFocused(windowId) {
  try {
    const win = await api.windows.get(windowId);
    return win.focused === true;
  } catch (err) {
    return false;
  }
}

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
  const batch = await readQueue();
  if (batch.length === 0) {
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
      body: JSON.stringify({ records: batch }),
    });
    if (!resp.ok) {
      return false;
    }
  } catch (err) {
    // Daemon down, permission revoked, or the browser is quitting. The
    // records stay queued for the next event.
    return false;
  }

  // Remove by identity, not by count: a record appended while the
  // request was in flight must survive, and at the queue limit the trim
  // keeps the length unchanged, so counting would delete the newcomer.
  await writeQueue(removeDelivered(await readQueue(), batch));
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

api.tabs.onActivated.addListener(({ tabId, windowId }) =>
  serialize(async () => {
    if (!(await isFocused(windowId))) {
      return;
    }
    try {
      await switchTo(await api.tabs.get(tabId));
    } catch (err) {
      await finalize();
    }
  }),
);

// A tab keeps its id across navigations, so a URL or title change is a
// new segment for the same tab -- but only if the user is looking at it.
api.tabs.onUpdated.addListener((tabId, changeInfo, tab) =>
  serialize(async () => {
    if (!tab.active || (!changeInfo.url && !changeInfo.title)) {
      return;
    }
    if (!(await isFocused(tab.windowId))) {
      return;
    }
    if (!shouldSwitch(await readCurrent(), tab)) {
      return;
    }
    await switchTo(tab);
  }),
);

api.windows.onFocusChanged.addListener((windowId) =>
  serialize(async () => {
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
  }),
);

api.idle.onStateChanged.addListener((state) =>
  serialize(async () => {
    if (state === "idle" || state === "locked") {
      // The browser reports idleness one interval after it began, so
      // the segment ended then, not now.
      await finalize(idleEndedAt(Date.now()));
      await deliver();
      return;
    }

    // Becoming active says the user touched *something*, not that they
    // came back to Firefox. getLastFocused returns Firefox's most
    // recent window even when another application has focus, so
    // resuming unconditionally would bill Firefox for the other
    // application's time -- and no focus-change event would arrive to
    // stop it, because Firefox never regained focus.
    const win = await api.windows.getLastFocused({ populate: false });
    if (!win || win.focused !== true) {
      return;
    }
    await startTracking(await activeTabIn(win.id));
  }),
);

// Closing the tracked tab ends its segment; no focus change follows if
// the window stays focused.
api.tabs.onRemoved.addListener((tabId) =>
  serialize(async () => {
    const current = await readCurrent();
    if (current && current.tabId === tabId) {
      await finalize();
      await deliver();
    }
  }),
);

// Firefox unloads an idle event page and fires this, which is not the
// same as the browser exiting. Finalizing here would truncate an
// ongoing visit and stop tracking until some later event happened to
// wake the page again -- so the current segment is deliberately left
// alone. It lives in session storage and survives the unload.
//
// Delivery is best effort: asynchronous work is not guaranteed to
// finish during suspension, which is precisely why the queue is made
// durable before this point rather than during it.
api.runtime.onSuspend.addListener(() => serialize(deliver));

// resume picks up tracking when the extension starts.
//
// Delivery alone is not enough: after a browser restart the user may
// simply read a restored tab, and no activation, navigation, focus, or
// idle event ever fires. Without this, that whole session goes
// unrecorded until they happen to switch tabs.
async function resume() {
  await deliver();

  // An existing segment survives event-page suspension and continues;
  // only a genuinely empty state needs seeding.
  if (await readCurrent()) {
    return;
  }

  try {
    const win = await api.windows.getLastFocused({ populate: false });
    if (!win || win.focused !== true) {
      return;
    }
    await startTracking(await activeTabIn(win.id));
  } catch (err) {
    // No window yet, which is normal early in startup. The next focus
    // or activation event will seed it.
  }
}

api.runtime.onStartup.addListener(() => serialize(resume));
api.runtime.onInstalled.addListener(() => serialize(resume));
