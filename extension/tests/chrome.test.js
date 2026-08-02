"use strict";

const test = require("node:test");
const assert = require("node:assert");
const { createHarness } = require("./harness");

const CHROME = { entrypoint: "background-cr.js" };

function chromeHarness(extra = {}) {
  return createHarness({
    ...CHROME,
    tabs: {
      // A real browser always populates incognito, and the legacy upgrade path
      // needs it to prove a tab was not private.
      10: {
        id: 10,
        windowId: 1,
        active: true,
        url: "https://example.com/a",
        title: "Docs",
        incognito: false,
      },
    },
    windows: { 1: { id: 1, focused: true } },
    focusedWindowId: 1,
    ...extra,
  });
}

test("the Chrome worker starts without runtime.onSuspend", async () => {
  // Chrome has no such event. Touching it during startup would throw a
  // TypeError and take every listener down with it.
  const h = chromeHarness();
  await h.settled();

  assert.equal(
    h.api.runtime.onSuspend,
    undefined,
    "the Chrome harness must not offer an event Chrome does not have",
  );
  assert.equal(h.current().url, "https://example.com/a", "tracking started anyway");
});

test("only the Firefox entrypoint registers runtime.onSuspend", async () => {
  const firefox = createHarness({
    tabs: { 10: { id: 10, windowId: 1, active: true, url: "https://example.com/a", title: "Docs" } },
    windows: { 1: { id: 1, focused: true } },
  });
  await firefox.settled();

  // Firing it proves the listener exists; the Chrome harness has no such
  // event to fire at all.
  await firefox.fire("runtime.onSuspend");
  await firefox.settled();
  assert.ok(firefox.current(), "suspension must leave the segment alone");
});

test("every listener exists before any storage read", () => {
  // Chrome delivers the event that woke a worker only if the listener is
  // already registered when global evaluation finishes. Nothing is awaited
  // here: this assertion runs in the same synchronous turn as construction.
  const h = chromeHarness();

  for (const name of [
    "tabs.onActivated",
    "tabs.onUpdated",
    "tabs.onRemoved",
    "windows.onFocusChanged",
    "idle.onStateChanged",
    "storage.onChanged",
    "runtime.onStartup",
    "runtime.onInstalled",
  ]) {
    assert.ok(h.registered(name), `${name} was not registered synchronously`);
  }

  assert.equal(h.state.reads, 0, "registration must complete before any storage read");
});

function requestPaths(h) {
  return h.state.requests.map((request) => new URL(request.url).pathname);
}

test("Chrome delivers to its own endpoint", async () => {
  const h = chromeHarness();
  await h.settled();
  h.age(60_000);

  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  const paths = requestPaths(h);
  assert.ok(paths.length > 0, "nothing was delivered");
  assert.ok(
    paths.every((p) => p === "/extension/activity/chrome"),
    `Chrome posted to ${paths.join(", ")}`,
  );
});

test("Firefox keeps the legacy endpoint", async () => {
  const h = createHarness({
    tabs: { 10: { id: 10, windowId: 1, active: true, url: "https://example.com/a", title: "Docs" } },
    windows: { 1: { id: 1, focused: true } },
  });
  await h.settled();
  h.age(60_000);

  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  const paths = requestPaths(h);
  assert.ok(paths.length > 0, "nothing was delivered");
  assert.ok(
    paths.every((p) => p === "/extension/activity"),
    `Firefox posted to ${paths.join(", ")}`,
  );
});

test("a rejected Chrome endpoint leaves the queue intact", async () => {
  const h = chromeHarness({ respond: () => ({ ok: false, status: 404 }) });
  await h.settled();
  h.age(60_000);

  await h.fire("windows.onFocusChanged", -1);
  await h.settled();

  assert.equal(h.queue().length, 1, "a 404 from an old daemon must not discard the record");
});

test("the segment reaches the queue before the session copy is cleared", async () => {
  const h = chromeHarness();
  await h.settled();
  h.age(60_000);
  const segment = h.current();
  assert.ok(segment.recordId, "a new segment carries an identity");

  // Make the session removal fail after the durable queue write succeeds.
  h.failSessionRemove();

  await assert.rejects(
    () => h.fire("windows.onFocusChanged", -1),
    "finalization must propagate a persistence failure rather than swallow it",
  );

  assert.equal(h.queue().length, 1, "the record was made durable first");
  assert.equal(h.queue()[0].record_id, segment.recordId, "the queued record keeps its identity");
  assert.ok(h.current(), "the session copy survives so nothing is lost");
});

