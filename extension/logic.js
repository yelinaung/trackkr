// Pure decision logic for the extension.
//
// These are the rules that decide what gets recorded: which tabs count,
// when a segment ends, and what the daemon is sent. They are kept free
// of browser APIs so they can be tested under node:test with no browser
// at all -- Playwright cannot load Firefox extensions, so without this
// split the rules would only ever be checked by hand.
//
// Loaded as a classic script in the extension and required as a module
// in tests, hence the dual export at the bottom.

(function attach(root) {
  "use strict";

  // MIN_DURATION_MS drops flicks through the tab strip. The daemon
  // enforces the same floor.
  const MIN_DURATION_MS = 1000;

  // QUEUE_LIMIT caps what an offline browser accumulates.
  const QUEUE_LIMIT = 500;

  // IDLE_SECONDS mirrors the daemon's idle_threshold.
  const IDLE_SECONDS = 300;

  const DEFAULT_DAEMON_URL = "http://127.0.0.1:7600";

  function isWebUrl(url) {
    return typeof url === "string" && (url.startsWith("http://") || url.startsWith("https://"));
  }

  // trackable decides whether a tab should be timed at all. Private
  // windows never are: nothing from them is stored or sent.
  function trackable(tab) {
    return Boolean(tab) && tab.incognito !== true && isWebUrl(tab.url);
  }

  // shouldSwitch reports whether a tab event describes a new segment. A
  // tab keeps its id across navigations, so the URL and title are what
  // distinguish one segment from the next.
  function shouldSwitch(current, tab) {
    if (!current || !tab) {
      return true;
    }
    return (
      current.tabId !== tab.id ||
      current.url !== tab.url ||
      current.title !== (tab.title || "")
    );
  }

  // buildRecord closes a segment, returning null for anything that
  // should not be reported.
  function buildRecord(current, endedAt) {
    if (!current || !isWebUrl(current.url)) {
      return null;
    }
    if (!Number.isFinite(current.startedAt) || !Number.isFinite(endedAt)) {
      return null;
    }
    if (endedAt - current.startedAt < MIN_DURATION_MS) {
      return null;
    }
    return {
      url: current.url,
      title: current.title || "",
      started_at: new Date(current.startedAt).toISOString(),
      ended_at: new Date(endedAt).toISOString(),
    };
  }

  // idleEndedAt is when the user actually stopped, which is one
  // detection interval before the browser said so. Without this the
  // segment would absorb the whole idle period and browser rows would
  // inflate against the desktop tracker's.
  function idleEndedAt(now, idleSeconds = IDLE_SECONDS) {
    return now - idleSeconds * 1000;
  }

  // trimQueue keeps the newest records when an offline browser has been
  // accumulating for a long time.
  function trimQueue(queue, limit = QUEUE_LIMIT) {
    return queue.length <= limit ? queue : queue.slice(queue.length - limit);
  }

  // recordKey identifies a record for removal after delivery. The
  // start time is unique per tab segment, and the URL disambiguates
  // two windows starting a segment in the same millisecond.
  function recordKey(record) {
    return `${record.started_at}|${record.url}`;
  }

  // removeDelivered drops exactly the records that were sent, keeping
  // anything appended while the request was in flight.
  //
  // Slicing by count instead would delete the wrong records: if the
  // queue is at its limit and a new record arrives mid-request, the
  // trim drops an old one and the length is unchanged, so a count-based
  // slice removes the newcomer that was never sent.
  function removeDelivered(latest, delivered) {
    const sent = new Set(delivered.map(recordKey));
    return latest.filter((record) => !sent.has(recordKey(record)));
  }

  function normalizeDaemonUrl(raw) {
    return String(raw || DEFAULT_DAEMON_URL).trim().replace(/\/+$/, "");
  }

  // originFor turns a daemon URL into the origin pattern a host
  // permission is granted against. The port is deliberately dropped:
  // permissions are granted per origin, and the port is configurable.
  function originFor(daemonUrl) {
    const url = new URL(normalizeDaemonUrl(daemonUrl));
    return `${url.protocol}//${url.hostname}/*`;
  }

  const logic = {
    MIN_DURATION_MS,
    QUEUE_LIMIT,
    IDLE_SECONDS,
    DEFAULT_DAEMON_URL,
    isWebUrl,
    trackable,
    shouldSwitch,
    buildRecord,
    idleEndedAt,
    trimQueue,
    recordKey,
    removeDelivered,
    normalizeDaemonUrl,
    originFor,
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = logic;
  } else {
    Object.assign(root, logic);
  }
})(globalThis);
