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

  await h.fire("tabs.onActivated", { tabId: 20, windowId: BACKGROUND });
  await h.settled();

  assert.equal(h.current(), null);
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

  const record = h.state.requests[0].body.records[0];
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
