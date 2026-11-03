package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolationHarness runs the real extension/background.js under Node with a fake
// chrome.* API and checks the tab-isolation rules: gocode only sees and acts on
// tabs in its own group, never the user's other tabs.
const isolationHarness = `
const fs = require("fs");
const vm = require("vm");

let nextTab = 100, nextGroup = 500;
const tabs = new Map();   // id -> tab
const groups = new Map(); // id -> group
const session = {};
const listeners = { created: [], removed: [], updated: [] };
function addTab(t) { const tab = { status: "complete", windowId: 1, groupId: -1, active: false, ...t }; tabs.set(tab.id, tab); return tab; }

// The user's own tabs, including a group they named "gocode" themselves.
addTab({ id: 1, url: "https://bank.example/", title: "Bank", active: true });
addTab({ id: 2, url: "https://mail.example/", title: "Mail" });
groups.set(50, { id: 50, title: "gocode", color: "blue", windowId: 1 });
addTab({ id: 3, url: "https://notes.example/", title: "Notes", groupId: 50 });

const ev = (arr) => ({ addListener: (f) => arr.push(f), removeListener: () => {} });
const chrome = {
  runtime: {
    getManifest: () => ({ version: "1" }),
    connectNative: () => ({ onMessage: ev([]), onDisconnect: ev([]), postMessage() {} }),
    onStartup: ev([]), onInstalled: ev([]), lastError: undefined,
  },
  action: { setBadgeText() {}, setBadgeBackgroundColor() {}, setTitle() {} },
  alarms: { create() {}, onAlarm: ev([]) },
  storage: { session: {
    get: async (keys) => { const o = {}; for (const k of [].concat(keys)) if (k in session) o[k] = session[k]; return o; },
    set: async (o) => Object.assign(session, o),
  } },
  debugger: { onDetach: ev([]), attach: async () => {}, detach: async () => {},
    sendCommand: (_t, _m, _p, cb) => cb({ data: "AAAA" }) },
  scripting: { executeScript: async ({ target }) => [{ result: "TEXT OF TAB " + target.tabId }] },
  tabGroups: {
    get: async (id) => { if (!groups.has(id)) throw new Error("no group"); return groups.get(id); },
    update: async (id, p) => Object.assign(groups.get(id), p),
    query: async () => [...groups.values()],
  },
  tabs: {
    onCreated: ev(listeners.created), onRemoved: ev(listeners.removed), onUpdated: ev(listeners.updated),
    get: (id, cb) => { const t = tabs.get(id); chrome.runtime.lastError = t ? undefined : { message: "no tab" }; cb(t); chrome.runtime.lastError = undefined; },
    query: async (q) => [...tabs.values()].filter((t) => q.groupId === undefined || t.groupId === q.groupId),
    create: async ({ url, openerTabId }) => { const t = addTab({ id: nextTab++, url: url || "chrome://newtab/", openerTabId }); for (const f of listeners.created) await f(t); return t; },
    update: async (id, p) => Object.assign(tabs.get(id), p),
    remove: async (id) => { tabs.delete(id); for (const f of listeners.removed) await f(id); },
    group: async ({ tabIds, groupId }) => {
      if (groupId === undefined) { groupId = nextGroup++; groups.set(groupId, { id: groupId, title: "", windowId: 1 }); }
      for (const id of tabIds) tabs.get(id).groupId = groupId;
      return groupId;
    },
  },
};
const ctx = vm.createContext({ chrome, console: { log() {} }, setTimeout, clearTimeout, Promise });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const call = (m, p) => vm.runInContext("dispatch", ctx)(m, p || {});

const fails = [];
async function refused(name, m, p) {
  try { await call(m, p); fails.push(name + ": was allowed"); }
  catch (e) { if (!/not a gocode tab|no gocode tab/.test(e.message)) fails.push(name + ": wrong error: " + e.message); }
}
const ids = (r) => r.tabs.map((t) => t.id).sort().join(",");

(async () => {
  // Before gocode opens anything it sees nothing, and never falls back to the active tab.
  let ctxRes = await call("tabs_context");
  if (ctxRes.tabs.length || ctxRes.groups.length) fails.push("tabs_context listed user tabs/groups: " + JSON.stringify(ctxRes));
  await refused("read_page with no gocode tab", "read_page");
  await refused("screenshot with no gocode tab", "screenshot");

  // gocode opens its own tab: it lands in a NEW gocode group, not the user's "gocode" group.
  const created = await call("tabs_create", { url: "https://example.com/" });
  const mine = tabs.get(created.tabId);
  if (mine.groupId === 50 || mine.groupId === -1) fails.push("new tab not in gocode's own group: " + mine.groupId);
  ctxRes = await call("tabs_context");
  if (ids(ctxRes) !== String(created.tabId)) fails.push("tabs_context should list only gocode's tab, got " + ids(ctxRes));
  if (ctxRes.groups.some((g) => g.id === 50)) fails.push("tabs_context listed the user's group");

  // Its own tab works.
  const page = await call("read_page", {});
  if (page.tabId !== created.tabId) fails.push("read_page default did not use gocode's tab");

  // Every way of reaching the user's tabs is refused.
  for (const id of [1, 2, 3]) {
    await refused("read_page " + id, "read_page", { tabId: id });
    await refused("navigate " + id, "navigate", { tabId: id, url: "https://evil.example/" });
    await refused("tabs_close " + id, "tabs_close", { tabId: id });
    await refused("screenshot " + id, "screenshot", { tabId: id });
    await refused("find " + id, "find", { tabId: id, query: "x" });
    await refused("form_input " + id, "form_input", { tabId: id, ref: "r", value: "v" });
    await refused("computer " + id, "computer", { tabId: id, action: "type", text: "x" });
  }
  const batch = await call("browser_batch", { actions: [{ name: "read_page", input: { tabId: 1 } }] });
  if (!batch.results[0].error) fails.push("browser_batch reached a user tab");
  if (!tabs.has(1) || tabs.get(1).url !== "https://bank.example/") fails.push("user tab was changed");

  // A link opened from gocode's tab by a gocode click joins gocode's group; a
  // tab the user opens does not — not even one they open from a gocode tab
  // (a middle-click of their own) while gocode is not acting on it.
  const quiet = await call("tabs_create", { url: "https://example.com/quiet" });
  await call("computer", { tabId: created.tabId, action: "left_click", coordinate: [5, 5] });
  const child = await chrome.tabs.create({ url: "https://example.com/next", openerTabId: created.tabId });
  await new Promise((r) => setTimeout(r, 10));
  if (tabs.get(child.id).groupId !== mine.groupId) fails.push("tab opened from gocode's tab not grouped");
  const userTab = await chrome.tabs.create({ url: "https://user.example/" });
  await new Promise((r) => setTimeout(r, 10));
  await refused("user's new tab", "read_page", { tabId: userTab.id });
  const userLink = await chrome.tabs.create({ url: "https://example.com/mine", openerTabId: quiet.tabId });
  await new Promise((r) => setTimeout(r, 10));
  if (tabs.get(userLink.id).groupId !== -1) fails.push("a tab the user opened from a gocode tab was adopted");
  await refused("user's link from a gocode tab", "read_page", { tabId: userLink.id });

  // Dragging gocode's tab out of its group takes it away from gocode.
  tabs.get(created.tabId).groupId = -1;
  await refused("tab dragged out of the group", "read_page", { tabId: created.tabId });

  if (fails.length) { console.error(fails.join("\n")); process.exit(1); }
  console.error("ok");
  process.exit(0);
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionTabIsolation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	bg, err := filepath.Abs(filepath.Join("..", "..", "extension", "background.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(script, []byte(isolationHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, bg).CombinedOutput()
	if err != nil {
		t.Fatalf("tab isolation violated:\n%s", strings.TrimSpace(string(out)))
	}
}
