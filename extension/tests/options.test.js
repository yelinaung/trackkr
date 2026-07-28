"use strict";

// The options and popup pages, driven through a fake DOM.
//
// One thing here is worth a test on its own: permissions.request() only
// prompts while the user gesture that triggered it is still in scope.
// Awaiting anything first -- even a storage write -- consumes the
// gesture, and the request then resolves false without ever showing a
// prompt. That failure is indistinguishable from the user declining,
// which is exactly how it went unnoticed until someone clicked Save.

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const EXTENSION_DIR = path.join(__dirname, "..");

function createPage(script, { elements, storage = {}, granted = true }) {
  const calls = [];
  const handlers = new Map();

  const nodes = {};
  for (const [id, props] of Object.entries(elements)) {
    nodes[id] = {
      ...props,
      addEventListener(type, fn) {
        handlers.set(`${id}:${type}`, fn);
      },
    };
  }

  const api = {
    storage: {
      local: {
        async get(keys) {
          calls.push("storage.get");
          const wanted = Array.isArray(keys) ? keys : [keys];
          const out = {};
          for (const key of wanted) {
            if (key in storage) {
              out[key] = storage[key];
            }
          }
          return out;
        },
        async set(entries) {
          calls.push("storage.set");
          Object.assign(storage, entries);
        },
      },
    },
    permissions: {
      async request() {
        calls.push("permissions.request");
        return granted;
      },
      async contains() {
        return granted;
      },
    },
    runtime: { openOptionsPage() {} },
  };

  const context = vm.createContext({
    console,
    URL,
    Date,
    JSON,
    Promise,
    Object,
    Array,
    String,
    Boolean,
    Number,
    Set,
    setTimeout,
    browser: api,
    fetch: async () => {
      calls.push("fetch");
      return { ok: true, status: 200 };
    },
    document: {
      getElementById(id) {
        return nodes[id] || null;
      },
    },
  });

  for (const file of ["logic.js", "common.js", script]) {
    vm.runInContext(fs.readFileSync(path.join(EXTENSION_DIR, file), "utf8"), context, {
      filename: file,
    });
  }

  return {
    calls,
    storage,
    nodes,
    fire(id, type, event = { preventDefault() {} }) {
      const handler = handlers.get(`${id}:${type}`);
      if (!handler) {
        throw new Error(`no ${type} handler on #${id}`);
      }
      return handler(event);
    },
  };
}

function optionsPage(options = {}) {
  return createPage("options.js", {
    elements: {
      form: {},
      daemonUrl: { value: "http://127.0.0.1:7600" },
      token: { value: "a-token" },
      status: { className: "", textContent: "" },
    },
    ...options,
  });
}

test("saving requests the permission before awaiting anything", async () => {
  const page = optionsPage();

  await page.fire("form", "submit");

  const requested = page.calls.indexOf("permissions.request");
  const stored = page.calls.indexOf("storage.set");

  assert.notEqual(requested, -1, "no permission was requested");
  assert.ok(
    requested < stored,
    `permissions.request must come first, got: ${page.calls.join(" -> ")}`,
  );
});

test("saving stores the normalized URL and token", async () => {
  const page = optionsPage({
    elements: {
      form: {},
      daemonUrl: { value: "  http://127.0.0.1:7600///  " },
      token: { value: "  a-token  " },
      status: { className: "", textContent: "" },
    },
  });

  await page.fire("form", "submit");

  assert.equal(page.storage.daemonUrl, "http://127.0.0.1:7600");
  assert.equal(page.storage.token, "a-token");
});

test("a declined permission is reported rather than looking saved", async () => {
  const page = optionsPage({ granted: false });

  await page.fire("form", "submit");

  assert.match(page.nodes.status.textContent, /blocking/);
  assert.equal(page.nodes.status.className, "status status--error");
});

test("an unparseable URL is rejected before storing anything", async () => {
  const page = optionsPage({
    elements: {
      form: {},
      daemonUrl: { value: "not a url" },
      token: { value: "a-token" },
      status: { className: "", textContent: "" },
    },
  });

  await page.fire("form", "submit");

  assert.equal(page.calls.includes("storage.set"), false);
  assert.match(page.nodes.status.textContent, /URL/);
});

function popupPage(options = {}) {
  return createPage("popup.js", {
    elements: {
      state: { className: "", textContent: "" },
      detail: { textContent: "" },
      grant: { hidden: true },
      queue: { textContent: "" },
      options: {},
    },
    storage: { token: "a-token", daemonUrl: "http://127.0.0.1:7600" },
    ...options,
  });
}

test("the popup grant button does not await before requesting", async () => {
  const page = popupPage({ granted: false });

  // Let the initial refresh settle so the button has settings cached.
  await new Promise((resolve) => setTimeout(resolve, 0));
  page.calls.length = 0;

  await page.fire("grant", "click");

  assert.equal(
    page.calls[0],
    "permissions.request",
    `the gesture must not be spent first, got: ${page.calls.join(" -> ")}`,
  );
});

test("the popup offers the grant button only for a missing permission", async () => {
  const blocked = popupPage({ granted: false });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(blocked.nodes.grant.hidden, false, "the button is offered");
  assert.match(blocked.nodes.state.textContent, /Permission not granted/);

  const ok = popupPage({ granted: true });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(ok.nodes.grant.hidden, true, "no button once granted");
});