test("recovery clears a session copy already present in the queue", async () => {
  // Delivery must fail, or the queued record drains before recovery can be
  // seen reconciling it.
  const h = chromeHarness({ respond: () => ({ ok: false, status: 503 }) });
  await h.settled();
  const segment = h.current();

  // Reproduce a worker killed between the queue write and the session clear.
  h.setQueue([
    {
      record_id: segment.recordId,
      url: segment.url,
      title: segment.title,
      started_at: new Date(segment.startedAt).toISOString(),
      ended_at: new Date(segment.startedAt + 60_000).toISOString(),
    },
  ]);
  h.closeTab(10);

  await h.fire("runtime.onStartup");
  await h.settled();

  assert.equal(h.queue().length, 1, "the queued record must not be appended twice");
  assert.equal(h.current(), null, "a reconciled session copy must be cleared");
});

test("recovery discards state it cannot vouch for", async () => {
  const cases = {
    "no record id": { tabId: 10, windowId: 1, url: "https://example.com/a", startedAt: 1, incognito: false },
    "incognito": {
      recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
      tabId: 10,
      windowId: 1,
      url: "https://example.com/a",
      startedAt: 1,
      incognito: true,
    },
    "future dated": {
      recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
      tabId: 10,
      windowId: 1,
      url: "https://example.com/a",
      startedAt: Date.now() + 3_600_000,
      incognito: false,
    },
    "not a web url": {
      recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
      tabId: 10,
      windowId: 1,
      url: "about:config",
      startedAt: 1,
      incognito: false,
    },
    "malformed": { nonsense: true },
  };

  for (const [name, current] of Object.entries(cases)) {
    const h = chromeHarness();
    await h.settled();
    h.setCurrent(current);
    h.closeTab(10);
    h.setQueue([]);

    await h.fire("runtime.onStartup");
    await h.settled();

    assert.equal(h.queue().length, 0, `${name} must not become a record`);
  }
});

test("finalizing twice queues the record once", async () => {
  // The queue-first ordering deliberately leaves both copies when the session
  // clear fails. Any event that finalizes again before recovery reconciles
  // must not append a second copy of the same segment.
  const h = chromeHarness({ respond: () => ({ ok: false, status: 503 }) });
  await h.settled();
  h.age(60_000);
  const segment = h.current();

  h.failSessionRemove();
  await assert.rejects(() => h.fire("windows.onFocusChanged", -1));
  assert.equal(h.queue().length, 1, "the first finalize queued the record");

  await assert.rejects(() => h.fire("windows.onFocusChanged", -1));
  assert.equal(h.queue().length, 1, "finalizing twice must queue the record once");
  assert.equal(h.queue()[0].record_id, segment.recordId);
});

test("both manifests can request every loopback form the daemon may bind", () => {
  const fs = require("node:fs");
  const path = require("node:path");
  const { originFor } = require("../logic.js");
  const dir = path.join(__dirname, "..");

  // extension_addr is configurable and the daemon accepts any loopback bind.
  // A manifest that declares only one form leaves the extension unable to ask
  // for access to a config the daemon considers perfectly valid -- and a
  // blocked request looks exactly like a dead daemon.
  const addresses = ["http://127.0.0.1:7600", "http://localhost:7600", "http://[::1]:7600"];

  for (const manifest of ["manifest.json", "manifest.chrome.json"]) {
    const declared = JSON.parse(fs.readFileSync(path.join(dir, manifest), "utf8"))
      .optional_host_permissions;
    for (const address of addresses) {
      assert.ok(
        declared.includes(originFor(address)),
        `${manifest} cannot request ${originFor(address)}`,
      );
    }
  }
});

test("the Chrome manifest is a service worker with no Gecko keys", () => {
  const fs = require("node:fs");
  const path = require("node:path");
  const manifest = JSON.parse(
    fs.readFileSync(path.join(__dirname, "..", "manifest.chrome.json"), "utf8"),
  );

  assert.equal(manifest.manifest_version, 3);
  assert.equal(manifest.background.service_worker, "background-cr.js");
  assert.equal("scripts" in manifest.background, false, "background.scripts is a Firefox key");
  assert.equal(manifest.minimum_chrome_version, "116");
  assert.deepEqual([...manifest.permissions].sort(), ["alarms", "idle", "storage", "tabs"]);

  for (const key of ["browser_specific_settings", "data_collection_permissions"]) {
    assert.equal(key in manifest, false, `${key} is Gecko-only`);
  }
});

