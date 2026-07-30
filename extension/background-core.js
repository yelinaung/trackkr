// Browser-neutral tracking core, shared by Firefox and Chrome.
//
// The decision rules -- trackable, shouldSwitch, buildRecord, usableSegment,
// queueHasRecord, idleEndsAt, trimQueue, removeDelivered -- come from logic.js,
// which is free of browser APIs so node:test can cover them. This file is the
// plumbing around them: storage, events, and delivery.
//
// Lifecycle differences stay out. Firefox runs an event page with
// runtime.onSuspend; Chrome runs a service worker that is killed without
// warning and has no such event. Each entrypoint registers what its browser
// actually provides, and calls registerListeners then recover from here.
//
// Two pieces of state, deliberately in different stores:
//
//   session  the tab currently being timed. It survives suspension but not a
//            browser restart, which is exactly right: a focus left over from a
//            previous run is stale.
//   local    the queue of finalized records not yet delivered. Session storage
//            would lose them when the browser exits, and "daemon unreachable
//            while the browser is quitting" is when a queue earns its keep.
//
// Neither runtime may hold durable state in module scope: Firefox unloads the
// event page and Chrome terminates the worker.

const CURRENT_KEY = "current";
const QUEUE_KEY = "queue";

// DELIVERY_TIMEOUT_MS bounds one upload attempt. Delivery runs on the same
// serialization chain as every tracking event, so this is the ceiling on how
// long a wedged daemon can stall them.
const DELIVERY_TIMEOUT_MS = 10_000;

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

// pendingWork hands out a promise for everything currently serialized.
//
// Recovery starts during script evaluation rather than from an event, so a test
// harness has nothing to await unless the chain is reachable -- and `chain` is a
// lexical binding, invisible to a caller. One closure is cheaper than making
// every caller thread promises back out.
globalThis.pendingWork = function pendingWork() {
  return chain;
};

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

// userIsActive reports whether the person is actually at the machine.
//
// Focus alone is not enough: a live-updating page can change its title
// while the user is away, and without this check that event would start
// a fresh segment and quietly record the rest of their absence.
async function userState() {
  try {
    return await api.idle.queryState(IDLE_SECONDS);
  } catch (err) {
    // If the state cannot be read, assume presence rather than
    // silently stopping all tracking.
    return "active";
  }
}

async function userIsActive() {
  return (await userState()) === "active";
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

  // Filtering here rather than on the daemon is the point: an ignored
  // host is never stored, never sent, and never appears in a log. The
  // daemon cannot un-see a URL it has already received.
  const { ignored } = await getSettings();
  if (isIgnored(tab.url, ignored)) {
    await writeCurrent(null);
    return;
  }
  await writeCurrent({
    // Minted here, at the start of the logical segment, and carried through
    // the durable queue so a retry replays instead of inserting twice.
    recordId: crypto.randomUUID(),
    tabId: tab.id,
    windowId: tab.windowId,
    url: tab.url,
    title: tab.title || "",
    // Stored explicitly rather than inferred later. On recovery this state is
    // untrusted, and "absent" cannot be read as "not private".
    incognito: tab.incognito === true,
    startedAt,
  });
}

// finalize closes the current segment and queues it. endedAt is a
// parameter because idle transitions end the segment when the user
// stopped, not when the browser noticed.
async function finalize(endedAt = Date.now()) {
  const current = await readCurrent();
  const record = buildRecord(current, endedAt);

  // Nothing worth keeping: clear the session copy and stop. An invalid,
  // too-short, or incognito segment leaves no trace.
  if (record === null || current.incognito === true) {
    await writeCurrent(null);
    return;
  }

  // Recheck at the last moment. A rule can be added while this segment is
  // open, and the copy in session storage was captured before that.
  const { ignored } = await getSettings();
  if (isIgnored(record.url, ignored)) {
    await writeCurrent(null);
    return;
  }

  // Queue first, then clear. The reverse order loses the segment outright if
  // the runtime dies between the two writes -- a narrow window on a Firefox
  // event page, a routine one under Chrome's unannounced worker termination.
  // Appending is idempotent, so surviving with both copies is recoverable
  // while surviving with neither is not.
  const queue = await readQueue();
  if (!queueHasRecord(queue, record.record_id)) {
    queue.push(record);
    await writeQueue(queue);
  }

  // Only now is the durable copy safe. A failure above propagates with the
  // session copy intact.
  await writeCurrent(null);
}

