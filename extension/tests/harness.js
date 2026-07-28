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
  } = options;

  const state = {
    session: {},
    local: { token, daemonUrl: "http://127.0.0.1:7600", ignored: [] },
    tabs: { ...tabs },
    windows: { ...windows },
    focusedWindowId,
    idleState,
    permitted,
    requests: [],
    writes: [],
    // Set by a test to hold a fetch open.
    pendingFetch: null,
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
          return key in state.session ? { [key]: state.session[key] } : {};
        },
        async set(entries) {
          Object.assign(state.session, entries);
        },
        async remove(key) {
          delete state.session[key];
        },
      },
      local: {
        async get(keys) {
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

    runtime: {
      onSuspend: event("runtime.onSuspend"),
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
    state.requests.push({ url, init, body: JSON.parse(init.body) });
    if (state.pendingFetch) {
      // Tell the test the request is genuinely in flight before it
      // starts interfering; appending earlier would race the handler
      // rather than the delivery.
      state.pendingFetch.started();
      await state.pendingFetch.promise;
    }
    const result = respond(state.requests.length);
    if (result instanceof Error) {
      throw result;
    }
    return { ok: result.ok, status: result.status };
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
    setTimeout,
    clearTimeout,
    browser: api,
    fetch: fetchImpl,
  });

  // Manifest order, so the same globals exist in the same sequence.
  for (const file of ["logic.js", "common.js", "background.js"]) {
    vm.runInContext(fs.readFileSync(path.join(EXTENSION_DIR, file), "utf8"), context, {
      filename: file,
    });
  }

  return {
    state,
    context,

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
      await context.chain;
      await context.chain;
    },

    current() {
      return state.session.current || null;
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
