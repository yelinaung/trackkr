"use strict";

// Tests for background.js itself, driven through a fake browser.
//
// Everything here is a behaviour that review caught by reading, not by
// running: focus scoping, event-page suspension, concurrent handlers,
// idle gating. Each test is written so it fails against the version
// that had the bug.

const test = require("node:test");
const assert = require("node:assert/strict");

const { createHarness } = require("./harness.js");

const FOCUSED = 1;
const BACKGROUND = 2;

const IDLE_ALARM = "trackkr-idle-poll";

// activityRecords picks the uploaded records out of the request log.
// Idle polls share that log and carry no body, so an index would drift
// every time one is added.
function activityRecords(h) {
  return h.state.requests.filter((r) => r.body && r.body.records).flatMap((r) => r.body.records);
}

// idleReply answers the idle poll and lets every other request succeed,
// so a test can say what the daemon thinks without hand-building the
// whole response sequence.
function idleReply(json) {
  return (n) => (n === 1 ? { ok: true, status: 200, json } : { ok: true, status: 200 });
}

function twoWindows(overrides = {}) {
  return createHarness({
    focusedWindowId: FOCUSED,
    windows: { [FOCUSED]: { id: FOCUSED }, [BACKGROUND]: { id: BACKGROUND } },
    tabs: {
      10: { id: 10, windowId: FOCUSED, active: true, url: "https://focused.example", title: "Focused" },
      20: { id: 20, windowId: BACKGROUND, active: true, url: "https://background.example", title: "Background" },
    },
    ...overrides,
  });
}

test("a tab in the focused window starts a segment", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  assert.equal(h.current().url, "https://focused.example");
});

// tab.active only means active within its own window, so a title change
// in a background window must not steal tracking from the focused one.
test("a background window cannot take over tracking", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  await h.fire(
    "tabs.onUpdated",
    20,
    { title: "Background changed" },
    { id: 20, windowId: BACKGROUND, active: true, url: "https://background.example", title: "Background changed" },
  );
  await h.settled();

  assert.equal(h.current().url, "https://focused.example", "the focused tab is still the one being timed");
  assert.equal(h.queue().length, 0, "nothing was finalized");
});

test("activation in an unfocused window is ignored", async () => {
  const h = twoWindows();
  // Startup recovery seeds the focused window, as it does in a real worker.
  await h.settled();

  await h.fire("tabs.onActivated", { tabId: 20, windowId: BACKGROUND });
  await h.settled();

  assert.equal(
    h.current().url,
    "https://focused.example",
    "activation in a background window must not take over tracking",
  );
});

// Firefox unloads an idle event page and fires onSuspend; finalizing
// there truncated the visit and stopped tracking until some unrelated
// event happened to wake the page.
test("suspension leaves the current segment alone", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  await h.fire("runtime.onSuspend");
  await h.settled();

  assert.notEqual(h.current(), null, "the segment survives event-page suspension");
  assert.equal(h.current().url, "https://focused.example");
  assert.equal(h.queue().length, 0, "nothing was finalized by the unload");
});

// Two events arriving together used to read the same current segment
// before either cleared it, queueing the same record twice.
test("overlapping events do not duplicate a record", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  const tab = { id: 10, windowId: FOCUSED, active: true, url: "https://focused.example/next", title: "Next" };
  // Fired without awaiting: a navigation produces a URL update and a
  // title update back to back.
  const first = h.fire("tabs.onUpdated", 10, { url: tab.url }, tab);
  const second = h.fire("tabs.onUpdated", 10, { title: tab.title }, tab);
  await Promise.all([first, second]);
  await h.settled();

  assert.equal(h.queue().length + h.state.requests.length, 1, "exactly one record was produced");
});

test("losing focus to another application ends the segment", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.current(), null);
  assert.equal(h.state.requests.length, 1, "the finalized record was delivered");
  assert.equal(h.state.requests[0].body.records[0].url, "https://focused.example");
});

// A live-updating page can change its title while the user is away.
test("tab events do not restart tracking while the user is idle", async () => {
  const h = twoWindows({ idleState: "idle" });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  assert.equal(h.current(), null, "no segment starts while the user is away");
});

// A manual lock happens now, not one interval ago; backdating it ended
// young segments before they started, discarding them entirely.
test("locking does not discard a young segment", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  // Two minutes of reading, then a deliberate lock.
  h.state.session.current.startedAt -= 120_000;

  await h.fire("idle.onStateChanged", "locked");
  await h.settled();

  const sent = h.state.requests.flatMap((r) => r.body.records);
  assert.equal(sent.length, 1, "the segment survived the lock");
  assert.equal(sent[0].url, "https://focused.example");
});

