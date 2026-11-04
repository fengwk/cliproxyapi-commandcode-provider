"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./ui.js");

function storage(seed = {}) {
  const values = new Map(Object.entries(seed));
  return {
    getItem: (key) => values.get(key) || null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
    values
  };
}

// The sole persistence exception is a management login in this plugin/prefix.
test("management credentials remember only their own namespace and clear both stores", () => {
  const namespace = ui.PLUGIN_ID + ".management-key:/proxy/keys";
  const win = { localStorage: storage({ "host-credential": "untouched" }), sessionStorage: storage() };
  const login = ui.managementCredentials(win, namespace);
  assert.equal(login.read(), null);
  assert.equal(login.save("synthetic-management-key", true).warning, "");
  assert.deepEqual(login.read(), { key: "synthetic-management-key", persistent: true });
  assert.equal(win.sessionStorage.getItem(namespace), null);
  login.clear();
  assert.equal(login.read(), null);
  assert.equal(win.localStorage.getItem("host-credential"), "untouched");
});

// Unchecking remember removes an old persistent login before using the session.
test("unchecked login replaces old persistence with a session-only management key", () => {
  const namespace = ui.PLUGIN_ID + ".management-key:/keys";
  const win = { localStorage: storage({ [namespace]: "old-key" }), sessionStorage: storage() };
  const login = ui.managementCredentials(win, namespace);
  login.save("new-key", false);
  assert.equal(win.localStorage.getItem(namespace), null);
  assert.deepEqual(login.read(), { key: "new-key", persistent: false });
  assert.equal(ui.managementCredentials(win, namespace + "/other-prefix").read(), null);
});

// Blocked browser storage must not prevent login or falsely claim persistence.
test("management login degrades from persistent to session to memory honestly", () => {
  const namespace = ui.PLUGIN_ID + ".management-key:/keys";
  const unavailable = { getItem() { throw new Error(); }, setItem() { throw new Error(); }, removeItem() { throw new Error(); } };
  const win = { localStorage: unavailable, sessionStorage: storage() };
  const login = ui.managementCredentials(win, namespace);
  assert.match(login.save("key", true).warning, /仅在当前会话/);
  assert.deepEqual(login.read(), { key: "key", persistent: false });
  win.sessionStorage = unavailable;
  assert.match(login.save("key", true).warning, /内存/);
  assert.equal(login.read(), null);
  assert.doesNotThrow(() => login.clear());
});
