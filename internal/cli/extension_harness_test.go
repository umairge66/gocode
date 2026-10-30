package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeChromePrelude loads the real extension/background.js under Node with a
// fake chrome.* API (no network, no browser). It records every CDP command the
// extension sends in `cdpLog` and exposes `call(method, params)` (the
// extension's dispatch), `tabs` and `fails`. A test appends its own script.
const fakeChromePrelude = `
const fs = require("fs");
const vm = require("vm");

let nextTab = 100, nextGroup = 500;
const tabs = new Map();
const groups = new Map();
const session = {};
const listeners = { created: [], removed: [], updated: [] };
const cdpLog = [];
function addTab(t) { const tab = { status: "complete", windowId: 1, groupId: -1, active: false, ...t }; tabs.set(tab.id, tab); return tab; }
addTab({ id: 1, url: "https://bank.example/", title: "Bank", active: true });

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
    sendCommand: (_t, method, params, cb) => {
      cdpLog.push({ method, params });
      if (method === "Runtime.evaluate") return cb({ result: { type: "number", value: 2 } });
      cb({ data: "AAAA" });
    } },
  scripting: { executeScript: async ({ target, func }) => {
    const src = String(func);
    if (src.includes("devicePixelRatio")) return [{ result: { dpr: 1, w: 800, h: 600 } }];
    if (src.includes("querySelector")) return [{ result: { ok: true } }];
    return [{ result: "TEXT OF TAB " + target.tabId }];
  } },
  windows: { update: async (id, p) => ({ id, width: p.width, height: p.height }) },
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
    goBack: async () => {}, goForward: async () => {},
    remove: async (ids) => {
      for (const id of [].concat(ids)) if (!tabs.has(id)) throw new Error("No tab with id: " + id);
      for (const id of [].concat(ids)) { tabs.delete(id); for (const f of listeners.removed) await f(id); }
    },
    group: async ({ tabIds, groupId }) => {
      if (groupId === undefined) { groupId = nextGroup++; groups.set(groupId, { id: groupId, title: "", windowId: 1 }); }
      for (const id of tabIds) tabs.get(id).groupId = groupId;
      return groupId;
    },
  },
};
const ctx = vm.createContext({ chrome, console: { log() {} }, setTimeout, clearTimeout, Promise, URL });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const call = (m, p) => vm.runInContext("dispatch", ctx)(m, p || {});
const fails = [];
function finish() {
  if (fails.length) { console.error(fails.join("\n")); process.exit(1); }
  console.error("ok");
  process.exit(0);
}
`