test("going idle backdates the segment to when the user stopped", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  const startedAt = Date.now() - 20 * 60_000;
  h.state.session.current.startedAt = startedAt;

  await h.fire("idle.onStateChanged", "idle");
  await h.settled();

  // An idle event now asks the daemon before believing itself, so the
  // poll precedes the delivery. This harness answers it with no JSON
  // body, which is unusable, so the handler falls back to the browser's
  // own reckoning -- the behaviour this test was written for.
  const record = activityRecords(h)[0];
  const lengthMs = Date.parse(record.ended_at) - Date.parse(record.started_at);
  // Twenty minutes present minus the five-minute detection interval.
  assert.ok(
    Math.abs(lengthMs - 15 * 60_000) < 2000,
    `segment was ${lengthMs}ms, want about 15 minutes`,
  );
});

test("a failed delivery keeps the records queued", async () => {
  const h = twoWindows({ respond: () => ({ ok: false, status: 503 }) });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.queue().length, 1, "the record stays queued for the next attempt");
});

// The case that made count-based removal wrong: a record is appended
// while the request is in flight.
test("a record queued during delivery is not deleted by it", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  // Hold the POST open, then queue a second record behind it.
  const held = h.holdFetch();
  h.state.focusedWindowId = -1;
  const delivering = h.fire("windows.onFocusChanged", -1);

  // Wait until the request is actually in flight: appending before
  // that races the handler instead of the delivery.
  await held.started;
  h.state.local.queue = [
    ...(h.state.local.queue || []),
    {
      url: "https://later.example",
      title: "Later",
      started_at: new Date(Date.now() - 30_000).toISOString(),
      ended_at: new Date().toISOString(),
    },
  ];

  held.resolve();
  await delivering;
  await h.settled();

  const remaining = h.queue();
  assert.equal(remaining.length, 1, "the record added mid-request survived");
  assert.equal(remaining[0].url, "https://later.example");
});

test("nothing is sent without a token", async () => {
  const h = twoWindows({ token: "" });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.state.requests.length, 0);
  assert.equal(h.queue().length, 1, "the record waits for configuration");
});

// A revoked host permission must not look like a delivery attempt.
test("nothing is sent without the host permission", async () => {
  const h = twoWindows({ permitted: false });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.state.requests.length, 0);
  assert.equal(h.queue().length, 1);
});

test("private windows are never tracked", async () => {
  const h = twoWindows();
  h.state.tabs[30] = {
    id: 30,
    windowId: FOCUSED,
    active: true,
    incognito: true,
    url: "https://private.example",
    title: "Private",
  };

  await h.fire("tabs.onActivated", { tabId: 30, windowId: FOCUSED });
  await h.settled();

  assert.equal(h.current(), null, "nothing from a private window is stored");
  assert.equal(h.queue().length, 0);
});

test("closing the tracked tab finalizes it", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  await h.fire("tabs.onRemoved", 10);
  await h.settled();

  assert.equal(h.current(), null);
  assert.equal(h.state.requests[0].body.records[0].url, "https://focused.example");
});

// Without this, a browser restart where the user just reads a restored
// tab records nothing at all.
test("startup seeds tracking from the focused window", async () => {
  const h = twoWindows();

  await h.fire("runtime.onStartup");
  await h.settled();

  assert.notEqual(h.current(), null, "a segment started without any user action");
  assert.equal(h.current().url, "https://focused.example");
});

test("startup does not disturb an existing segment", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  const startedAt = h.current().startedAt;

  await h.fire("runtime.onStartup");
  await h.settled();

  assert.equal(h.current().startedAt, startedAt, "the running segment was not restarted");
});

// An ignored host must never be recorded at all: filtering in the
// browser means the daemon cannot log what it never receives.
test("an ignored site is never tracked", async () => {
  const h = twoWindows();
  h.state.local.ignored = ["private.example"];
  h.state.tabs[40] = {
    id: 40,
    windowId: FOCUSED,
    active: true,
    url: "https://login.private.example/session",
    title: "Bank",
  };

  await h.fire("tabs.onActivated", { tabId: 40, windowId: FOCUSED });
  await h.settled();

  assert.equal(h.current(), null, "no segment started for an ignored host");
  assert.equal(h.queue().length, 0);
  assert.equal(h.state.requests.length, 0, "nothing was sent");
});

// Navigating from a recorded page to an ignored one must close the
// first segment rather than silently continuing it.
test("navigating to an ignored site ends the previous segment", async () => {
  const h = twoWindows();
  h.state.local.ignored = ["private.example"];

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  const tab = {
    id: 10,
    windowId: FOCUSED,
    active: true,
    url: "https://private.example/secret",
    title: "Secret",
  };
  await h.fire("tabs.onUpdated", 10, { url: tab.url }, tab);
  await h.settled();

  assert.equal(h.current(), null, "the ignored page is not being timed");

  const sent = h.state.requests.flatMap((r) => r.body.records);
  assert.equal(sent.length, 1, "the earlier page was still recorded");
  assert.equal(sent[0].url, "https://focused.example");
  for (const record of sent) {
    assert.equal(record.url.includes("private.example"), false, "an ignored URL was sent");
  }
});

