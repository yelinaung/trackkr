"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const {
  MIN_DURATION_MS,
  IDLE_SECONDS,
  isWebUrl,
  trackable,
  shouldSwitch,
  buildRecord,
  idleEndedAt,
  trimQueue,
  normalizeDaemonUrl,
  originFor,
} = require("../logic.js");

test("isWebUrl accepts only http and https", () => {
  for (const url of ["http://example.com", "https://example.com/a?b=c"]) {
    assert.equal(isWebUrl(url), true, url);
  }
  for (const url of [
    "about:config",
    "about:blank",
    "file:///home/ye/notes.txt",
    "moz-extension://abc/options.html",
    "view-source:https://example.com",
    "javascript:alert(1)",
    "",
    undefined,
    null,
    42,
  ]) {
    assert.equal(isWebUrl(url), false, String(url));
  }
});

test("trackable skips private windows", () => {
  const base = { id: 1, url: "https://example.com", title: "t" };

  assert.equal(trackable({ ...base }), true);
  assert.equal(trackable({ ...base, incognito: false }), true);
  // Nothing from a private window is stored or sent.
  assert.equal(trackable({ ...base, incognito: true }), false);
  assert.equal(trackable({ ...base, url: "about:blank" }), false);
  assert.equal(trackable(null), false);
  assert.equal(trackable(undefined), false);
});

test("shouldSwitch detects a new segment", () => {
  const current = { tabId: 7, url: "https://example.com/a", title: "A" };

  // Same tab, same page: the event is noise.
  assert.equal(shouldSwitch(current, { id: 7, url: "https://example.com/a", title: "A" }), false);

  // A tab keeps its id across navigations, so the URL is what changed.
  assert.equal(shouldSwitch(current, { id: 7, url: "https://example.com/b", title: "A" }), true);
  // Single-page apps change the title without navigating.
  assert.equal(shouldSwitch(current, { id: 7, url: "https://example.com/a", title: "B" }), true);
  assert.equal(shouldSwitch(current, { id: 8, url: "https://example.com/a", title: "A" }), true);

  // A missing title is an empty title, not a change.
  assert.equal(
    shouldSwitch({ tabId: 7, url: "https://example.com/a", title: "" }, { id: 7, url: "https://example.com/a" }),
    false,
  );

  assert.equal(shouldSwitch(null, { id: 7, url: "https://example.com/a", title: "A" }), true);
});

test("buildRecord enforces the minimum duration", () => {
  const startedAt = Date.UTC(2026, 4, 4, 10, 0, 0);
  const current = { tabId: 1, url: "https://example.com/a", title: "Docs", startedAt };

  assert.equal(buildRecord(current, startedAt + MIN_DURATION_MS - 1), null);
  assert.notEqual(buildRecord(current, startedAt + MIN_DURATION_MS), null);
  assert.equal(buildRecord(current, startedAt), null, "zero length");
  assert.equal(buildRecord(current, startedAt - 5000), null, "negative");
});

test("buildRecord emits the shape the daemon expects", () => {
  const startedAt = Date.UTC(2026, 4, 4, 10, 0, 0);
  const record = buildRecord(
    { tabId: 1, url: "https://example.com/a", title: "Docs", startedAt },
    startedAt + 90_000,
  );

  assert.deepEqual(record, {
    url: "https://example.com/a",
    title: "Docs",
    started_at: "2026-05-04T10:00:00.000Z",
    ended_at: "2026-05-04T10:01:30.000Z",
  });
  // The daemon computes duration itself; sending one would be ignored.
  assert.equal("duration_s" in record, false);
});

test("buildRecord drops segments that should never be reported", () => {
  const startedAt = Date.UTC(2026, 4, 4, 10, 0, 0);
  const later = startedAt + 60_000;

  assert.equal(buildRecord(null, later), null);
  assert.equal(buildRecord({ url: "about:config", startedAt }, later), null);
  assert.equal(buildRecord({ url: "https://example.com", startedAt: undefined }, later), null);
  assert.equal(buildRecord({ url: "https://example.com", startedAt }, undefined), null);
  assert.equal(buildRecord({ url: "https://example.com", startedAt: NaN }, later), null);
});

test("buildRecord treats a missing title as empty", () => {
  const startedAt = Date.UTC(2026, 4, 4, 10, 0, 0);
  const record = buildRecord({ url: "https://example.com/a", startedAt }, startedAt + 60_000);

  assert.equal(record.title, "");
});

test("idleEndedAt backdates to when the user stopped", () => {
  const now = Date.UTC(2026, 4, 4, 12, 0, 0);

  // The browser reports idleness one interval after it began, so a
  // segment must not absorb the whole idle period.
  assert.equal(idleEndedAt(now), now - IDLE_SECONDS * 1000);
  assert.equal(idleEndedAt(now, 60), now - 60_000);

  // A tab focused for less than the interval yields nothing, rather
  // than a negative-length record.
  const startedAt = now - 30_000;
  assert.equal(buildRecord({ url: "https://example.com", startedAt }, idleEndedAt(now)), null);
});

test("trimQueue keeps the newest records", () => {
  const queue = Array.from({ length: 10 }, (_, i) => ({ n: i }));

  assert.equal(trimQueue(queue, 10).length, 10);
  assert.equal(trimQueue(queue, 20).length, 10);

  const trimmed = trimQueue(queue, 4);
  assert.equal(trimmed.length, 4);
  assert.deepEqual(
    trimmed.map((r) => r.n),
    [6, 7, 8, 9],
    "the oldest records are the ones dropped",
  );
});

test("normalizeDaemonUrl strips trailing slashes", () => {
  assert.equal(normalizeDaemonUrl("http://127.0.0.1:7600/"), "http://127.0.0.1:7600");
  assert.equal(normalizeDaemonUrl("http://127.0.0.1:7600///"), "http://127.0.0.1:7600");
  assert.equal(normalizeDaemonUrl("  http://127.0.0.1:7600  "), "http://127.0.0.1:7600");
  assert.equal(normalizeDaemonUrl(""), "http://127.0.0.1:7600", "falls back to the default");
});

test("originFor drops the port", () => {
  // Host permissions are granted per origin, and the daemon port is
  // configurable; a pattern carrying the port would not match.
  assert.equal(originFor("http://127.0.0.1:7600"), "http://127.0.0.1/*");
  assert.equal(originFor("http://127.0.0.1:9999/"), "http://127.0.0.1/*");
  assert.equal(originFor("http://localhost:7600"), "http://localhost/*");

  assert.throws(() => originFor("not a url"));
});
