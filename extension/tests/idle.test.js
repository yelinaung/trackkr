"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { IDLE_SOURCE, readIdleReply } = require("../logic.js");

const NOW = Date.parse("2026-08-02T05:00:00Z");
const STOPPED = "2026-08-02T04:48:02Z";

test("a daemon reporting an active user leaves the segment open", () => {
  const got = readIdleReply(200, { idle: false, threshold_s: 300 }, NOW);
  assert.deepEqual(got, { usable: true, endsAt: null });
});

test("a daemon reporting idle returns the moment activity stopped", () => {
  const got = readIdleReply(200, { idle: true, idle_since: STOPPED }, NOW);
  assert.equal(got.usable, true);
  assert.equal(got.endsAt, Date.parse(STOPPED));
});

// The bug this phase removes, in miniature. The daemon answers with a
// timestamp rather than a boolean precisely so a late poll still closes
// the segment where the user actually stopped. A test that polls
// immediately would pass with either shape.
test("the close time does not depend on when the poll happened", () => {
  const promptly = readIdleReply(200, { idle: true, idle_since: STOPPED }, Date.parse("2026-08-02T04:54:00Z"));
  const late = readIdleReply(200, { idle: true, idle_since: STOPPED }, Date.parse("2026-08-02T05:26:00Z"));

  assert.equal(promptly.endsAt, Date.parse(STOPPED));
  assert.equal(late.endsAt, promptly.endsAt);
});

test("an idle_since in the future is unusable", () => {
  // A forward-skewed clock would end the segment before it started, and
  // buildRecord would discard the real browsing with it.
  const got = readIdleReply(200, { idle: true, idle_since: "2026-08-02T06:00:00Z" }, NOW);
  assert.deepEqual(got, { usable: false, endsAt: null });
});

test("an unparseable idle_since is unusable", () => {
  const got = readIdleReply(200, { idle: true, idle_since: "yesterday" }, NOW);
  assert.deepEqual(got, { usable: false, endsAt: null });
});

test("non-2xx replies are unusable", () => {
  for (const status of [401, 404, 500, 503]) {
    assert.deepEqual(
      readIdleReply(status, { idle: true, idle_since: STOPPED }, NOW),
      { usable: false, endsAt: null },
      `status ${status}`,
    );
  }
});

test("a missing or malformed body is unusable rather than throwing", () => {
  for (const body of [null, undefined, "not an object", 42]) {
    assert.deepEqual(
      readIdleReply(200, body, NOW),
      { usable: false, endsAt: null },
      `body ${JSON.stringify(body)}`,
    );
  }
});

test("a request that never got a status is unusable", () => {
  assert.deepEqual(readIdleReply(NaN, null, NOW), { usable: false, endsAt: null });
});

test("the two idle sources are named", () => {
  assert.equal(IDLE_SOURCE.DAEMON, "daemon");
  assert.equal(IDLE_SOURCE.BROWSER, "browser");
});
