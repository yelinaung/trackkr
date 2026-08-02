"use strict";

// A fake browser for background.js.
//
// The logic tests cover the decision rules, but every bug found in
// review so far has been in the plumbing: event ordering, focus
// scoping, suspension, concurrent delivery. Those need the real script
// running against something that can be made to misbehave on purpose --
// deferring a storage write, holding a fetch open, firing two events
// before either finishes.
//
// The extension's scripts are loaded into a vm context in manifest
// order, so this exercises the same globals the browser would provide.

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const EXTENSION_DIR = path.join(__dirname, "..");

// deferred returns a promise plus the handle to settle it later, which
// is how a test holds one operation open while firing another event.
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function createHarness(options = {}) {
  const {
    tabs = {},
    windows = {},
    focusedWindowId = 1,
    idleState = "active",
    token = "test-token",
    permitted = true,
    respond = () => ({ ok: true, status: 202 }),
    // Which background entrypoint to load. Firefox is the default because it
    // is the only build that registers runtime.onSuspend.
    entrypoint = "background-fx.js",
    session = {},
    local = {},
  } = options;

  const state = {
    // Seeded before the scripts load, so a test can reproduce a cold wake:
    // storage.session survives worker termination, the worker does not.
    session: { ...session },
    local: { token, daemonUrl: "http://127.0.0.1:7600", ignored: [], ...local },
    tabs: { ...tabs },
    windows: { ...windows },
    focusedWindowId,
    // reads counts storage reads so a test can prove registration finished
    // before recovery began.
    reads: 0,
    failSessionRemove: false,
    timers: [],
    idleState,
    permitted,
    requests: [],
    writes: [],
    // Set by a test to hold a fetch open.
    pendingFetch: null,
    // Alarms the worker has asked for, by name.
    alarms: new Map(),
    alarmCreates: 0,
  };

  const listeners = new Map();

  // Promises from listeners the fake invokes on its own (storage
  // changes). A test cannot await those through fire(), and the
  // background script's chain is a `let` binding, which a vm script
  // does not expose on the context object.
  const pending = [];

  function event(name) {
    return {
      addListener(fn) {
        listeners.set(name, fn);
      },
    };
  }

  const api = {
    storage: {
      session: {
        async get(key) {
          state.reads += 1;
          return key in state.session ? { [key]: state.session[key] } : {};
        },
        async set(entries) {
          Object.assign(state.session, entries);
        },
        async remove(key) {
          // A deterministic fault stands in for a worker killed after the
          // durable queue write. It proves the write ordering without
          // pretending the harness can force-kill real in-flight work.
          if (state.failSessionRemove) {
            throw new Error("session storage unavailable");
          }
          delete state.session[key];
        },
      },
      local: {
        async get(keys) {
          state.reads += 1;
          const wanted = Array.isArray(keys) ? keys : [keys];
          const out = {};
          for (const key of wanted) {
            if (key in state.local) {
              out[key] = state.local[key];
            }
          }
          return out;
        },
        async set(entries) {
          // Every write is logged so a test can assert what was
          // persisted, not merely what survived. An ignored URL must
          // never reach storage at all, even for an instant.
          state.writes.push(JSON.parse(JSON.stringify(entries)));

          const changes = {};
          for (const [key, newValue] of Object.entries(entries)) {
            changes[key] = { oldValue: state.local[key], newValue };
          }
          Object.assign(state.local, entries);

          // Real storage notifies listeners without awaiting them.
          // Awaiting here deadlocks: a handler already inside the
          // serialization chain writes storage, the listener appends to
          // that same chain, and the write waits on itself.
          const listener = listeners.get("storage.onChanged");
          if (listener) {
            pending.push(Promise.resolve(listener(changes, "local")));
          }
        },
      },
      onChanged: event("storage.onChanged"),
    },

    tabs: {
      onActivated: event("tabs.onActivated"),
      onUpdated: event("tabs.onUpdated"),
      onRemoved: event("tabs.onRemoved"),
      async get(tabId) {
        const tab = state.tabs[tabId];
        if (!tab) {
          throw new Error(`no such tab: ${tabId}`);
        }
        return tab;
      },
      async query({ windowId }) {
        return Object.values(state.tabs).filter((t) => t.windowId === windowId && t.active);
      },
    },

    windows: {
      WINDOW_ID_NONE: -1,
      onFocusChanged: event("windows.onFocusChanged"),
      async get(windowId) {
        const win = state.windows[windowId];
        if (!win) {
          throw new Error(`no such window: ${windowId}`);
        }
        return { ...win, focused: state.focusedWindowId === windowId };
      },
      async getLastFocused() {
        const id = state.lastFocusedWindowId ?? focusedWindowId;
        return { id, focused: state.focusedWindowId === id };
      },
    },

    idle: {
      onStateChanged: event("idle.onStateChanged"),
      setDetectionInterval() {},
      async queryState() {
        return state.idleState;
      },
    },

    // Alarms rather than timers, because only alarms survive Chrome
    // evicting the worker. The fake keeps them in a map so a test can
    // see one created, see it cleared, and fire it by name.
    alarms: {
      onAlarm: event("alarms.onAlarm"),
      async create(name, info) {
        // Counted, because creating over an existing name cancels and
        // replaces it. A test needs to see that happen, and the stored
        // value alone looks identical either way.
        state.alarmCreates += 1;
        state.alarms.set(name, info);
      },
      async get(name) {
        return state.alarms.has(name) ? { name, ...state.alarms.get(name) } : undefined;
      },
      async clear(name) {
        return state.alarms.delete(name);
      },
    },

    runtime: {
      // Chrome MV3 has no onSuspend. Omitting it is the whole point: the
      // Chrome entrypoint must start without touching a property that does
      // not exist, and a harness that provided one could not prove that.
      ...(entrypoint === "background-cr.js" ? {} : { onSuspend: event("runtime.onSuspend") }),
      onStartup: event("runtime.onStartup"),
      onInstalled: event("runtime.onInstalled"),
    },

    permissions: {
      async contains() {
        return state.permitted;
      },
    },
  };

  async function fetchImpl(url, init) {
    // The idle poll is a body-less GET, so parsing unconditionally would
    // throw before the request was ever recorded.
    const body = init && typeof init.body === "string" ? JSON.parse(init.body) : null;
    state.requests.push({ url, init, body });
    state.pendingSignal = init && init.signal ? init.signal : null;
    if (state.pendingFetch) {
      // Tell the test the request is genuinely in flight before it
      // starts interfering; appending earlier would race the handler
      // rather than the delivery.
      state.pendingFetch.started();
      await Promise.race([
        state.pendingFetch.promise,
        new Promise((_, reject) => {
          if (!init || !init.signal) {
            return;
          }
          init.signal.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        }),
      ]);
    }
    const result = respond(state.requests.length);
    if (result instanceof Error) {
      throw result;
    }
    return {
      ok: result.ok,
      status: result.status,
      // Only the idle route reads a body back. Responses without one
      // reject, matching a daemon that answered with nothing parseable.
      async json() {
        if (result.json === undefined) {
          throw new SyntaxError("no JSON body");
        }
        return result.json;
      },
    };
  }

  const context = vm.createContext({
    console,
    URL,
    Date,
    Set,
    JSON,
    Promise,
    Number,
    Array,
    Object,
    String,
    Boolean,
    // Captured rather than real, so a test can fire the delivery deadline the
    // production code actually scheduled instead of reaching past it to reject
    // the request directly.
    setTimeout: (fn, delay) => {
      const handle = { fn, delay, cancelled: false };
      state.timers.push(handle);
      return handle;
    },
    clearTimeout: (handle) => {
      if (handle) {
        handle.cancelled = true;
      }
    },
    AbortController,
    DOMException,
    ...(entrypoint === "background-cr.js" ? { chrome: api } : { browser: api }),
    crypto: {
      // Deterministic and canonical: recovery and queue idempotence are
      // asserted by identity, so the tests need to be able to name one.
      randomUUID: () => {
        state.uuidCounter = (state.uuidCounter || 0) + 1;
        const n = String(state.uuidCounter).padStart(12, "0");
        return `00000000-0000-4000-8000-${n}`;
      },
    },
    fetch: fetchImpl,
  });

  // Chrome's worker loads these through importScripts; Firefox loads them as
  // ordered classic scripts. Same files, same order, so the shim is enough to
  // run either entrypoint against one fake browser.
  const load = (file) => {
    vm.runInContext(fs.readFileSync(path.join(EXTENSION_DIR, file), "utf8"), context, {
      filename: file,
    });
  };
  context.importScripts = (...files) => files.forEach(load);

  // Each browser is loaded the way it really loads. Firefox's manifest lists
  // four ordered classic scripts; Chrome's worker is one file that pulls in the
  // rest through importScripts, so preloading them here would double-declare
  // every const and is not what Chrome does.
  if (entrypoint === "background-cr.js") {
    load(entrypoint);
  } else {
    load("logic.js");
    load("common.js");
    load("background-core.js");
    load(entrypoint);
  }

  return {
    state,
    context,
    api,

    // fire invokes a listener the way the browser would, returning the
    // promise the handler chains onto so a test can await settlement.
    fire(name, ...args) {
      const listener = listeners.get(name);
      if (!listener) {
        throw new Error(`no listener registered for ${name}`);
      }
      return listener(...args);
    },

    // settled waits for everything in flight: handlers the test fired,
    // and listeners the fake invoked itself.
    async settled() {
      while (pending.length > 0) {
        await pending.shift();
      }
      // pendingWork reaches the real chain; `chain` itself is a lexical
      // binding the context does not expose. Twice, because settling one
      // link can enqueue the next.
      await context.pendingWork();
      await context.pendingWork();
    },

    current() {
      return state.session.current || null;
    },

    // alarm reports what the worker asked for under a name, so a test
    // can assert one exists after a segment opens and is gone after it
    // closes.
    alarm(name) {
      return state.alarms.get(name) || null;
    },

    // dropAlarm reproduces Chrome losing an alarm across a worker
    // restart, which it does not promise to preserve before 150.
    dropAlarm(name) {
      state.alarms.delete(name);
    },

    alarmCreates() {
      return state.alarmCreates;
    },

    idleSource() {
      return state.session.idleSource || null;
    },

    // registered reports whether a listener exists, without firing it.
    registered(name) {
      return listeners.has(name);
    },

    // age backdates the current segment so it clears the one-second floor
    // without any test having to sleep.
    age(ms) {
      if (state.session.current) {
        state.session.current.startedAt -= ms;
      }
    },

    setCurrent(current) {
      state.session.current = current;
    },

    setQueue(queue) {
      state.local.queue = queue;
    },

    closeTab(tabId) {
      delete state.tabs[tabId];
    },

    failSessionRemove() {
      state.failSessionRemove = true;
    },

    // runTimers fires every scheduled, uncancelled callback -- the production
    // deadline included -- without waiting for wall-clock time. Returns how
    // many ran, so a test can assert one was actually scheduled.
    runTimers() {
      const due = state.timers.filter((timer) => !timer.cancelled);
      state.timers = [];
      for (const timer of due) {
        timer.cancelled = true;
        timer.fn();
      }
      return due.length;
    },

    // The AbortSignal of the request currently in flight, so a test can assert
    // the deadline actually aborted it rather than that something rejected.
    pendingSignal() {
      return state.pendingSignal || null;
    },

    queue() {
      return state.local.queue || [];
    },

    // holdFetch pauses the next delivery mid-request. `started`
    // resolves once the request has actually been issued.
    holdFetch() {
      const release = deferred();
      const entered = deferred();
      state.pendingFetch = {
        promise: release.promise,
        started: entered.resolve,
      };
      return {
        started: entered.promise,
        resolve: release.resolve,
      };
    },
  };
}

module.exports = { createHarness, deferred };
