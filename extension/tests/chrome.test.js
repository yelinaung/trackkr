"use strict";

const test = require("node:test");
const assert = require("node:assert");
const { createHarness } = require("./harness");

const CHROME = { entrypoint: "background-cr.js" };

function chromeHarness(extra = {}) {
  return createHarness({
    ...CHROME,
    tabs: {
      10: { id: 10, windowId: 1, active: true, url: "https://example.com/a", title: "Docs" },
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

  assert.equal(h.state.reads, 0, "recovery read storage before registration finished");
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

  assert.equal(h.queue().length, 1, "recovery appended the same record a second time");
  assert.equal(h.current(), null, "the reconciled session copy was not cleared");
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

    assert.equal(h.queue().length, 0, `${name} was converted into a record`);
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
  assert.equal(h.queue().length, 1, "the second finalize duplicated the record");
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
  assert.deepEqual([...manifest.permissions].sort(), ["idle", "storage", "tabs"]);

  for (const key of ["browser_specific_settings", "data_collection_permissions"]) {
    assert.equal(key in manifest, false, `${key} is Gecko-only`);
  }
});
