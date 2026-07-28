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

  // The daemon accepts any loopback bind, so the IPv6 form has to
  // produce a pattern the manifest actually declares.
  assert.equal(originFor("http://[::1]:7600"), "http://[::1]/*");

  assert.throws(() => originFor("not a url"));
});

// Whatever originFor produces for a daemon address must be declared in
// optional_host_permissions, or the extension can never request it and
// reporting is blocked with no way to fix it from the UI.
test("declared host permissions cover the addresses the daemon accepts", () => {
  const manifest = require("../manifest.json");
  const declared = new Set(manifest.optional_host_permissions);

  for (const addr of ["http://127.0.0.1:7600", "http://localhost:7600", "http://[::1]:7600"]) {
    assert.ok(
      declared.has(originFor(addr)),
      `${addr} derives ${originFor(addr)}, which the manifest does not declare`,
    );
  }
});

test("removeDelivered keeps records appended during the request", () => {
  const { removeDelivered } = require("../logic.js");

  const delivered = [
    { url: "https://a.example", started_at: "2026-05-04T10:00:00.000Z" },
    { url: "https://b.example", started_at: "2026-05-04T10:01:00.000Z" },
  ];
  // A third record arrived while the first two were in flight.
  const latest = [
    ...delivered,
    { url: "https://c.example", started_at: "2026-05-04T10:02:00.000Z" },
  ];

  const remaining = removeDelivered(latest, delivered);
  assert.equal(remaining.length, 1);
  assert.equal(remaining[0].url, "https://c.example");
});

// The case a count-based slice gets wrong: at the queue limit, the trim
// drops an old record while a new one is appended, so the length never
// changes and slice(delivered.length) would delete the newcomer that
// was never sent.
test("removeDelivered survives a trimmed full queue", () => {
  const { removeDelivered, trimQueue } = require("../logic.js");

  const limit = 5;
  const delivered = Array.from({ length: limit }, (_, i) => ({
    url: `https://example.com/${i}`,
    started_at: new Date(Date.UTC(2026, 4, 4, 10, i)).toISOString(),
  }));

  const newcomer = { url: "https://late.example", started_at: "2026-05-04T11:00:00.000Z" };
  const latest = trimQueue([...delivered, newcomer], limit);

  assert.equal(latest.length, limit, "the trim kept the queue at its limit");

  const remaining = removeDelivered(latest, delivered);
  assert.deepEqual(remaining, [newcomer], "the unsent record survives");
});

test("removeDelivered tolerates duplicates and empty input", () => {
  const { removeDelivered } = require("../logic.js");

  const record = { url: "https://a.example", started_at: "2026-05-04T10:00:00.000Z" };

  assert.deepEqual(removeDelivered([], [record]), []);
  assert.deepEqual(removeDelivered([record], []), [record]);
  assert.deepEqual(removeDelivered([record, record], [record]), []);
});

test("hostFor normalizes the comparable hostname", () => {
  const { hostFor } = require("../logic.js");

  assert.equal(hostFor("https://www.YouTube.com/watch?v=x"), "youtube.com");
  assert.equal(hostFor("https://example.com:8443/a"), "example.com");
  assert.equal(hostFor("https://user:pw@intranet.example.com/a"), "intranet.example.com");
  assert.equal(hostFor("http://[::1]:7600/status"), "[::1]");
  assert.equal(hostFor("not a url"), "");
  assert.equal(hostFor(""), "");
});

test("parseIgnoreList reads the textarea forgivingly", () => {
  const { parseIgnoreList } = require("../logic.js");

  const text = [
    "  Bank.Example  ",
    "",
    "# a note about the next line",
    "*.gov.sg",
    "www.reddit.com",
  ].join("\n");

  assert.deepEqual(parseIgnoreList(text).patterns, ["bank.example", "gov.sg", "reddit.com"]);
  assert.deepEqual(parseIgnoreList("").patterns, []);
  assert.deepEqual(parseIgnoreList(undefined).patterns, []);
});

// A pattern has to cover subdomains, or ignoring a bank means listing
// every host it redirects through.
test("isIgnored matches a host and its subdomains", () => {
  const { isIgnored } = require("../logic.js");

  const patterns = ["gov.sg", "bank.example"];

  assert.equal(isIgnored("https://login.id.singpass.gov.sg/x", patterns), true);
  assert.equal(isIgnored("https://gov.sg/", patterns), true);
  assert.equal(isIgnored("https://www.bank.example/accounts", patterns), true);
  assert.equal(isIgnored("https://BANK.EXAMPLE/accounts", patterns), true);

  assert.equal(isIgnored("https://youtube.com/", patterns), false);
  // A suffix that is not a domain boundary must not match.
  assert.equal(isIgnored("https://notgov.sg/", patterns), false);
  assert.equal(isIgnored("https://mybank.example.org/", patterns), false);

  assert.equal(isIgnored("https://gov.sg/", []), false);
  assert.equal(isIgnored("https://gov.sg/", undefined), false);
  assert.equal(isIgnored("not a url", patterns), false);
});

// Rules and page URLs must normalize identically, or a rule that looks
// right never matches.
test("ignore rules canonicalize through the URL parser", () => {
  const { canonicalIgnoreEntry, hostFor, isIgnored } = require("../logic.js");

  const cases = {
    "bank.example": "bank.example",
    "https://bank.example": "bank.example",
    "bank.example:443": "bank.example",
    "bank.example/private": "bank.example",
    "WWW.Reddit.com": "reddit.com",
    "*.gov.sg": "gov.sg",
    "example.com.": "example.com",
    // The browser reports punycode, so a Unicode rule has to become it.
    "bücher.de": "xn--bcher-kva.de",
    "not a host": "",
  };

  for (const [entry, want] of Object.entries(cases)) {
    assert.equal(canonicalIgnoreEntry(entry), want, entry);
  }

  // The pair that motivated this: both sides land on the same string.
  assert.equal(hostFor("https://bücher.de/x"), canonicalIgnoreEntry("bücher.de"));
  assert.equal(isIgnored("https://bücher.de/x", [canonicalIgnoreEntry("bücher.de")]), true);
  // A trailing DNS dot is the same host.
  assert.equal(isIgnored("https://example.com./x", ["example.com"]), true);
});

test("parseIgnoreList reports lines it cannot use", () => {
  const { parseIgnoreList } = require("../logic.js");

  const { patterns, invalid } = parseIgnoreList(
    ["bank.example", "not a host", "# note", "", "https://reddit.com", "bank.example"].join("\n"),
  );

  assert.deepEqual(patterns, ["bank.example", "reddit.com"], "deduplicated and canonical");
  assert.deepEqual(invalid, ["not a host"], "the unusable line is reported, not stored");
});

test("filterIgnored drops queued records for ignored hosts", () => {
  const { filterIgnored } = require("../logic.js");

  const queue = [
    { url: "https://keep.example/a" },
    { url: "https://login.bank.example/session" },
    { url: "https://keep.example/b" },
  ];

  assert.deepEqual(
    filterIgnored(queue, ["bank.example"]).map((r) => r.url),
    ["https://keep.example/a", "https://keep.example/b"],
  );
  assert.equal(filterIgnored(queue, []).length, 3);
  assert.equal(filterIgnored(queue, undefined).length, 3);
});