test("a cold wake by idle keeps the idle backdating", async () => {
  // A worker asleep when the user goes idle is woken by idle.onStateChanged.
  // Recovery is serialized first and sees a non-active user, so if it finalizes
  // at "now" the later idle handler finds no segment and its backdating never
  // applies -- adding the whole idle threshold to the segment.
  const startedAt = Date.now() - 600_000;
  const h = chromeHarness({
    respond: () => ({ ok: false, status: 503 }),
    // Already idle when the worker starts, with a segment left behind: the
    // exact state a worker inherits when an idle transition wakes it.
    idleState: "idle",
    session: {
      current: {
        recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
        tabId: 10,
        windowId: 1,
        url: "https://example.com/a",
        title: "Docs",
        incognito: false,
        startedAt,
      },
    },
  });

  // No settle in between: recovery and the idle handler are both in flight,
  // which is the ordering a cold wake produces.
  await h.fire("idle.onStateChanged", "idle");
  await h.settled();

  assert.equal(h.queue().length, 1, "the segment was not finalized");
  const endedAt = Date.parse(h.queue()[0].ended_at);
  const idleMs = h.context.IDLE_SECONDS * 1000;

  assert.ok(
    endedAt <= startedAt + 600_000 - idleMs + 2_000,
    `segment ended at +${endedAt - startedAt}ms, want the idle threshold backdated off`,
  );
});

test("a cold wake by lock ends the segment without backdating", async () => {
  // A lock is deliberate and immediate, unlike an idle transition. Recovery
  // runs before the waking handler, so it must preserve that distinction when
  // it closes the inherited segment itself.
  const now = Date.now();
  const h = chromeHarness({
    respond: () => ({ ok: false, status: 503 }),
    idleState: "locked",
    session: {
      current: {
        recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
        tabId: 10,
        windowId: 1,
        url: "https://example.com/a",
        title: "Docs",
        incognito: false,
        startedAt: now - 60_000,
      },
    },
  });

  await h.fire("idle.onStateChanged", "locked");
  await h.settled();

  assert.equal(h.queue().length, 1, "the segment was not finalized");
  const endedAt = Date.parse(h.queue()[0].ended_at);
  assert.ok(endedAt >= now - 2_000, `lock was incorrectly backdated to ${endedAt}`);
});

test("recovery does not start tracking while the user is already idle", async () => {
  // A browser start or extension update can run recovery with no valid segment
  // while the machine is idle. idle.onStateChanged does not fire just because a
  // listener was registered, so seeding on window focus alone would record the
  // absence until some later event arrived.
  const h = chromeHarness({ idleState: "idle" });
  await h.settled();

  assert.equal(h.current(), null, "an idle machine must not be credited with a new segment");
});

test("recovery still seeds when the user is present", async () => {
  const h = chromeHarness();
  await h.settled();

  assert.equal(h.current().url, "https://example.com/a");
});

test("the two manifests agree on everything shared", () => {
  const fs = require("node:fs");
  const path = require("node:path");
  const dir = path.join(__dirname, "..");
  const read = (name) => JSON.parse(fs.readFileSync(path.join(dir, name), "utf8"));

  const firefox = read("manifest.json");
  const chrome = read("manifest.chrome.json");

  // They are separate source files: ext-lint validates only the Firefox one and
  // Chrome rejects Gecko keys, so nothing else stops a release that bumps one
  // from shipping the other stale.
  for (const key of [
    "name",
    "version",
    "description",
    "permissions",
    "optional_host_permissions",
    "action",
    "options_ui",
    "icons",
  ]) {
    assert.deepEqual(chrome[key], firefox[key], `${key} has drifted between the manifests`);
  }
});

test("a delivered segment does not keep being timed under its spent identity", async () => {
  // A title change queues segment x, then the worker dies before the session
  // copy is cleared. On recovery, delivery succeeds and removes x from the
  // queue -- so the queued marker recovery relies on is gone. stillActive
  // ignores the title, so the segment looks live and keeps its spent ID. The
  // next finalization replays x, the daemon keeps the first shorter record, and
  // everything browsed since the title change is lost.
  const recordId = "3f2504e0-4f89-41d3-9a0c-0305e82c3301";
  const startedAt = Date.now() - 600_000;
  const h = chromeHarness({
    session: {
      current: {
        recordId,
        tabId: 10,
        windowId: 1,
        url: "https://example.com/a",
        title: "Docs",
        incognito: false,
        startedAt,
      },
    },
    local: {
      queue: [
        {
          record_id: recordId,
          url: "https://example.com/a",
          title: "Docs",
          started_at: new Date(startedAt).toISOString(),
          ended_at: new Date(startedAt + 60_000).toISOString(),
        },
      ],
    },
  });
  await h.settled();

  assert.equal(h.state.requests.length, 1, "the queued record should have been delivered");
  const current = h.current();
  if (current) {
    assert.notEqual(
      current.recordId,
      recordId,
      "a delivered identity must not keep being timed",
    );
  }
});