// deliver sends the queue as one batch, mirroring the daemon's own
// ingest shape so draining a backlog costs a single request. The queue
// is only cleared once the daemon has accepted it.
async function deliver() {
  const { daemonUrl, token, ignored } = await getSettings();

  // Filter immediately before sending, not only when queueing. The
  // queue is durable, so a page recorded while the daemon was down
  // would otherwise still be delivered after its host was ignored.
  const queued = await readQueue();
  const batch = filterIgnored(queued, ignored);
  if (batch.length !== queued.length) {
    await writeQueue(batch);
  }
  if (batch.length === 0) {
    return true;
  }

  if (!token || !(await hasHostPermission(daemonUrl))) {
    return false;
  }

  // Delivery shares the serialization chain with every tracking event, so an
  // unbounded request is not just a slow upload -- a daemon that accepts the
  // connection and never answers holds up activations, focus changes, and idle
  // transitions behind it until Chrome kills the worker, taking their
  // in-memory transition times with it. Recovery could then infer only the
  // final tab. The deadline sits well below Chrome's own request limit; the
  // batch stays queued and is retried.
  const timeout = new AbortController();
  const deadline = setTimeout(() => timeout.abort(), DELIVERY_TIMEOUT_MS);
  try {
    const resp = await fetch(new URL(ACTIVITY_PATH, daemonUrl), {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify({ records: batch }),
      signal: timeout.signal,
    });
    if (!resp.ok) {
      return false;
    }
  } catch (err) {
    // Daemon down, permission revoked, request aborted at the deadline, or the
    // browser is quitting. The records stay queued for the next event.
    return false;
  } finally {
    clearTimeout(deadline);
  }

  // Remove by identity, not by count: a record appended while the
  // request was in flight must survive, and at the queue limit the trim
  // keeps the length unchanged, so counting would delete the newcomer.
  await writeQueue(removeDelivered(await readQueue(), batch));
  return true;
}

// switchTo finalizes the current segment and starts a new one in a
// single step, which is what every event below actually wants.
//
// The new segment only starts if the user is present. Ending the old
// one is unconditional: whatever prompted the event, the previous tab
// stopped being watched.
async function switchTo(tab) {
  const now = Date.now();
  await finalize(now);
  if (await userIsActive()) {
    await startTracking(tab, now);
  }
  await deliver();
}

async function activeTabIn(windowId) {
  const tabs = await api.tabs.query({ active: true, windowId });
  return tabs[0] || null;
}


// registerListeners installs every event listener, synchronously.
//
// Chrome requires this: a service worker is woken by an event, and if the
// listener for that event is not registered by the time global evaluation
// finishes, the event is lost and the worker goes back to sleep. So nothing
// here may await, read storage, or wait on a timer -- the callbacks are free to
// do that once they fire, but installing them cannot.
globalThis.registerListeners = function registerListeners() {
  api.idle.setDetectionInterval(IDLE_SECONDS);

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
        // Idleness is reported one interval after it began, so that
        // segment ended then. A lock happens the moment the user asks
        // for it, and backdating it would end a young segment before it
        // started -- discarding real browsing.
        await finalize(idleEndsAt(state, Date.now()));
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

  // Rules changing must affect what is already captured, not just what
  // comes next. Without this, adding a rule while its page is open
  // leaves the segment in session storage, and finalize would queue and
  // send it.
  api.storage.onChanged.addListener((changes, area) =>
    serialize(async () => {
      if (area !== "local" || !changes.ignored) {
        return;
      }
      const patterns = changes.ignored.newValue || [];

      // Discarded, not finalized: a newly ignored page must leave no
      // record behind at all.
      const current = await readCurrent();
      if (current && isIgnored(current.url, patterns)) {
        await writeCurrent(null);
      }

      const queued = await readQueue();
      const kept = filterIgnored(queued, patterns);
      if (kept.length !== queued.length) {
        await writeQueue(kept);
      }
    }),
  );

  // Registered here rather than left to the entrypoint: Chrome delivers
  // onStartup to a worker only if the listener exists when global
  // evaluation ends, and these are the events that wake a worker after a
  // browser restart.
  api.runtime.onStartup.addListener(() => recover());
  api.runtime.onInstalled.addListener(() => recover());
};

