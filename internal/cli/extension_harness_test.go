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
//
// Page scripts: a tab given a page with loadPage(tabId, build) runs the
// extension's injected functions against a small fake DOM (one vm context per
// document, like the extension's isolated world), so find / form_input / ref
// clicks run for real. Other tabs answer page scripts with canned results.
// cdpHook(target, method, params, cb) may take over a CDP command (return
// true; call cb later or never); scriptHook(target, func, args) may take over
// a page script (return its result array). emitCdp sends a debugger event.
// The timeouts are shortened (see fastTimeouts) so a hang fails in well under
// a second.
const fakeChromePrelude = `
const fs = require("fs");
const vm = require("vm");

let nextTab = 100, nextGroup = 500;
const tabs = new Map();
const groups = new Map();
const session = {};
const listeners = { created: [], removed: [], updated: [] };
const cdpLog = [];
const scriptLog = [];
const attached = new Set();
const attachLog = [];
const resized = [];
let cdpHook = null, scriptHook = null;
function addTab(t) { const tab = { status: "complete", windowId: 1, groupId: -1, active: false, ...t }; tabs.set(tab.id, tab); return tab; }
addTab({ id: 1, url: "https://bank.example/", title: "Bank", active: true });

// --- a small fake DOM ---
class FEvent { constructor(type, o) { Object.assign(this, o || {}); this.type = type; this.bubbles = !!(o && o.bubbles); } }
class FNode {
  constructor(doc) { this.ownerDocument = doc; this.childNodes = []; this.parentNode = null; this.listeners = {}; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.childNodes.push(c); return c; }
  removeChild(c) { this.childNodes = this.childNodes.filter((n) => n !== c); c.parentNode = null; return c; }
  get children() { return this.childNodes.filter((n) => n.nodeType === 1); }
  get textContent() { return this.childNodes.map((n) => n.textContent).join(""); }
  set textContent(v) { for (const c of this.childNodes) c.parentNode = null; this.childNodes = []; if (v !== "" && v != null) this.appendChild(new FText(this.ownerDocument, String(v))); }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  getRootNode() { let n = this; while (n.parentNode) n = n.parentNode; return n; }
  get isConnected() { const r = this.getRootNode(); return r === this.ownerDocument || (!!r.host && r.host.isConnected); }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  dispatchEvent(e) {
    if (!e.target) e.target = this;
    for (let n = this; n; n = n.parentNode || n.host) { for (const f of (n.listeners[e.type] || [])) f.call(n, e); if (!e.bubbles) break; }
    return true;
  }
  getElementById(id) { let found = null; const v = (n) => { for (const c of n.children) { if (found) return; if (c.getAttribute("id") === id) { found = c; return; } v(c); } }; v(this); return found; }
}
class FText extends FNode { constructor(doc, t) { super(doc); this.nodeType = 3; this.data = t; } get textContent() { return this.data; } }
class FElement extends FNode {
  constructor(doc, tag, attrs) { super(doc); this.nodeType = 1; this.tagName = tag.toUpperCase(); this.attrs = {}; this.box = null; this.shadowRoot = null; for (const k in attrs || {}) this.attrs[k] = String(attrs[k]); }
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; }
  setAttribute(n, v) { this.attrs[n] = String(v); }
  hasAttribute(n) { return n in this.attrs; }
  removeAttribute(n) { delete this.attrs[n]; }
  get id() { return this.attrs.id || ""; }
  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get innerText() { return this.textContent; }
  get isContentEditable() { for (let n = this; n && n.nodeType === 1; n = n.parentNode) { const v = n.attrs.contenteditable; if (v === "" || v === "true") return true; if (v === "false") return false; } return false; }
  get disabled() { return "disabled" in this.attrs; }
  get readOnly() { return "readonly" in this.attrs; }
  get form() { for (let n = this.parentNode; n; n = n.parentNode) if (n.tagName === "FORM") return n; return null; }
  attachShadow() { this.shadowRoot = new FShadow(this); return this.shadowRoot; }
  get contentDocument() { return this.frameDoc || null; }
  getBoundingClientRect() {
    const b = this.box || [0, 0, 0, 0];
    const sy = "data-fixed" in this.attrs ? 0 : this.ownerDocument.scrollY;
    return { left: b[0], top: b[1] - sy, width: b[2], height: b[3] };
  }
  scrollIntoView() { const d = this.ownerDocument; d.scrolls++; const b = this.box; if (b) d.scrollY = Math.max(0, b[1] + b[3] / 2 - d.defaultView.innerHeight / 2); }
  focus() { this.ownerDocument.activeElement = this; }
  click() { this.dispatchEvent(new FEvent("click", { bubbles: true })); }
  get clientLeft() { return 0; }
  get clientTop() { return 0; }
}
const VALID = { date: /^\d{4}-\d{2}-\d{2}$/ };
class FInput extends FElement {
  get type() { return (this.attrs.type || "text").toLowerCase(); }
  get value() { return this._value !== undefined ? this._value : (this.attrs.value || (this.type === "checkbox" || this.type === "radio" ? "on" : "")); }
  set value(v) { v = String(v); const re = VALID[this.type]; this._value = re && !re.test(v) ? "" : v; }
  get checked() { return !!this._checked; }
  set checked(v) {
    this._checked = !!v;
    if (v && this.type === "radio") for (const r of allElements(this.ownerDocument)) if (r !== this && r.tagName === "INPUT" && r.type === "radio" && r.attrs.name === this.attrs.name) r._checked = false;
  }
  click() {
    if (this.disabled) return;
    if (this.type === "checkbox") this.checked = !this.checked;
    else if (this.type === "radio") this.checked = true;
    super.click();
    if (this.type === "checkbox" || this.type === "radio") { this.dispatchEvent(new FEvent("input", { bubbles: true })); this.dispatchEvent(new FEvent("change", { bubbles: true })); }
  }
}
class FTextArea extends FElement { get value() { return this._value !== undefined ? this._value : this.textContent; } set value(v) { this._value = String(v); } }
class FOption extends FElement {
  get value() { return "value" in this.attrs ? this.attrs.value : this.text; }
  get text() { return this.textContent.trim(); }
  get selected() {
    const s = this.select;
    if (s && !s.multiple && !s.options.some((o) => o._sel)) return s.options[0] === this; // the default selection
    return !!this._sel;
  }
  set selected(v) { this._sel = !!v; const s = this.select; if (v && s && !s.multiple) for (const o of s.options) if (o !== this) o._sel = false; }
  get select() { for (let n = this.parentNode; n; n = n.parentNode) if (n.tagName === "SELECT") return n; return null; }
}
class FSelect extends FElement {
  get multiple() { return "multiple" in this.attrs; }
  get options() { const out = []; const v = (n) => { for (const c of n.children) { if (c.tagName === "OPTION") out.push(c); else v(c); } }; v(this); return out; }
  get value() { const o = this.options.find((x) => x.selected) || (!this.multiple && this.options[0]); return o ? o.value : ""; }
  set value(v) { const o = this.options.find((x) => x.value === String(v)); for (const x of this.options) x._sel = false; if (o) o._sel = true; }
}
class FShadow extends FNode {
  constructor(host) { super(host.ownerDocument); this.nodeType = 11; this.host = host; }
  elementFromPoint(x, y) { return hitTest(this, x, y); }
}
class FDocument extends FNode {
  constructor(win) { super(null); this.ownerDocument = this; this.nodeType = 9; this.defaultView = win; this.scrollY = 0; this.scrolls = 0; }
  get documentElement() { return this.children[0] || null; }
  get body() { const html = this.documentElement; return html && html.children.find((c) => c.tagName === "BODY"); }
  elementFromPoint(x, y) { return hitTest(this, x, y); }
  querySelector() { return null; }
}
function allElements(root) { const out = []; const v = (n) => { for (const c of n.children) { out.push(c); if (c.shadowRoot) v(c.shadowRoot); v(c); } }; v(root); return out; }
// hitTest: the last element in DOM order (light tree of root) whose box holds the point.
function hitTest(root, x, y) {
  let hit = null;
  const v = (n) => { for (const c of n.children) { const r = c.getBoundingClientRect(); if (c.box && x >= r.left && x < r.left + r.width && y >= r.top && y < r.top + r.height) hit = c; v(c); } };
  v(root);
  return hit;
}
function makeDocument(frameElement) {
  const win = { innerWidth: 800, innerHeight: 600, devicePixelRatio: 1, scrollX: 0, frameElement: frameElement || null, Event: FEvent, InputEvent: FEvent, setTimeout,
    getComputedStyle: (el) => ({ visibility: /visibility:\s*hidden/.test(el.attrs.style || "") ? "hidden" : "visible", display: "block" }) };
  const doc = new FDocument(win);
  Object.defineProperty(win, "scrollY", { get: () => doc.scrollY });
  win.document = doc;
  const E = (tag, attrs, ...kids) => {
    const T = { input: FInput, textarea: FTextArea, select: FSelect, option: FOption }[tag] || FElement;
    const a = { ...(attrs || {}) };
    const box = a.box; delete a.box;
    const el = new T(doc, tag, a);
    if (box) el.box = box;
    for (const k of kids.flat()) el.appendChild(typeof k === "string" ? new FText(doc, k) : k);
    return el;
  };
  doc.appendChild(E("html", {}, E("body", { box: [0, 0, 800, 2000] })));
  return { doc, win, E };
}
const pages = new Map(); // tabId -> { ctx, doc, fns }
// loadPage gives tabId a new document (as a navigation does) built by build(body, E, doc).
function loadPage(tabId, build, origin) {
  const { doc, win, E } = makeDocument();
  build(doc.body, E, doc);
  const tab = tabs.get(tabId);
  const o = origin || (tab && /^https?:/.test(tab.url) ? new URL(tab.url).origin : "null");
  const pctx = vm.createContext({ document: doc, window: win, self: { origin: o }, setTimeout });
  pages.set(tabId, { ctx: pctx, doc, fns: new Map() });
  return doc;
}
// frame builds a same-origin iframe element whose document is built by build.
function frame(E, attrs, build) {
  const el = E("iframe", attrs);
  const sub = makeDocument(el);
  build(sub.doc.body, sub.E, sub.doc);
  el.frameDoc = sub.doc;
  return el;
}

const dbgEvents = [], dbgDetach = [];
function emitCdp(tabId, method, params) { for (const f of dbgEvents) f({ tabId }, method, params); }
const ev = (arr) => ({ addListener: (f) => arr.push(f), removeListener: () => {} });
// Native ports: ports[i] is the i-th connectNative; port.sent lists what the extension posted.
const ports = [];
const chrome = {
  runtime: {
    getManifest: () => ({ version: "1" }),
    connectNative: () => {
      const p = { msg: [], disc: [], sent: [], throwOnce: false };
      p.onMessage = ev(p.msg); p.onDisconnect = ev(p.disc);
      p.postMessage = (m) => { if (p.throwOnce && m.result) { p.throwOnce = false; throw new Error("Message length exceeded maximum allowed length."); } p.sent.push(m); };
      p.deliver = (m) => { for (const f of p.msg) f(m); };
      p.disconnect = () => { for (const f of p.disc) f(); };
      ports.push(p);
      return p;
    },
    onStartup: ev([]), onInstalled: ev([]), lastError: undefined,
  },
  action: { setBadgeText() {}, setBadgeBackgroundColor() {}, setTitle() {} },
  alarms: { create() {}, onAlarm: ev([]) },
  storage: { session: {
    get: async (keys) => { await null; const o = {}; for (const k of [].concat(keys)) if (k in session) o[k] = JSON.parse(JSON.stringify(session[k])); return o; },
    set: async (o) => { await null; Object.assign(session, JSON.parse(JSON.stringify(o))); },
  } },
  debugger: { onDetach: ev(dbgDetach), onEvent: ev(dbgEvents),
    attach: async ({ tabId }) => {
      if (attached.has(tabId)) throw new Error("Another debugger is already attached to the tab with id: " + tabId + ".");
      attached.add(tabId); attachLog.push(tabId);
    },
    detach: async ({ tabId }) => { attached.delete(tabId); },
    sendCommand: (t, method, params, cb) => {
      cdpLog.push({ tabId: t.tabId, method, params });
      if (cdpHook && cdpHook(t, method, params, cb)) return;
      if (method === "Runtime.evaluate") return cb({ result: { type: "number", value: 2 } });
      cb({ data: "AAAA" });
    } },
  scripting: { executeScript: async ({ target, func, args }) => {
    scriptLog.push({ tabId: target.tabId, name: func.name, args });
    if (scriptHook) { const r = await scriptHook(target, func, args || []); if (r !== undefined) return r; }
    const pg = pages.get(target.tabId);
    if (pg) {
      if (!pg.fns.has(func)) pg.fns.set(func, vm.runInContext("(" + func.toString() + ")", pg.ctx));
      return [{ result: await pg.fns.get(func)(...(args || [])) }];
    }
    if (func.name === "pageAgent") {
      const [op, arg] = args;
      if (op === "info") return [{ result: { dpr: 1, w: 800, h: 600, sx: 0, sy: 0 } }];
      if (op === "fill") return [{ result: { ok: true, ref: arg.ref, value: String(arg.value) } }];
      if (op === "point") return [{ result: { x: 10, y: 10 } }];
      if (op === "find") return [{ result: { matches: [], total: 0 } }];
      if (op === "origin") return [{ result: "null" }];
      return [{ result: { ok: true } }];
    }
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
class OffscreenCanvas {
  constructor(w, h) { resized.push([w, h]); }
  getContext() { return { drawImage() {} }; }
  async convertToBlob() { return { arrayBuffer: async () => new Uint8Array([105, 109, 103]).buffer }; }
}
const ctx = vm.createContext({ chrome, console: { log() {} }, setTimeout, clearTimeout, Promise, URL, TextEncoder,
  OffscreenCanvas, fetch: async () => ({ blob: async () => ({}) }), createImageBitmap: async () => ({}),
  btoa: (s) => Buffer.from(s, "binary").toString("base64") });
// fastTimeouts shortens the extension's timeouts for the harness.
const fastTimeouts = {
  CDP_TIMEOUT_MS: 300, SCREENSHOT_TIMEOUT_MS: 300, SCRIPT_TIMEOUT_MS: 300, JS_TIMEOUT_MS: 300, JS_GRACE_MS: 50,
  DEBUGGER_WAIT_MS: 600, LOAD_TIMEOUT_MS: 300, MAX_WAIT_SECONDS: 0.05, MAX_REPLY_BYTES: 20000,
};
let src = fs.readFileSync(process.argv[2], "utf8");
for (const [name, v] of Object.entries(fastTimeouts)) {
  const re = new RegExp("const " + name + " = [^;]+;");
  if (!re.test(src)) { console.error("harness: no constant " + name + " in background.js"); process.exit(2); }
  src = src.replace(re, "const " + name + " = " + v + ";");
}
vm.runInContext(src, ctx);
const call = (m, p) => vm.runInContext("dispatch", ctx)(m, p || {});
const ext = (name) => vm.runInContext(name, ctx);
// linkFrom opens a tab from tabId during a gocode click, as a target=_blank link does.
async function linkFrom(tabId, url) {
  await call("computer", { action: "left_click", coordinate: [5, 5], tabId });
  const t = await chrome.tabs.create({ url, openerTabId: tabId });
  await new Promise((r) => setTimeout(r, 10));
  return t;
}
const sleepMs = (ms) => new Promise((r) => setTimeout(r, ms));
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
  // An upper-case letter in a shortcut is the letter's key: no Shift added.
  await expectKeys("ctrl+C", [CTRL_D(2), "rawKeyDown \"c\" KeyC 67 m2", "keyUp \"c\" KeyC 67 m2", CTRL_U(0)]);
  await expectKeys("ctrl+shift+C", [CTRL_D(2), SHIFT_D(10), "rawKeyDown \"C\" KeyC 67 m10", "keyUp \"C\" KeyC 67 m10", SHIFT_U(2), CTRL_U(0)]);
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