// Adding a rule while its page is open must discard that segment. The
// copy in session storage was captured before the rule existed.
test("ignoring a site discards the segment already being timed", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;
  assert.notEqual(h.current(), null, "a segment is running");

  // What saving the options page does.
  await h.context.api.storage.local.set({ ignored: ["focused.example"] });
  await h.settled();

  assert.equal(h.current(), null, "the running segment was dropped");
  assert.equal(h.queue().length, 0, "and was not queued on the way out");
  assert.equal(h.state.requests.length, 0);
});

// The queue is durable, so a rule added after a record was captured has
// to reach back into it.
test("ignoring a site purges matching records already queued", async () => {
  const h = twoWindows({ respond: () => ({ ok: false, status: 503 }) });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  // Delivery fails, so the record stays queued.
  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();
  assert.equal(h.queue().length, 1, "the record is waiting to send");

  await h.context.api.storage.local.set({ ignored: ["focused.example"] });
  await h.settled();

  assert.equal(h.queue().length, 0, "the queued record was purged");
});

// Belt and braces: even if a record reaches the queue some other way,
// it must not be sent once its host is ignored.
test("a queued record for an ignored host is never delivered", async () => {
  const h = twoWindows();

  h.state.local.ignored = ["secret.example"];
  h.state.local.queue = [
    {
      url: "https://secret.example/page",
      title: "Secret",
      started_at: new Date(Date.now() - 120_000).toISOString(),
      ended_at: new Date(Date.now() - 60_000).toISOString(),
    },
  ];

  await h.fire("runtime.onStartup");
  await h.settled();

  assert.equal(h.state.requests.length, 0, "nothing was sent");
  assert.equal(h.queue().length, 0, "and the record was dropped, not kept");
});

// The event page can be unloaded between the rule landing in storage
// and the segment ending, so the change listener may never have run in
// this instance. finalize rechecks for exactly that case.
test("finalize rechecks the rules even if the change was never observed", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.state.session.current.startedAt -= 60_000;

  // Written directly, bypassing storage.onChanged: this is what a
  // restarted event page sees -- rules already in storage, no
  // notification in this instance.
  h.state.local.ignored = ["focused.example"];

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.state.requests.length, 0, "nothing was sent");
  assert.equal(h.queue().length, 0, "nothing was left queued");

  // The stronger claim: the URL never reached storage at all. Relying
  // on the delivery filter alone would persist it first and remove it
  // afterwards.
  const persisted = JSON.stringify(h.state.writes);
  assert.equal(
    persisted.includes("focused.example"),
    false,
    "an ignored URL was written to storage before being filtered",
  );
});

// The alarm is the only clock that survives Chrome evicting the worker,
// so its lifecycle is worth asserting directly.

test("the idle poll runs only while a segment is open", async () => {
  const h = twoWindows();

  assert.equal(h.alarm(IDLE_ALARM), null, "polling before anything is timed");

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  assert.notEqual(h.alarm(IDLE_ALARM), null, "no poll while a segment is open");

  h.state.focusedWindowId = -1;
  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  // The alarm outlives the segment on purpose. Every switch finalizes
  // before it starts, so tearing it down here would rebuild it ahead of
  // each deadline and it would never fire. It stops itself on the next
  // poll instead, costing one stray wake.
  await h.fire("alarms.onAlarm", { name: IDLE_ALARM });
  await h.settled();
  assert.equal(h.alarm(IDLE_ALARM), null, "the poll did not stop itself");
});

test("an alarm belonging to someone else is ignored", async () => {
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.age(10 * 60_000);

  await h.fire("alarms.onAlarm", { name: "somebody-elses-alarm" });
  await h.settled();

  assert.notEqual(h.current(), null, "a foreign alarm closed the segment");
  assert.equal(h.state.requests.length, 0, "a foreign alarm talked to the daemon");
});

test("a worker that lost its alarm recreates it on recovery", async () => {
  // Chrome promises no alarm survives a restart before 150, and a
  // segment left running with no poll behind it overcounts silently.
  const h = twoWindows({
    session: {
      current: {
        recordId: "00000000-0000-4000-8000-000000000001",
        tabId: 10,
        windowId: FOCUSED,
        url: "https://focused.example",
        title: "Focused",
        incognito: false,
        startedAt: Date.now() - 60_000,
      },
    },
  });
  await h.settled();
  h.dropAlarm(IDLE_ALARM);

  await h.fire("runtime.onStartup");
  await h.settled();

  assert.notEqual(h.alarm(IDLE_ALARM), null, "the alarm was not restored");
});