// recover runs after registration, never before it.
//
// Delivery alone is not enough: after a browser restart the user may simply
// read a restored tab, and no activation, navigation, focus, or idle event ever
// fires. Without this that whole session goes unrecorded until they happen to
// switch tabs.
//
// It also has to deal with session state it did not necessarily write. A worker
// that was killed mid-finalization leaves a segment behind, and an older build
// leaves one without an identity, so this treats persisted state as untrusted
// input rather than as its own memory.
// Coalesced, because recovery has two triggers that fire together. The
// entrypoint starts it during evaluation, and onStartup or onInstalled is
// usually the very event that woke the worker -- so a startup wake queued two
// recoveries, each with its own delivery attempt. Against a daemon that accepts
// connections and never answers, that meant two consecutive deadlines and about
// twenty seconds of blocked tracking rather than the ten the deadline promises.
let recovering = null;

globalThis.recover = function recover() {
  if (recovering) {
    return recovering;
  }
  recovering = serialize(resume).finally(() => {
    recovering = null;
  });
  return recovering;
};

async function resume() {
  // Reconcile before delivering. A successful delivery removes the queued
  // record, and that record is the only evidence that the session copy was
  // already finalized -- deliver first and recovery cannot tell an interrupted
  // finalization from a live segment, so it keeps timing under an identity the
  // daemon has already stored. The replay is then discarded as a duplicate and
  // everything since is lost.
  const current = await readCurrent();
  let keepCurrent = false;
  if (current) {
    keepCurrent = await resolveRecoveredSegment(current);
  }

  await deliver();

  if (keepCurrent) {
    // Still the tab the user is looking at: keep its original start time
    // rather than restarting the clock on a visit already in progress.
    return;
  }

  // Window focus alone is not presence. idle.onStateChanged does not fire
  // merely because a listener was registered while the machine was already
  // idle, so seeding here on focus alone would record the user's absence until
  // some later event happened to arrive.
  if (!(await userIsActive())) {
    return;
  }

  try {
    const win = await api.windows.getLastFocused({ populate: false });
    if (!win || win.focused !== true) {
      return;
    }
    await startTracking(await activeTabIn(win.id));
  } catch (err) {
    // No window yet, which is normal early in startup. The next focus or
    // activation event will seed one.
  }
}

// resolveRecoveredSegment decides what to do with persisted session state and
// reports whether that state should continue as the current segment.
async function resolveRecoveredSegment(current) {
  const now = Date.now();
  const { ignored } = await getSettings();

  if (legacySegment(current)) {
    const upgraded = await upgradeLegacySegment(current);
    if (upgraded === null) {
      // Could not prove the tab was non-private: discard rather than invent
      // an identity for activity we cannot vouch for.
      await writeCurrent(null);
      return false;
    }
    current = upgraded;
  }

  // Corrupt, future-dated, incognito, or newly ignored state is discarded
  // without queueing. It is not activity, and finalizing it would turn a bad
  // write into a record.
  if (!usableSegment(current, now, ignored)) {
    await writeCurrent(null);
    return false;
  }

  // A queued copy with this identity means a previous run wrote the durable
  // record and died before clearing the session. Clear it now; appending
  // again would duplicate.
  if (queueHasRecord(await readQueue(), current.recordId)) {
    await writeCurrent(null);
    return false;
  }

  if (await stillActive(current)) {
    return true;
  }

  // Close the segment through the same queue-first handoff. When the user is
  // already idle, end it where the idle handler would have: an idle transition
  // is what wakes a sleeping worker, recovery is serialized ahead of that
  // handler, and finalizing at "now" would leave the handler with no segment to
  // backdate -- silently adding the whole idle threshold to nearly every
  // segment that ends after a cold wake.
  await finalize(idleEndsAt(await userState(), now));
  return false;
}

// upgradeLegacySegment fills in the identity an older build did not store, but
// only when the live tab can still prove the segment was not private.
async function upgradeLegacySegment(current) {
  let tab;
  try {
    tab = await api.tabs.get(current.tabId);
  } catch (err) {
    return null;
  }
  if (!tab || tab.incognito !== false) {
    return null;
  }
  if (tab.windowId !== current.windowId || tab.url !== current.url) {
    return null;
  }

  // Both fields were absent to reach here, so both are minted rather than
  // repaired.
  const upgraded = {
    ...current,
    recordId: crypto.randomUUID(),
    incognito: false,
  };
  await writeCurrent(upgraded);
  return upgraded;
}

// stillActive reports whether the recovered segment is the tab the user is
// looking at right now.
async function stillActive(current) {
  if (!(await userIsActive())) {
    return false;
  }
  try {
    const tab = await api.tabs.get(current.tabId);
    if (!tab || !tab.active || tab.url !== current.url) {
      return false;
    }
    return await isFocused(tab.windowId);
  } catch (err) {
    return false;
  }
}