// The legacy repair path exists for state an older build wrote. Anything with a
// present-but-wrong field is corrupt, not old, and repairing it would convert
// state explicitly marked private into reportable activity.
test("present-but-invalid state is discarded, not repaired", async () => {
  const base = {
    tabId: 10,
    windowId: 1,
    url: "https://example.com/a",
    title: "Docs",
    startedAt: Date.now() - 600_000,
  };
  const cases = {
    "explicitly private": { ...base, recordId: undefined, incognito: true },
    "private with a valid id": {
      ...base,
      recordId: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
      incognito: true,
    },
    "malformed id, new schema": { ...base, recordId: "not-a-uuid", incognito: false },
    "malformed id and private": { ...base, recordId: "not-a-uuid", incognito: true },
  };

  for (const [name, current] of Object.entries(cases)) {
    // The live tab is present and non-private, which is exactly the condition
    // the upgrade path would have accepted as proof.
    const h = chromeHarness({ respond: () => ({ ok: false, status: 503 }), session: { current } });
    await h.settled();

    assert.equal(h.queue().length, 0, `${name} must not become a record`);

    // A repaired segment keeps its original start time; a discarded one is
    // replaced by a fresh segment for the live tab. The start time is what
    // distinguishes them, and it is what would silently backdate private
    // browsing into the record.
    const now = h.current();
    if (now) {
      assert.notEqual(
        now.startedAt,
        current.startedAt,
        `${name} must not keep the discarded segment's start time`,
      );
      assert.equal(now.incognito, false, `${name} must yield an explicit privacy flag`);
    }
  }
});

// A genuine predecessor segment is still recoverable when the live tab proves it.
test("state from an older build is upgraded when the tab proves it", async () => {
  const startedAt = Date.now() - 600_000;
  const h = chromeHarness({
    respond: () => ({ ok: false, status: 503 }),
    session: {
      current: {
        tabId: 10,
        windowId: 1,
        url: "https://example.com/a",
        title: "Docs",
        startedAt,
      },
    },
  });
  await h.settled();

  const current = h.current();
  assert.ok(current, "a provable legacy segment should survive recovery");
  assert.match(current.recordId, /^[0-9a-f-]{36}$/, "it gained a canonical identity");
  assert.equal(current.incognito, false, "and an explicit privacy flag");
  assert.equal(current.startedAt, startedAt, "keeping its original start");
});

test("a wedged daemon does not stall tracking indefinitely", async () => {
  // Delivery shares the serialization chain with every tracking event, so a
  // daemon that accepts the connection and never answers would hold up
  // activations and focus changes behind it until Chrome killed the worker --
  // taking their in-memory transition times with it.
  const h = chromeHarness();
  await h.settled();
  h.age(600_000);

  const held = h.holdFetch();
  const finalizing = h.fire("windows.onFocusChanged", -1);
  await held.started;

  const signal = h.pendingSignal();
  assert.ok(signal, "delivery must pass an AbortSignal so it can be bounded");
  assert.equal(signal.aborted, false, "the request is still in flight");

  // Fire the deadline the production code scheduled, rather than rejecting the
  // request from the outside: that would pass even with no timer at all.
  assert.ok(h.runTimers() > 0, "delivery must schedule a deadline");
  assert.equal(signal.aborted, true, "the deadline must abort the request");

  await finalizing;
  await h.settled();
  assert.equal(h.queue().length, 1, "an aborted upload must leave the batch queued");

  // A later event still reaches durable state.
  held.resolve();
  h.state.focusedWindowId = 1;
  await h.fire("tabs.onActivated", { tabId: 10, windowId: 1 });
  await h.settled();
  assert.ok(h.current(), "an event after an aborted upload must still update state");
});

test("a startup wake does not run recovery twice", async () => {
  // The entrypoint starts recovery during evaluation, and onStartup is usually
  // the very event that woke the worker. Two recoveries mean two delivery
  // attempts, and against a daemon that never answers that is two consecutive
  // deadlines -- twice the bound the deadline promises.
  const h = chromeHarness({
    respond: () => ({ ok: false, status: 503 }),
    local: {
      queue: [
        {
          record_id: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
          url: "https://example.com/a",
          title: "Docs",
          started_at: new Date(Date.now() - 600_000).toISOString(),
          ended_at: new Date(Date.now() - 540_000).toISOString(),
        },
      ],
    },
  });

  // Fired before the initial recovery settles, which is the real ordering.
  const startup = h.fire("runtime.onStartup");
  await h.settled();
  await startup;
  await h.settled();

  assert.equal(h.state.requests.length, 1, "recovery must coalesce into one delivery attempt");
});