test("the daemon closes the segment where the user stopped", async () => {
  const stoppedAt = Date.now() - 8 * 60_000;
  const h = twoWindows({
    respond: idleReply({ idle: true, idle_since: new Date(stoppedAt).toISOString() }),
  });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.age(20 * 60_000);

  await h.fire("alarms.onAlarm", { name: IDLE_ALARM });
  await h.settled();

  const records = activityRecords(h);
  assert.equal(records.length, 1, "the poll did not close the segment");
  const endedAt = Date.parse(records[0].ended_at);
  assert.ok(
    Math.abs(endedAt - stoppedAt) < 2000,
    `segment ended at ${records[0].ended_at}, want the moment the daemon reported`,
  );
});

test("a daemon reporting an active user overrides browser idle", async () => {
  // The whole point of the arbitration. browser.idle on Wayland fires
  // late or never, so its word alone must not end a segment.
  const h = twoWindows({ respond: idleReply({ idle: false, threshold_s: 300 }) });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.age(20 * 60_000);

  await h.fire("idle.onStateChanged", "idle");
  await h.settled();

  assert.notEqual(h.current(), null, "the browser closed a segment the daemon kept open");
  assert.equal(activityRecords(h).length, 0, "a record was written anyway");
  assert.equal(h.idleSource(), "daemon");
});

test("an unusable reply hands idle back to the browser", async () => {
  const h = twoWindows({ respond: () => ({ ok: false, status: 503 }) });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.age(20 * 60_000);

  await h.fire("idle.onStateChanged", "idle");
  await h.settled();

  assert.equal(h.idleSource(), "browser", "a 503 left the daemon in charge");
  assert.equal(h.current(), null, "the fallback did not close the segment");
});

test("a lock ends the segment at once under either source", async () => {
  const h = twoWindows({ respond: idleReply({ idle: false, threshold_s: 300 }) });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  h.age(10 * 60_000);

  await h.fire("idle.onStateChanged", "locked");
  await h.settled();

  const records = activityRecords(h);
  assert.equal(records.length, 1, "a lock did not close the segment");
  // No backdating: the user asked for the lock now.
  const lengthMs = Date.parse(records[0].ended_at) - Date.parse(records[0].started_at);
  assert.ok(
    Math.abs(lengthMs - 10 * 60_000) < 2000,
    `segment was ${lengthMs}ms, want the full ten minutes`,
  );
});

// Regressions from review. Each fails against the version that shipped
// the arbitration without it.

test("a stale browser idle does not block a new segment", async () => {
  // The self-sustaining failure: browser.idle wrongly reports idle on
  // Wayland, so no segment starts, so no alarm exists, so nothing polls
  // the daemon, so nothing ever corrects it. Tracking stops for good.
  const h = twoWindows({
    idleState: "idle",
    respond: () => ({ ok: true, status: 200, json: { idle: false, threshold_s: 300 } }),
  });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  assert.notEqual(h.current(), null, "the daemon said active and no segment started");
  assert.equal(h.current().url, "https://focused.example");
  assert.notEqual(h.alarm(IDLE_ALARM), null, "no poll to recover with");
});

test("rewriting a segment does not restart the poll countdown", async () => {
  // Creating an alarm over an existing name cancels and replaces it. A
  // title that changes every few seconds -- a chat window carrying an
  // unread count -- would push the poll past every deadline it had.
  const h = twoWindows();

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();
  const created = h.alarmCreates();

  for (let i = 0; i < 5; i += 1) {
    const tab = {
      id: 10,
      windowId: FOCUSED,
      active: true,
      url: "https://focused.example",
      title: `Focused (${i})`,
    };
    await h.fire("tabs.onUpdated", 10, { title: tab.title }, tab);
    await h.settled();
  }

  assert.notEqual(h.alarm(IDLE_ALARM), null, "the alarm went missing");
  assert.equal(h.alarmCreates(), created, "the alarm was recreated, resetting its countdown");
});

test("a recovered daemon is found again after a transient failure", async () => {
  // askDaemonIdle is the only thing that moves the source back to the
  // daemon, and with no segment open nothing else calls it: no segment
  // means no alarm, no alarm means no poll. Gating the probe on the
  // source made one failed request permanent.
  const h = twoWindows({
    idleState: "idle",
    session: { idleSource: "browser" },
    respond: () => ({ ok: true, status: 200, json: { idle: false, threshold_s: 300 } }),
  });

  await h.fire("tabs.onActivated", { tabId: 10, windowId: FOCUSED });
  await h.settled();

  assert.notEqual(h.current(), null, "the recovered daemon was never asked");
  assert.equal(h.idleSource(), "daemon", "the source never came back");
});
