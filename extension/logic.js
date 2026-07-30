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
      // The daemon preserves a canonical ID so a retry from our durable
      // queue conflicts as a replay instead of inserting twice.
      record_id: validRecordId(current.recordId) ? current.recordId : "",
      url: current.url,
      title: current.title || "",
      started_at: new Date(current.startedAt).toISOString(),
      ended_at: new Date(endedAt).toISOString(),
    };
  }

  // validRecordId matches the daemon's rule exactly: canonical lowercase
  // 36-character UUID text and nothing else. Accepting other spellings
  // would let one segment insert twice.
  const RECORD_ID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

  function validRecordId(id) {
    return typeof id === "string" && RECORD_ID_PATTERN.test(id);
  }

  // The states the popup and options page can report. They are distinct
  // because the recovery each one needs is distinct, and collapsing two of
  // them sends the user to fix the wrong thing.
  const STATUS = {
    NO_TOKEN: "no-token",
    PERMISSION: "permission",
    // Chrome 142 and newer gate loopback requests with Local Network Access
    // on top of the host permission. Once the permission is granted, a
    // request that fails before any HTTP response could be an LNA denial or
    // a stopped daemon, and JavaScript cannot tell them apart -- so this
    // state names both rather than guessing.
    BLOCKED: "blocked",
    TOKEN_REJECTED: "token-rejected",
    UPGRADE: "upgrade",
    HTTP_ERROR: "http-error",
    CONNECTED: "connected",
  };

  // daemonSupportsBrowser requires the exact lowercase producer name. A
  // daemon that predates this route answers without the field, and a
  // differently cased entry is not a match: silently accepting either would
  // let Chrome activity be stored as Firefox.
  function daemonSupportsBrowser(body, kind) {
    if (!body || typeof body !== "object" || body.ok !== true) {
      return false;
    }
    return Array.isArray(body.browsers) && body.browsers.includes(kind);
  }

  // classifyStatus maps one status attempt onto a UI state.
  //
  // Only Chrome demands the capability field. The legacy Firefox route is
  // permanent, so an older daemon serves Firefox correctly and must not be
  // reported as needing an upgrade.
  function classifyStatus({ hasToken, hasPermission, status, body, kind }) {
    if (!hasToken) {
      return STATUS.NO_TOKEN;
    }
    if (!hasPermission) {
      return STATUS.PERMISSION;
    }
    // No status at all means the request never reached an HTTP response.
    if (!Number.isFinite(status)) {
      return STATUS.BLOCKED;
    }
    if (status === 401) {
      return STATUS.TOKEN_REJECTED;
    }
    if (status < 200 || status > 299) {
      return STATUS.HTTP_ERROR;
    }
    if (kind === "chrome" && !daemonSupportsBrowser(body, kind)) {
      return STATUS.UPGRADE;
    }
    return STATUS.CONNECTED;
  }

  // usableSegment decides whether persisted session state may become
  // activity. Session storage survives a service-worker restart, so on
  // recovery it is untrusted input rather than something this code wrote:
  // a partial write, a stale build, or an incognito tab must never be
  // converted into a record.
  //
  // incognito must be exactly false. Absent means an older build wrote
  // it and cannot prove the tab was not private, which the caller
  // resolves by asking the live tab.
  function usableSegment(current, now, ignored = []) {
    if (!current || typeof current !== "object") {
      return false;
    }
    if (!validRecordId(current.recordId)) {
      return false;
    }
    if (current.incognito !== false) {
      return false;
    }
    if (!positiveInteger(current.tabId) || !positiveInteger(current.windowId)) {
      return false;
    }
    if (!Number.isFinite(current.startedAt) || current.startedAt > now) {
      return false;
    }
    if (!isWebUrl(current.url)) {
      return false;
    }
    return !isIgnored(current.url, ignored);
  }

  // legacySegment reports state written before record IDs and the explicit
  // incognito flag existed, and only that.
  //
  // Both fields must be absent, not merely invalid. Treating a present-but-bad
  // value as legacy hands it to the upgrade path, which would overwrite
  // incognito: true with false and mint a fresh ID over a malformed one --
  // turning state explicitly marked private, or partly corrupt state, into
  // ordinary reportable activity. Anything present and wrong is discarded
  // instead.
  function legacySegment(current) {
    if (!current || typeof current !== "object") {
      return false;
    }
    if (current.recordId !== undefined || current.incognito !== undefined) {
      return false;
    }
    // The full predecessor shape, so only what the old build really wrote is
    // eligible for repair.
    return (
      positiveInteger(current.tabId) &&
      positiveInteger(current.windowId) &&
      Number.isFinite(current.startedAt) &&
      isWebUrl(current.url)
    );
  }

  function positiveInteger(value) {
    return Number.isInteger(value) && value > 0;
  }

  // queueHasRecord makes the queue append idempotent. Termination between
  // the queue write and the session clear leaves both copies; recovery
  // must recognize the queued identity rather than append it again.
  function queueHasRecord(queue, recordId) {
    if (!validRecordId(recordId)) {
      return false;
    }
    return (queue || []).some((entry) => entry && entry.record_id === recordId);
  }

  // idleEndedAt is when the user actually stopped, which is one
  // detection interval before the browser said so. Without this the
  // segment would absorb the whole idle period and browser rows would
  // inflate against the desktop tracker's.
  function idleEndedAt(now, idleSeconds = IDLE_SECONDS) {
    return now - idleSeconds * 1000;
  }

  // idleEndsAt picks the end time for an idle-state transition.
  //
  // Only "idle" is backdated. A lock is an instant, deliberate act --
  // the user hit the shortcut now, not five minutes ago -- so
  // backdating it would place the end before the start for any segment
  // younger than the interval, and buildRecord would discard perfectly
  // real browsing.
  function idleEndsAt(state, now, idleSeconds = IDLE_SECONDS) {
    return state === "idle" ? idleEndedAt(now, idleSeconds) : now;
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

  // hostFor returns the comparable hostname of a URL: lowercase,
  // punycode, no port, no trailing DNS dot, no "www." prefix. Returns
  // "" for anything unparseable.
  function hostFor(rawUrl) {
    try {
      return normalizeHost(new URL(rawUrl).hostname);
    } catch (err) {
      return "";
    }
  }

  function normalizeHost(host) {
    return String(host).toLowerCase().replace(/\.$/, "").replace(/^www\./, "");
  }

  // canonicalIgnoreEntry turns one written rule into the form hosts are
  // compared in, or "" if it cannot be one.
  //
  // The rule goes through the URL parser, exactly as a page URL does.
  // Anything else means the two sides normalize differently: a rule
  // written "bücher.de" would never match the hostname the browser
  // reports, which is the punycode "xn--bcher-kva.de". Parsing also
  // makes the forgiving forms work -- a pasted "https://bank.example",
  // a "bank.example:443", a "bank.example/private" -- rather than
  // storing them as rules that silently never match.
  function canonicalIgnoreEntry(line) {
    const trimmed = String(line).trim().replace(/^\*\./, "");
    if (trimmed === "") {
      return "";
    }
    const candidate = /^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed) ? trimmed : `http://${trimmed}`;
    try {
      const { hostname } = new URL(candidate);
      return hostname === "" ? "" : normalizeHost(hostname);
    } catch (err) {
      return "";
    }
  }

  // parseIgnoreList turns the textarea into rules, reporting the lines
  // it could not use rather than storing them as dead entries.
  function parseIgnoreList(text) {
    const patterns = [];
    const invalid = [];

    for (const raw of String(text || "").split("\n")) {
      const line = raw.trim();
      if (line === "" || line.startsWith("#")) {
        continue;
      }
      const host = canonicalIgnoreEntry(line);
      if (host === "") {
        invalid.push(line);
        continue;
      }
      if (!patterns.includes(host)) {
        patterns.push(host);
      }
    }
    return { patterns, invalid };
  }

  // filterIgnored drops queued records whose host is now ignored.
  //
  // Rules added after a record was queued must still apply: the queue
  // is durable, so without this a sensitive page recorded while the
  // daemon was down would be delivered anyway once it came back.
  function filterIgnored(records, patterns) {
    if (!patterns || patterns.length === 0) {
      return records;
    }
    return records.filter((record) => !isIgnored(record.url, patterns));
  }

  // isIgnored reports whether a URL should never be recorded.
  //
  // A pattern covers its subdomains: "gov.sg" ignores
  // "login.id.singpass.gov.sg". That is the expectation people bring
  // from hosts files and ad blockers, and the alternative -- listing
  // every subdomain a bank redirects through -- is unusable.
  function isIgnored(rawUrl, patterns) {
    if (!patterns || patterns.length === 0) {
      return false;
    }
    const host = hostFor(rawUrl);
    if (host === "") {
      return false;
    }
    return patterns.some((pattern) => host === pattern || host.endsWith(`.${pattern}`));
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
    validRecordId,
    STATUS,
    daemonSupportsBrowser,
    classifyStatus,
    usableSegment,
    legacySegment,
    queueHasRecord,
    idleEndedAt,
    idleEndsAt,
    trimQueue,
    recordKey,
    removeDelivered,
    hostFor,
    normalizeHost,
    canonicalIgnoreEntry,
    parseIgnoreList,
    isIgnored,
    filterIgnored,
    normalizeDaemonUrl,
    originFor,
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = logic;
  } else {
    Object.assign(root, logic);
  }
})(globalThis);