// runExtensionHarness runs the prelude plus script against the real extension.
func runExtensionHarness(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	bg, err := filepath.Abs(filepath.Join("..", "..", "extension", "background.js"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(path, []byte(fakeChromePrelude+script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path, bg).CombinedOutput()
	if err != nil {
		t.Fatalf("extension harness failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// keyHarness checks the CDP Input.dispatchKeyEvent events the computer tool's
// key action produces. Each event is rendered compactly as
// "type key code vk m<mask> [l<location>] [t=<text>]".
const keyHarness = `
function render(p) {
  let s = p.type + " " + JSON.stringify(p.key) + " " + (p.code || "-") + " " + (p.windowsVirtualKeyCode || 0) + " m" + p.modifiers;
  if (p.location) s += " l" + p.location;
  if (p.text !== undefined) s += " t=" + JSON.stringify(p.text);
  if (p.commands) s += " cmd=" + p.commands.join(",");
  return s;
}
async function keys(text, repeat) {
  cdpLog.length = 0;
  await call("computer", { action: "key", text, repeat, tabId: tab });
  return cdpLog.filter((c) => c.method === "Input.dispatchKeyEvent").map((c) => render(c.params));
}
async function expectKeys(text, want, repeat) {
  let got;
  try { got = await keys(text, repeat); } catch (e) { fails.push(text + ": threw " + e.message); return; }
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    fails.push(text + ":\n  got  " + got.join("\n       ") + "\n  want " + want.join("\n       "));
  }
}
async function expectKeyError(text, re) {
  cdpLog.length = 0;
  try { await call("computer", { action: "key", text, tabId: tab }); fails.push(text + ": no error"); }
  catch (e) { if (!re.test(e.message)) fails.push(text + ": wrong error: " + e.message); }
  if (cdpLog.some((c) => c.method === "Input.dispatchKeyEvent")) fails.push(text + ": sent key events before failing");
}

const SHIFT_D = (m) => "rawKeyDown \"Shift\" ShiftLeft 16 m" + m + " l1";
const SHIFT_U = (m) => "keyUp \"Shift\" ShiftLeft 16 m" + m + " l1";
const CTRL_D = (m) => "rawKeyDown \"Control\" ControlLeft 17 m" + m + " l1";
const CTRL_U = (m) => "keyUp \"Control\" ControlLeft 17 m" + m + " l1";
const ALT_D = (m) => "rawKeyDown \"Alt\" AltLeft 18 m" + m + " l1";
const ALT_U = (m) => "keyUp \"Alt\" AltLeft 18 m" + m + " l1";
let tab;

(async () => {
  tab = (await call("tabs_create", { url: "https://example.com/" })).tabId;

  await expectKeys("alt+x", [ALT_D(1), "rawKeyDown \"x\" KeyX 88 m1", "keyUp \"x\" KeyX 88 m1", ALT_U(0)]);
  await expectKeys("shift+Tab", [SHIFT_D(8), "rawKeyDown \"Tab\" Tab 9 m8", "keyUp \"Tab\" Tab 9 m8", SHIFT_U(0)]);
  await expectKeys("ctrl+shift+a", [CTRL_D(2), SHIFT_D(10), "rawKeyDown \"A\" KeyA 65 m10", "keyUp \"A\" KeyA 65 m10", SHIFT_U(2), CTRL_U(0)]);
  await expectKeys("alt+shift+Tab", [ALT_D(1), SHIFT_D(9), "rawKeyDown \"Tab\" Tab 9 m9", "keyUp \"Tab\" Tab 9 m9", SHIFT_U(1), ALT_U(0)]);
  await expectKeys("ctrl+a", [CTRL_D(2), "rawKeyDown \"a\" KeyA 65 m2", "keyUp \"a\" KeyA 65 m2", CTRL_U(0)]);
  await expectKeys("cmd+a", ["rawKeyDown \"Meta\" MetaLeft 91 m4 l1", "rawKeyDown \"a\" KeyA 65 m4", "keyUp \"a\" KeyA 65 m4", "keyUp \"Meta\" MetaLeft 91 m0 l1"]);
  await expectKeys("shift+a", [SHIFT_D(8), "keyDown \"A\" KeyA 65 m8 t=\"A\"", "keyUp \"A\" KeyA 65 m8", SHIFT_U(0)]);
  await expectKeys("shift+1", [SHIFT_D(8), "keyDown \"!\" Digit1 49 m8 t=\"!\"", "keyUp \"!\" Digit1 49 m8", SHIFT_U(0)]);
  await expectKeys("A", [SHIFT_D(8), "keyDown \"A\" KeyA 65 m8 t=\"A\"", "keyUp \"A\" KeyA 65 m8", SHIFT_U(0)]);
  await expectKeys("a", ["keyDown \"a\" KeyA 65 m0 t=\"a\"", "keyUp \"a\" KeyA 65 m0"]);
  await expectKeys("ctrl+shift", [CTRL_D(2), SHIFT_D(10), SHIFT_U(2), CTRL_U(0)]);
  await expectKeys("shift", [SHIFT_D(8), SHIFT_U(0)]);

  await expectKeys("Numpad5", ["keyDown \"5\" Numpad5 101 m0 l3 t=\"5\"", "keyUp \"5\" Numpad5 101 m0 l3"]);
  await expectKeys("Numpad0", ["keyDown \"0\" Numpad0 96 m0 l3 t=\"0\"", "keyUp \"0\" Numpad0 96 m0 l3"]);
  await expectKeys("NumpadEnter", ["keyDown \"Enter\" NumpadEnter 13 m0 l3 t=\"\\r\"", "keyUp \"Enter\" NumpadEnter 13 m0 l3"]);
  await expectKeys("KP_Enter", ["keyDown \"Enter\" NumpadEnter 13 m0 l3 t=\"\\r\"", "keyUp \"Enter\" NumpadEnter 13 m0 l3"]);
  await expectKeys("NumpadAdd", ["keyDown \"+\" NumpadAdd 107 m0 l3 t=\"+\"", "keyUp \"+\" NumpadAdd 107 m0 l3"]);
  await expectKeys("kp_add", ["keyDown \"+\" NumpadAdd 107 m0 l3 t=\"+\"", "keyUp \"+\" NumpadAdd 107 m0 l3"]);
  await expectKeys("numpad+", ["keyDown \"+\" NumpadAdd 107 m0 l3 t=\"+\"", "keyUp \"+\" NumpadAdd 107 m0 l3"]);
  await expectKeys("NumpadDecimal", ["keyDown \".\" NumpadDecimal 110 m0 l3 t=\".\"", "keyUp \".\" NumpadDecimal 110 m0 l3"]);
  await expectKeys("ctrl+Numpad1", [CTRL_D(2), "rawKeyDown \"1\" Numpad1 97 m2 l3", "keyUp \"1\" Numpad1 97 m2 l3", CTRL_U(0)]);

  await expectKeys("Return", ["keyDown \"Enter\" Enter 13 m0 t=\"\\r\"", "keyUp \"Enter\" Enter 13 m0"]);
  await expectKeys("Enter", ["keyDown \"Enter\" Enter 13 m0 t=\"\\r\"", "keyUp \"Enter\" Enter 13 m0"]);
  await expectKeys("space", ["keyDown \" \" Space 32 m0 t=\" \"", "keyUp \" \" Space 32 m0"]);
  await expectKeys("ArrowUp", ["rawKeyDown \"ArrowUp\" ArrowUp 38 m0", "keyUp \"ArrowUp\" ArrowUp 38 m0"]);
  await expectKeys("Page_Down", ["rawKeyDown \"PageDown\" PageDown 34 m0", "keyUp \"PageDown\" PageDown 34 m0"]);
  await expectKeys("BackSpace", ["rawKeyDown \"Backspace\" Backspace 8 m0", "keyUp \"Backspace\" Backspace 8 m0"]);
  await expectKeys("F12", ["rawKeyDown \"F12\" F12 123 m0", "keyUp \"F12\" F12 123 m0"]);
  await expectKeys("Backspace Delete", ["rawKeyDown \"Backspace\" Backspace 8 m0", "keyUp \"Backspace\" Backspace 8 m0", "rawKeyDown \"Delete\" Delete 46 m0", "keyUp \"Delete\" Delete 46 m0"]);
  await expectKeys("Tab", ["rawKeyDown \"Tab\" Tab 9 m0", "keyUp \"Tab\" Tab 9 m0", "rawKeyDown \"Tab\" Tab 9 m0", "keyUp \"Tab\" Tab 9 m0"], 2);
  await expectKeys("+", [SHIFT_D(8), "keyDown \"+\" Equal 187 m8 t=\"+\"", "keyUp \"+\" Equal 187 m8", SHIFT_U(0)]);

  await expectKeyError("ctrl+=", /zoom/i);
  await expectKeyError("cmd+0", /zoom/i);
  await expectKeyError("ctrl+-", /zoom/i);
  await expectKeyError("ctrl+plus", /zoom/i);
  await expectKeyError("a ctrl+=", /zoom/i);
  await expectKeyError("hyper+a", /Unknown modifier/);
  try { await call("computer", { action: "key", text: "a", repeat: 0, tabId: tab }); fails.push("repeat 0 accepted"); }
  catch (e) { if (!/repeat/.test(e.message)) fails.push("repeat 0: wrong error " + e.message); }

  // Click modifiers use the same names, including win/windows.
  cdpLog.length = 0;
  await call("computer", { action: "left_click", coordinate: [5, 5], modifiers: "ctrl+shift+win", tabId: tab });
  const press = cdpLog.find((c) => c.method === "Input.dispatchMouseEvent" && c.params.type === "mousePressed");
  if (!press || press.params.modifiers !== 14) fails.push("click modifiers: " + JSON.stringify(press && press.params));

  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionKeyEvents(t *testing.T) {
	runExtensionHarness(t, keyHarness)
}

// siteHarness checks the extension's own enforcement of the per-site policy the
// host sends with each command (defence in depth for the Go-side check).
const siteHarness = `
async function refused(name, m, p, re) {
  try { await call(m, p); fails.push(name + ": was allowed"); }
  catch (e) { if (!re.test(e.message)) fails.push(name + ": wrong error: " + e.message); }
}
async function allowed(name, m, p) {
  try { return await call(m, p); } catch (e) { fails.push(name + ": refused: " + e.message); }
}
const ask = { allowed: [], denied: [], requireAllowed: true };
const NEEDS = /needs_approval.*\[chrome_site=example\.com\]/;

(async () => {
  // No policy: nothing changes.
  const tab = (await call("tabs_create", { url: "https://example.com/" })).tabId;
  await allowed("type without policy", "computer", { action: "type", text: "x", tabId: tab });

  // Acting on a site with no allow rule needs approval; reading does not.
  await refused("type on unknown site", "computer", { action: "type", text: "x", tabId: tab, sitePolicy: ask }, NEEDS);
  await refused("form_input on unknown site", "form_input", { ref: "r", value: "v", tabId: tab, sitePolicy: ask }, NEEDS);
  await refused("javascript on unknown site", "javascript_tool", { action: "javascript_exec", text: "1+1", tabId: tab, sitePolicy: ask }, NEEDS);
  await allowed("read on unknown site", "read_page", { tabId: tab, sitePolicy: ask });
  await allowed("screenshot on unknown site", "computer", { action: "screenshot", tabId: tab, sitePolicy: ask });

  // The last policy sticks for a command that carries none.
  await refused("stored policy", "computer", { action: "key", text: "a", tabId: tab }, NEEDS);

  // Allowed site: acting works.
  const ok = { allowed: ["example.com"], denied: [], requireAllowed: true };
  await allowed("type on allowed site", "computer", { action: "type", text: "x", tabId: tab, sitePolicy: ok });
  const js = await allowed("javascript on allowed site", "javascript_tool", { action: "javascript_exec", text: "1+1", tabId: tab, sitePolicy: ok });
  if (!js || js.value !== "2") fails.push("javascript value: " + JSON.stringify(js));

  // Denied site: refused for reading and acting, even when also allowed.
  const deny = { allowed: ["example.com"], denied: ["example.com"], requireAllowed: false };
  await refused("read on denied site", "read_page", { tabId: tab, sitePolicy: deny }, /denied on example\.com/);
  await refused("find on denied site", "find", { query: "x", tabId: tab, sitePolicy: deny }, /denied on example\.com/);
  await refused("screenshot on denied site", "screenshot", { tabId: tab, sitePolicy: deny }, /denied on example\.com/);

  // Navigation: denied, unknown, allowed (an https rule covers http :80), non-web.
  const nav = { allowed: ["example.com"], denied: ["bank.example"], requireAllowed: true };
  await refused("navigate to denied", "navigate", { tabId: tab, url: "https://bank.example/login", sitePolicy: nav }, /denied on bank\.example/);
  if (tabs.get(tab).url !== "https://example.com/") fails.push("denied navigation changed the tab: " + tabs.get(tab).url);
  await refused("navigate to unknown", "navigate", { tabId: tab, url: "other.example", sitePolicy: nav }, /\[chrome_site=other\.example\]/);
  await refused("navigate to javascript:", "navigate", { tabId: tab, url: "javascript:alert(1)", sitePolicy: nav }, /browser-internal/);
  await allowed("navigate to allowed http", "navigate", { tabId: tab, url: "http://example.com/a", sitePolicy: nav });
  await refused("tabs_create on denied", "tabs_create", { url: "https://bank.example/", sitePolicy: nav }, /denied on bank\.example/);

  // A batch stops at the step that needs approval.
  const batch = await call("browser_batch", { sitePolicy: ask, actions: [
    { name: "read_page", input: { tabId: tab } },
    { name: "computer", input: { action: "type", text: "x", tabId: tab } },
    { name: "computer", input: { action: "type", text: "y", tabId: tab } },
  ] });
  if (batch.results.length !== 2 || !batch.results[0].ok || !/chrome_site=example\.com/.test(batch.results[1].error || "")) {
    fails.push("batch: " + JSON.stringify(batch.results));
  }

  // The verbs added for the MCP server keep tab isolation too.
  await refused("javascript on user's tab", "javascript_tool", { action: "javascript_exec", text: "1", tabId: 1 }, /not a gocode tab/);
  await refused("resize user's window", "resize_window", { width: 100, height: 100, tabId: 1 }, /not a gocode tab/);
  await refused("history on user's tab", "navigate", { url: "back", tabId: 1 }, /not a gocode tab/);
  const listed = await call("tabs_context", { createIfEmpty: true });
  if (listed.tabs.some((t) => t.id === 1)) fails.push("tabs_context listed the user's tab");

  // Site rules never widen tab isolation: the user's tab stays off limits.
  await refused("allowed site, user's tab", "read_page", { tabId: 1, sitePolicy: { allowed: ["bank.example"], denied: [], requireAllowed: false } }, /not a gocode tab/);

  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionSitePolicy(t *testing.T) {
	runExtensionHarness(t, siteHarness)
}
