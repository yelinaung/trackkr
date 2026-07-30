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

function createPage(script, {
  elements,
  storage = {},
  granted = true,
  kind = "firefox",
  // Off by default so a case that starts ungranted stays that way: several
  // tests are about a declined request.
  grantOnRequest = false,
  status = 200,
  body = { ok: true, browsers: ["firefox", "chrome"] },
  // jsonFails is separate from body because passing body: undefined would
  // select the default above rather than mean "no parseable JSON".
  jsonFails = false,
  fetchFails = false,
}) {
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
        // A real grant changes what contains() reports, which is what lets a
        // test observe the probe that must follow it.
        if (grantOnRequest) {
          granted = true;
        }
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
    ...(kind === "chrome" ? { chrome: api } : { browser: api }),
    fetch: async () => {
      calls.push("fetch");
      if (fetchFails) {
        // No HTTP response at all: an LNA denial or a stopped daemon.
        throw new TypeError("Failed to fetch");
      }
      return {
        ok: status >= 200 && status <= 299,
        status,
        async json() {
          if (jsonFails) {
            throw new SyntaxError("not json");
          }
          return body;
        },
      };
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
      ignored: { value: "" },
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
      ignored: { value: "" },
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
      ignored: { value: "" },
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
      retry: { hidden: true },
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

test("the ignore list is parsed on save and restored on load", async () => {
  const page = optionsPage({
    elements: {
      form: {},
      daemonUrl: { value: "http://127.0.0.1:7600" },
      token: { value: "a-token" },
      ignored: { value: "  Bank.Example \n\n# a note\n*.gov.sg\n" },
      status: { className: "", textContent: "" },
    },
  });

  await page.fire("form", "submit");

  // Array.from crosses the vm realm boundary: the page builds its
  // array with the context's own Array, which deepStrictEqual treats
  // as a different type despite identical contents.
  assert.deepEqual(Array.from(page.storage.ignored), ["bank.example", "gov.sg"]);

  // A second page load shows the cleaned-up list back to the user.
  const reopened = optionsPage({ storage: page.storage });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(reopened.nodes.ignored.value, "bank.example\ngov.sg");
});

// Each state exists because its fix is different. Collapsing any two sends the
// user to change the wrong thing.
test("the popup separates permission, network, token, and capability failures", async () => {
  const cases = [
    {
      name: "host permission missing",
      options: { granted: false },
      expect: /Permission not granted/,
    },
    {
      name: "blocked before any response",
      options: { kind: "chrome", fetchFails: true },
      expect: /Local network access blocked or daemon unavailable/,
    },
    {
      name: "token rejected",
      options: { status: 401 },
      expect: /Token rejected/,
    },
    {
      name: "daemon without Chrome support",
      options: { kind: "chrome", body: { ok: true, browsers: ["firefox"] } },
      expect: /Daemon upgrade required/,
    },
    {
      name: "connected",
      options: { kind: "chrome" },
      expect: /Connected/,
    },
  ];

  for (const { name, options, expect } of cases) {
    const page = popupPage(options);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.match(page.nodes.state.textContent, expect, name);
  }
});

// An old daemon serves Firefox correctly through the permanent legacy route, so
// a missing capability field must not be reported as needing an upgrade.
test("a daemon without the capability field still connects Firefox", async () => {
  const page = popupPage({ kind: "firefox", body: { ok: true } });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.match(page.nodes.state.textContent, /Connected/);
});

test("Chrome requires ok, an array, and the exact lowercase name", async () => {
  const page = popupPage({ kind: "chrome", jsonFails: true });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.match(page.nodes.state.textContent, /Daemon upgrade required/, "unparseable JSON");

  for (const body of [
    {},
    { ok: false, browsers: ["chrome"] },
    { ok: true, browsers: "chrome" },
    { ok: true, browsers: ["Chrome"] },
    { ok: true, browsers: ["CHROME"] },
    { ok: true, browsers: [] },
  ]) {
    const page = popupPage({ kind: "chrome", body });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.match(
      page.nodes.state.textContent,
      /Daemon upgrade required/,
      `body ${JSON.stringify(body)} was accepted as connected`,
    );
  }
});

test("the popup offers Retry only when the network failed", async () => {
  const blocked = popupPage({ kind: "chrome", fetchFails: true });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(blocked.nodes.retry.hidden, false, "a blocked request needs a Retry action");
  assert.equal(blocked.nodes.grant.hidden, true, "the permission is already granted");

  const connected = popupPage({ kind: "chrome" });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(connected.nodes.retry.hidden, true);
});

// Under Chrome the foreground document is what raises the Local Network Access
// prompt; a service-worker request stays blocked. So the grant must be followed
// immediately by a probe from this page.
test("granting the permission is followed by a foreground status request", async () => {
  const page = popupPage({ kind: "chrome", granted: false, grantOnRequest: true });
  await new Promise((resolve) => setTimeout(resolve, 0));

  const before = page.calls.filter((call) => call === "fetch").length;
  await page.fire("grant", "click");
  await new Promise((resolve) => setTimeout(resolve, 0));

  const after = page.calls.filter((call) => call === "fetch").length;
  assert.ok(after > before, "no status request followed the grant");
  assert.ok(
    page.calls.indexOf("permissions.request") < page.calls.lastIndexOf("fetch"),
    "the probe must come after the request, from the foreground document",
  );
});
