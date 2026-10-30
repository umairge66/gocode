// gocode browser control — MV3 background service worker.
//
// LOCAL ONLY. This worker connects to the gocode native-messaging host
// ("com.gocode.gocode") over Chrome's stdio pipe and answers the commands the
// host sends. It makes NO network requests, uses NO account or token, and
// sends NO telemetry anywhere. Chrome spawns the host process on this machine
// and guarantees (via the host manifest's allowed_origins) that only THIS
// extension can reach it — that origin check is the whole security boundary,
// so no port or shared secret is needed.
//
// Protocol: the host sends { id, method, params? } commands; we reply with
// { id, result } or { id, error:{ code?, message } } echoing the id. On
// connect we announce ourselves with a Hello event { method:"hello", name,
// version }. Tab events are reserved for a later milestone.
//
// Milestone 3 adds the CDP-backed interaction verbs (computer / find /
// form_input / browser_batch). chrome.debugger is attached to the target tab
// ONLY while input is actually dispatched and ALWAYS detached afterwards (in a
// finally), so Chrome's "started debugging this browser" banner appears only
// during active control and never lingers — even when an action fails or is
// aborted. A browser_batch shares one attach across its steps and detaches once
// at the end. find and form_input use chrome.scripting (a DOM query / value set
// with input+change events), so they need no debugger and raise no banner.
//
// The --gocode-in-chrome-mcp server adds javascript_tool (CDP Runtime.evaluate,
// same attach/detach discipline) and resize_window (chrome.windows.update on a
// gocode tab's window); tabs_context accepts createIfEmpty. Every verb is
// subject to the same tab-group isolation and to the per-site policy the host
// sends (see "per-site policy" below).

const HOST_NAME = "com.gocode.gocode";
const EXTENSION_NAME = "gocode browser control";
const EXTENSION_VERSION = chrome.runtime.getManifest().version;

// read_page text cap, so a huge page cannot blow past the native-messaging
// frame limit; truncation is flagged in the result.
const MAX_PAGE_TEXT = 500000;

let port = null;
let reconnectDelay = 1000; // backoff floor (ms)
const MAX_RECONNECT_DELAY = 30000;

function log(...args) {
  // Diagnostics go to the extension's service-worker console only.
  console.log("[gocode]", ...args);
}

// setConnected shows the connection state on the toolbar icon: no badge
// while the native-messaging pipe is live, a red dot when it is not. The
// tooltip names the state either way.
function setConnected(connected) {
  try {
    chrome.action.setBadgeText({ text: connected ? "" : "●" });
    if (!connected) chrome.action.setBadgeBackgroundColor({ color: "#b42318" });
    chrome.action.setTitle({
      title: connected
        ? "gocode browser control — connected to native host"
        : "gocode browser control — NOT connected (run: gocode chrome install)",
    });
  } catch (e) {
    // chrome.action may be unavailable in some contexts; badge is cosmetic.
  }
}

function connect() {
  if (port) return;
  try {
    port = chrome.runtime.connectNative(HOST_NAME);
  } catch (e) {
    log("connectNative threw", e);
    setConnected(false);
    scheduleReconnect();
    return;
  }
  port.onMessage.addListener(onCommand);
  port.onDisconnect.addListener(onDisconnect);
  // Announce ourselves. Any command the host already queued arrives after.
  post({ method: "hello", name: EXTENSION_NAME, version: EXTENSION_VERSION });
  reconnectDelay = 1000; // reset backoff on a successful connect
  setConnected(true);
  log("connected to native host");
}

function post(msg) {
  if (!port) return;
  try {
    port.postMessage(msg);
  } catch (e) {
    log("postMessage failed", e);
  }
}

function onDisconnect() {
  const err = chrome.runtime.lastError;
  log("native port disconnected", err && err.message);
  port = null;
  setConnected(false);
  scheduleReconnect();
}

function scheduleReconnect() {
  const delay = reconnectDelay;
  reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY);
  setTimeout(connect, delay);
}

// onCommand dispatches one host command and always replies, echoing the id.
async function onCommand(msg) {
  if (!msg || typeof msg !== "object") return;
  const { id, method, params } = msg;
  // A message with a method but no id would be an event, not a command; ignore.
  if (id === undefined || id === null) return;
  try {
    const result = await dispatch(method, params || {});
    post({ id, result });
  } catch (e) {
    post({ id, error: { message: String((e && e.message) || e) } });
  }
}

async function dispatch(method, params) {
  // The site policy the host sent with this command (or the last one it sent);
  // it can only restrict what the command may do.
  const policy = await resolvePolicy(params);
  switch (method) {
    case "ping":
      return { pong: true };
    case "navigate":
      return await doNavigate(params, policy);
    case "read_page":
    case "get_page_text":
      return await doReadPage(params, policy);
    case "tabs_context":
      return await doTabsContext(params, policy);
    case "tabs_create":
      return await doTabsCreate(params, policy);
    case "tabs_close":
      return await doTabsClose(params);
    case "screenshot":
      return await doScreenshot(params, undefined, policy);
    case "computer":
      // A single computer call gets its own debugger session, always detached.
      return await withSession((ctx) => doComputer(params, ctx, policy));
    case "find":
      return await doFind(params, policy);
    case "form_input":
      return await doFormInput(params, policy);
    case "javascript_tool":
      return await withSession((ctx) => doJavascript(params, ctx, policy));
    case "resize_window":
      return await doResizeWindow(params);
    case "browser_batch":
      return await doBatch(params, policy);
    default:
      throw new Error("unknown method: " + method);
  }
}

// --- per-site policy (defence in depth) -------------------------------------
//
// The host sends the user's site rules with every command as
// params.sitePolicy = { allowed:[host], denied:[host], requireAllowed:bool }
// (rules "gocodeInChromeDomain(<host>)" in the gocode settings). The latest one
// is kept in chrome.storage.session for commands that carry none. Enforcement
// here mirrors the Go side, so a confused caller cannot get around it:
//   - a denied site is refused for every tool, reading or acting;
//   - with requireAllowed, an ACTION (navigate, click/type/key, form_input,
//     javascript_tool) on a site that is not allowed is refused with a
//     needs-approval error naming the site ("[chrome_site=<host>]"), which the
//     session turns into a permission prompt and retries once approved;
//   - pages that are not http(s) (about:blank, chrome://newtab) name no site.
// With no policy at all nothing changes (the list can only restrict).

const NON_WEB_URL_ERROR = "Can't interact with browser-internal or unparseable URLs. Navigate to a web page first.";

function cleanHostList(list) {
  if (!Array.isArray(list)) return [];
  return list.filter((h) => typeof h === "string" && h).map((h) => normalizeHost(h)).filter(Boolean);
}

function sanitizePolicy(p) {
  return {
    allowed: cleanHostList(p.allowed),
    denied: cleanHostList(p.denied),
    requireAllowed: p.requireAllowed === true,
  };
}

async function resolvePolicy(params) {
  if (params && params.sitePolicy && typeof params.sitePolicy === "object") {
    const p = sanitizePolicy(params.sitePolicy);
    await sessionSet({ sitePolicy: p });
    return p;
  }
  const { sitePolicy } = await sessionGet("sitePolicy");
  return sitePolicy ? sanitizePolicy(sitePolicy) : null;
}

function stripPort(h) {
  if (h.startsWith("[")) {
    const i = h.indexOf("]");
    return i >= 0 ? h.slice(0, i + 1) : h;
  }
  const i = h.lastIndexOf(":");
  return i >= 0 ? h.slice(0, i) : h;
}

function normalizeHost(h) {
  h = String(h).trim().toLowerCase();
  if (!h) return "";
  const name = stripPort(h);
  const port = h.slice(name.length);
  const bare = name.replace(/\.+$/, "");
  return bare ? bare + port : "";
}

// siteOf returns the site (host[:port]) a URL is on, or null when the URL is
// not an http(s) page. An http URL without a port is "<host>:80", as on the Go
// side.
function siteOf(url) {
  let u;
  try { u = new URL(url); } catch (e) { return null; }
  if (u.protocol !== "http:" && u.protocol !== "https:") return null;
  let host = u.host;
  if (!host) return null;
  if (u.protocol === "http:" && !u.port) host += ":80";
  return normalizeHost(host);
}

function hostListed(list, site) {
  return list.includes(site) || list.includes(stripPort(site));
}

// checkSite enforces the policy for one URL. kind is "act" or "read".
function checkSite(policy, url, kind) {
  if (!policy) return;
  const site = siteOf(url);
  if (!site) return;
  if (hostListed(policy.denied, site)) {
    throw new Error("gocode in Chrome is denied on " + site + ".");
  }
  if (kind === "act" && policy.requireAllowed && !hostListed(policy.allowed, site)) {
    throw new Error(
      "needs_approval: gocode in Chrome needs permission to act on " + site + " [chrome_site=" + site + "]"
    );
  }
}

async function checkTabSite(tabId, policy, kind) {
  if (!policy) return;
  const tab = await getTab(tabId);
  checkSite(policy, (tab && (tab.pendingUrl || tab.url)) || "", kind);
}

// navTarget turns a navigate url into a full URL ("example.com" -> https://).
function navTarget(url) {
  const s = String(url).trim();
  if (/^[a-z][a-z0-9+.-]*:/i.test(s) && !/^(localhost|[a-z0-9-]+\.[a-z0-9.-]+):\d+($|[/?#])/i.test(s)) return s;
  return "https://" + s;
}

// checkNavigation enforces the policy for a navigation target: with a policy,
// a non-web URL is refused outright.
function checkNavigation(policy, url) {
  if (!policy) return;
  if (!siteOf(url)) throw new Error(NON_WEB_URL_ERROR);
  checkSite(policy, url, "act");
}

// --- chrome.debugger (CDP) session lifecycle --------------------------------
//
// A "session" is a set of tabs this worker has attached the debugger to for one
// logical operation (one computer call, or one browser_batch). withSession runs
// the body and ALWAYS detaches every tab it attached — success, thrown error,
// or denied action — so the debugging banner cannot linger.

function withSession(fn) {
  const ctx = { attached: new Set() };
  return Promise.resolve()
    .then(() => fn(ctx))
    .finally(() => detachAll(ctx));
}

async function detachAll(ctx) {
  for (const tabId of Array.from(ctx.attached)) {
    try {
      await chrome.debugger.detach({ tabId });
    } catch (e) {
      // Already gone (tab closed, user cancelled) — nothing to clean up.
    }
    ctx.attached.delete(tabId);
  }
}

async function ensureAttached(ctx, tabId) {
  if (ctx.attached.has(tabId)) return;
  await chrome.debugger.attach({ tabId }, "1.3");
  ctx.attached.add(tabId);
}

// cdp sends one CDP command and resolves with its result (or rejects).
function cdp(tabId, method, params) {
  return new Promise((resolve, reject) => {
    chrome.debugger.sendCommand({ tabId }, method, params || {}, (res) => {
      const err = chrome.runtime.lastError;
      if (err) reject(new Error(err.message));
      else resolve(res);
    });
  });
}

// If the user detaches via the banner's "Cancel", drop our bookkeeping so we do
// not try to detach a dead session later.
chrome.debugger.onDetach.addListener((source) => {
  // Nothing persistent to clear (sessions are per-call), but logging helps.
  log("debugger detached", source && source.tabId);
});

// --- gocode tab group: the ONLY tabs gocode may see or touch -----------------
//
// SECURITY BOUNDARY. gocode works only inside its own "gocode" tab group: every
// tab it opens joins that group, and every command — read, find, click, type,
// screenshot, list, close — is refused for any tab outside it. The user's other
// tabs are never listed, read, captured or controlled, and there is no fallback
// to the user's active tab. This is enforced here in the extension, so a
// compromised or confused caller on the native side cannot get around it.
//
// Ownership is tracked by id, not by group title (a group the user happens to
// name "gocode" is not ours): ownedGroups holds the group ids this extension
// created, ownedTabs the tabs it opened that could not be grouped (e.g. a popup
// window, which Chrome keeps out of tab groups). A tab is gocode's when it is in
// one of those groups, or it is such an ungroupable tab gocode opened. A tab the
// user drags into the gocode group is shared with gocode; dragging a tab out of
// it takes it away. State lives in chrome.storage.session (survives service-worker
// restarts, cleared when the browser closes).
//
// Separately from ownership, openedTabs records the tabs gocode opened itself
// (tabs_create, navigate without a tab, a link followed from a gocode tab) and
// keptTabs the tabs marked keep_open. Neither grants access to a tab — only
// the ownership rules above do — they only decide what an end-of-session
// cleanup may close: tabs_close with cleanup:true refuses a tab gocode did not
// open (e.g. one the user dragged into the group) and a kept tab.
//
// Sessions: a command may carry params.sessionId, the gocode session it comes
// from. Each session gets its own "gocode" group (per window), recorded in
// groupSessions (group id -> session id), and its own current tab. tabs_context
// with a sessionId lists only that session's groups, so a session's end-of-task
// cleanup can never reach another session's tabs. A group with no recorded
// session belongs to commands that carry no sessionId. Ownership itself is not
// per session: any gocode tab stays usable by an explicit tabId.

const GROUP_TITLE = "gocode";
const TAB_GROUP_NONE = -1;

async function sessionGet(keys) {
  try {
    return await chrome.storage.session.get(keys);
  } catch (e) {
    return {};
  }
}

async function sessionSet(obj) {
  try {
    await chrome.storage.session.set(obj);
  } catch (e) {}
}

async function ownership() {
  const { ownedGroups, ownedTabs } = await sessionGet(["ownedGroups", "ownedTabs"]);
  return { groups: ownedGroups || [], tabs: ownedTabs || [] };
}

async function rememberOwnedTab(tabId) {
  const { tabs } = await ownership();
  if (!tabs.includes(tabId)) await sessionSet({ ownedTabs: tabs.concat([tabId]) });
}

async function tabIdList(key) {
  const o = await sessionGet(key);
  return Array.isArray(o[key]) ? o[key] : [];
}

// rememberOpenedTab records that gocode opened tabId itself.
async function rememberOpenedTab(tabId) {
  const opened = await tabIdList("openedTabs");
  if (!opened.includes(tabId)) await sessionSet({ openedTabs: opened.concat([tabId]) });
}

// setKeepOpen marks tabId to stay open after the session ends (keep true) or
// clears that mark.
async function setKeepOpen(tabId, keep) {
  const kept = await tabIdList("keptTabs");
  if (keep && !kept.includes(tabId)) await sessionSet({ keptTabs: kept.concat([tabId]) });
  if (!keep && kept.includes(tabId)) await sessionSet({ keptTabs: kept.filter((id) => id !== tabId) });
}

// applyKeepOpen applies a command's keep_open parameter (when it carries one)
// to tabId.
async function applyKeepOpen(params, tabId) {
  if (tabId == null || !params || typeof params.keep_open !== "boolean") return;
  await setKeepOpen(tabId, params.keep_open);
}

async function rememberOwnedGroup(groupId) {
  const { groups } = await ownership();
  if (!groups.includes(groupId)) await sessionSet({ ownedGroups: groups.concat([groupId]) });
}

// sessionOf returns the session id a command carries ("" when none).
function sessionOf(params) {
  const s = params && params.sessionId;
  return typeof s === "string" && s.length <= 256 ? s : "";
}

async function groupSessionMap() {
  const { groupSessions } = await sessionGet("groupSessions");
  return groupSessions && typeof groupSessions === "object" ? groupSessions : {};
}

// groupSession returns the session that owns gocode group groupId ("" when the
// group was opened by commands without a session).
async function groupSession(groupId) {
  const m = await groupSessionMap();
  return m[String(groupId)] || "";
}

async function rememberGroupSession(groupId, sid) {
  if (!sid) return;
  const m = await groupSessionMap();
  m[String(groupId)] = sid;
  await sessionSet({ groupSessions: m });
}

// sessionGroupIds returns the ids of gocode's open groups that belong to sid.
async function sessionGroupIds(sid) {
  const own = await ownership();
  const m = await groupSessionMap();
  const out = [];
  for (const gid of own.groups) {
    if ((m[String(gid)] || "") !== sid) continue;
    try {
      await chrome.tabGroups.get(gid);
      out.push(gid);
    } catch (e) {
      // The group was closed.
    }
  }
  return out;
}

async function setCurrentTab(tabId, sid) {
  if (tabId == null) return;
  if (!sid) {
    await sessionSet({ currentTabId: tabId });
    return;
  }
  const { currentTabs } = await sessionGet("currentTabs");
  const m = currentTabs && typeof currentTabs === "object" ? currentTabs : {};
  m[sid] = tabId;
  await sessionSet({ currentTabs: m });
}

async function currentTabFor(sid) {
  if (!sid) {
    const { currentTabId } = await sessionGet("currentTabId");
    return currentTabId;
  }
  const { currentTabs } = await sessionGet("currentTabs");
  return currentTabs && typeof currentTabs === "object" ? currentTabs[sid] : undefined;
}

// isOwnedTab reports whether a chrome.tabs.Tab belongs to gocode.
function isOwnedTab(tab, own) {
  if (!tab) return false;
  if (own.groups.includes(tab.groupId)) return true;
  return own.tabs.includes(tab.id) && tab.groupId === TAB_GROUP_NONE;
}

// ownedTabList returns gocode's tabs (chrome.tabs.Tab objects). With a session
// id it returns that session's tabs: the tabs in its groups, plus gocode's
// ungroupable tabs (which belong to no group).
async function ownedTabList(sid) {
  const own = await ownership();
  const all = await chrome.tabs.query({});
  const mine = all.filter((t) => isOwnedTab(t, own));
  if (!sid) return mine;
  const groups = await sessionGroupIds(sid);
  return mine.filter((t) => t.groupId === TAB_GROUP_NONE || groups.includes(t.groupId));
}

// assertOwned returns the tab when it is gocode's, and refuses any other tab.
async function assertOwned(tabId) {
  const tab = await getTab(tabId);
  if (!tab) throw new Error("tab " + tabId + " does not exist");
  if (!isOwnedTab(tab, await ownership())) {
    throw new Error(
      "tab " + tabId + " is not a gocode tab — gocode can only use tabs in its \"" +
        GROUP_TITLE + "\" tab group. Open one with tabs_create or navigate."
    );
  }
  return tab;
}

// addToGocodeGroup records the tab as gocode's (and as opened by gocode — it
// is only called for tabs gocode opened) and puts it in a gocode group:
// preferGroupId when that is one of gocode's open groups, else session sid's
// group in the tab's window, else a new group owned by sid.
async function addToGocodeGroup(tabId, sid, preferGroupId) {
  sid = sid || "";
  await rememberOwnedTab(tabId);
  await rememberOpenedTab(tabId);
  try {
    const t = await getTab(tabId);
    const windowId = t ? t.windowId : undefined;
    const { groups } = await ownership();
    let groupId;
    if (preferGroupId != null && groups.includes(preferGroupId)) {
      try {
        const g = await chrome.tabGroups.get(preferGroupId);
        groupId = g.id;
      } catch (e) {}
    }
    for (const gid of groupId == null ? groups : []) {
      let g = null;
      try { g = await chrome.tabGroups.get(gid); } catch (e) {}
      if (g && (windowId == null || g.windowId === windowId) && (await groupSession(g.id)) === sid) {
        groupId = g.id;
        break;
      }
    }
    if (groupId != null) {
      await chrome.tabs.group({ tabIds: [tabId], groupId });
    } else {
      groupId = await chrome.tabs.group({ tabIds: [tabId] });
      await chrome.tabGroups.update(groupId, { title: GROUP_TITLE, color: "green" });
      await rememberOwnedGroup(groupId);
      await rememberGroupSession(groupId, sid);
    }
    // Grouped: from now on the group decides. If the user drags the tab out
    // of the gocode group, it is no longer gocode's.
    const { tabs } = await ownership();
    await sessionSet({ ownedTabs: tabs.filter((id) => id !== tabId) });
    log("grouped tab", tabId, "-> gocode group", groupId);
    return groupId;
  } catch (e) {
    // The tab stays gocode's (ownedTabs) even if it cannot be grouped, e.g. a
    // popup window, which Chrome does not allow in tab groups.
    log("group tab failed:", (e && e.message) || e);
    return null;
  }
}

// gocodeTabOrNull returns the session's current gocode tab (or its most recent
// tab), or null when the session has no tab.
async function gocodeTabOrNull(sid) {
  const current = await currentTabFor(sid);
  const own = await ownership();
  if (current != null && isOwnedTab(await getTab(current), own)) return current;
  const tabs = await ownedTabList(sid);
  return tabs.length ? tabs[tabs.length - 1].id : null;
}

// resolveTabId returns the tab a command acts on: the given tab if it is
// gocode's (any other tab is refused), else gocode's current tab. There is NO
// fallback to the user's active tab.
async function resolveTabId(tabId, sid) {
  if (tabId !== undefined && tabId !== null) {
    await assertOwned(tabId);
    return tabId;
  }
  const own = await gocodeTabOrNull(sid);
  if (own == null) throw new Error("no gocode tab yet — open one with navigate or tabs_create");
  return own;
}

// A tab opened FROM a gocode tab (a link with target=_blank, window.open) joins
// the gocode group, so following a link keeps working inside the group. Tabs
// the user opens themselves are never touched.
chrome.tabs.onCreated.addListener(async (tab) => {
  if (tab.openerTabId == null) return;
  const opener = await getTab(tab.openerTabId);
  const own = await ownership();
  if (!isOwnedTab(opener, own)) return;
  const grouped = own.groups.includes(opener.groupId);
  const sid = grouped ? await groupSession(opener.groupId) : "";
  await addToGocodeGroup(tab.id, sid, grouped ? opener.groupId : undefined);
});

chrome.tabs.onRemoved.addListener(async (tabId) => {
  const { tabs } = await ownership();
  if (tabs.includes(tabId)) await sessionSet({ ownedTabs: tabs.filter((id) => id !== tabId) });
  const opened = await tabIdList("openedTabs");
  if (opened.includes(tabId)) await sessionSet({ openedTabs: opened.filter((id) => id !== tabId) });
  const kept = await tabIdList("keptTabs");
  if (kept.includes(tabId)) await sessionSet({ keptTabs: kept.filter((id) => id !== tabId) });
});

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// getTab promisifies chrome.tabs.get (null if the tab is gone).
function getTab(tabId) {
  return new Promise((resolve) =>
    chrome.tabs.get(tabId, (t) => resolve(chrome.runtime.lastError ? null : t))
  );
}

// waitForTabLoad resolves when the tab reaches status "complete" (or the
// timeout fires), so navigate/tabs_create return a settled, readable page
// instead of a still-loading one.
function waitForTabLoad(tabId, timeoutMs) {
  return new Promise((resolve) => {
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      try { chrome.tabs.onUpdated.removeListener(listener); } catch (e) {}
      resolve();
    };
    const listener = (id, info) => {
      if (id === tabId && info.status === "complete") finish();
    };
    chrome.tabs.onUpdated.addListener(listener);
    chrome.tabs.get(tabId, (t) => {
      if (!chrome.runtime.lastError && t && t.status === "complete") finish();
    });
    setTimeout(finish, timeoutMs || 15000);
  });
}

// viewportInfo returns the page's CSS viewport size and device-pixel-ratio.
// CDP input coordinates are in CSS pixels, so this lets us keep the screenshot
// coordinate space (which captureVisibleTab renders at dpr) aligned with it.
async function viewportInfo(tabId) {
  try {
    const r = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => ({
        dpr: window.devicePixelRatio || 1,
        w: window.innerWidth,
        h: window.innerHeight,
      }),
    });
    return (r && r[0] && r[0].result) || { dpr: 1, w: 0, h: 0 };
  } catch (e) {
    return { dpr: 1, w: 0, h: 0 };
  }
}

// showCursor injects (once) and moves a PERSISTENT, visible arrow cursor at CSS
// coords [x,y] so the user can see where gocode's synthetic mouse is. It is
// pointer-events:none (never intercepts the real click) at the top z-index, and
// does NOT mark/ripple the click location. Uses chrome.scripting so it works
// with or without the debugger attached and is purely visual. It is re-created
// automatically after a navigation (which wipes the page DOM).
async function showCursor(tabId, x, y) {
  try {
    await chrome.scripting.executeScript({
      target: { tabId },
      args: [x, y],
      func: (x, y) => {
        const ID = "__gocode_cursor__";
        const root = document.documentElement || document.body;
        if (!root) return;
        let c = document.getElementById(ID);
        if (!c) {
          c = document.createElement("div");
          c.id = ID;
          c.style.cssText =
            "position:fixed;left:0;top:0;z-index:2147483647;" +
            "pointer-events:none;will-change:transform;" +
            "transition:transform .08s linear;" +
            "filter:drop-shadow(0 1px 1px rgba(0,0,0,.45));";
          // A classic arrow pointer so it reads unmistakably as a cursor.
          c.innerHTML =
            '<svg width="22" height="22" viewBox="0 0 24 24" ' +
            'xmlns="http://www.w3.org/2000/svg">' +
            '<path d="M5 3 L5 20 L10 15 L13.5 22 L16.5 20.5 L13 14 L20 14 Z" ' +
            'fill="#1a7f37" stroke="#ffffff" stroke-width="1.3" ' +
            'stroke-linejoin="round"/></svg>';
          root.appendChild(c);
        }
        // The arrow's hotspot is its top-left tip, so translate straight to
        // [x,y] (no centering offset).
        c.style.transform = "translate(" + x + "px," + y + "px)";
      },
    });
  } catch (e) {
    // Cursor overlay is cosmetic; never fail an action because of it.
  }
}

// dataUrlToCssPixels downscales a captureVisibleTab PNG (rendered at dpr) to the
// CSS viewport size, so screenshot pixel coords equal CSS/CDP coords. Runs in
// the service worker via OffscreenCanvas (no FileReader, which SWs lack).
async function dataUrlToCssPixels(dataUrl, w, h) {
  const blob = await (await fetch(dataUrl)).blob();
  const bmp = await createImageBitmap(blob);
  const canvas = new OffscreenCanvas(w, h);
  const cx = canvas.getContext("2d");
  cx.drawImage(bmp, 0, 0, w, h);
  const out = await canvas.convertToBlob({ type: "image/png" });
  const bytes = new Uint8Array(await out.arrayBuffer());
  let bin = "";
  const CH = 0x8000;
  for (let i = 0; i < bytes.length; i += CH) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + CH));
  }
  return "data:image/png;base64," + btoa(bin);
}

// --- computer (CDP mouse/keyboard input + screenshots) ----------------------

// --- keyboard: key names -> CDP Input.dispatchKeyEvent -----------------------
//
// The computer tool's `key` action takes space-separated keys, each a single
// key or a "+"-joined chord ("Backspace Backspace Delete", "ctrl+a",
// "ctrl+shift+Tab", "alt+x", "shift+Tab", "Numpad5", "cmd+a"), and `repeat`
// presses the whole sequence that many times (1-100). Page zoom shortcuts
// ("cmd+=", "ctrl+-", "cmd+0", …) are refused: use the zoom action instead.
//
// Each key becomes the CDP events a real keyboard produces:
//   - modifiers are pressed as real keys first (ShiftLeft/ControlLeft/AltLeft/
//     MetaLeft, location 1) and released last in reverse order; every event
//     carries the modifier bitmask of the keys held at that moment (a modifier's
//     own keyDown includes its bit, its keyUp no longer does);
//   - the base key gets its DOM `key` (the shifted character when Shift is held,
//     e.g. shift+a -> "A"), physical `code`, `windowsVirtualKeyCode`, `location`
//     (3 for the numeric keypad) and — only when it produces a character and no
//     Ctrl/Alt/Meta is held — `text`, sent as "keyDown"; a key that produces no
//     character is sent as "rawKeyDown";
//   - on macOS, cmd+a/c/x/v/z (and cmd+shift+z) also carry the editing command,
//     since Chrome on macOS performs those through `commands`, not the key event.

// CDP modifier bits.
const MOD_ALT = 1, MOD_CTRL = 2, MOD_META = 4, MOD_SHIFT = 8;

const IS_MAC = /Mac/i.test((typeof navigator !== "undefined" && navigator && navigator.userAgent) || "");

// Modifier keys: name -> CDP descriptor + its bit. location 1 = the left key.
const MOD_KEYS = {
  shift: { key: "Shift", code: "ShiftLeft", vk: 16, bit: MOD_SHIFT, location: 1 },
  ctrl: { key: "Control", code: "ControlLeft", vk: 17, bit: MOD_CTRL, location: 1 },
  control: { key: "Control", code: "ControlLeft", vk: 17, bit: MOD_CTRL, location: 1 },
  ctl: { key: "Control", code: "ControlLeft", vk: 17, bit: MOD_CTRL, location: 1 },
  alt: { key: "Alt", code: "AltLeft", vk: 18, bit: MOD_ALT, location: 1 },
  option: { key: "Alt", code: "AltLeft", vk: 18, bit: MOD_ALT, location: 1 },
  opt: { key: "Alt", code: "AltLeft", vk: 18, bit: MOD_ALT, location: 1 },
  meta: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
  cmd: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
  command: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
  win: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
  windows: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
  super: { key: "Meta", code: "MetaLeft", vk: 91, bit: MOD_META, location: 1 },
};
function modKey(name) {
  return MOD_KEYS[String(name).toLowerCase()] || null;
}

// modifierMask parses a click's `modifiers` ("ctrl", "shift+alt", "cmd", "win")
// into the CDP bitmask.
function modifierMask(spec) {
  let mask = 0;
  if (!spec) return 0;
  for (const part of String(spec).split("+")) {
    const m = modKey(part.trim());
    if (m) mask |= m.bit;
  }
  return mask;
}

// Named (non-printing or whitespace) keys. Lookup is case-insensitive and
// ignores "_" so DOM names ("ArrowUp", "PageDown"), xdotool names ("Return",
// "BackSpace", "Page_Up") and short forms ("esc", "pgdn") all resolve.
const KEY_DEFS = {
  enter: { key: "Enter", code: "Enter", vk: 13, text: "\r" },
  return: { key: "Enter", code: "Enter", vk: 13, text: "\r" },
  tab: { key: "Tab", code: "Tab", vk: 9 },
  backspace: { key: "Backspace", code: "Backspace", vk: 8 },
  delete: { key: "Delete", code: "Delete", vk: 46 },
  del: { key: "Delete", code: "Delete", vk: 46 },
  escape: { key: "Escape", code: "Escape", vk: 27 },
  esc: { key: "Escape", code: "Escape", vk: 27 },
  space: { key: " ", code: "Space", vk: 32, text: " " },
  up: { key: "ArrowUp", code: "ArrowUp", vk: 38 },
  down: { key: "ArrowDown", code: "ArrowDown", vk: 40 },
  left: { key: "ArrowLeft", code: "ArrowLeft", vk: 37 },
  right: { key: "ArrowRight", code: "ArrowRight", vk: 39 },
  arrowup: { key: "ArrowUp", code: "ArrowUp", vk: 38 },
  arrowdown: { key: "ArrowDown", code: "ArrowDown", vk: 40 },
  arrowleft: { key: "ArrowLeft", code: "ArrowLeft", vk: 37 },
  arrowright: { key: "ArrowRight", code: "ArrowRight", vk: 39 },
  home: { key: "Home", code: "Home", vk: 36 },
  end: { key: "End", code: "End", vk: 35 },
  pageup: { key: "PageUp", code: "PageUp", vk: 33 },
  pagedown: { key: "PageDown", code: "PageDown", vk: 34 },
  pgup: { key: "PageUp", code: "PageUp", vk: 33 },
  pgdn: { key: "PageDown", code: "PageDown", vk: 34 },
  insert: { key: "Insert", code: "Insert", vk: 45 },
  ins: { key: "Insert", code: "Insert", vk: 45 },
  capslock: { key: "CapsLock", code: "CapsLock", vk: 20 },
  numlock: { key: "NumLock", code: "NumLock", vk: 144 },
  scrolllock: { key: "ScrollLock", code: "ScrollLock", vk: 145 },
  scrlk: { key: "ScrollLock", code: "ScrollLock", vk: 145 },
  printscreen: { key: "PrintScreen", code: "PrintScreen", vk: 44 },
  print: { key: "PrintScreen", code: "PrintScreen", vk: 44 },
  prtsc: { key: "PrintScreen", code: "PrintScreen", vk: 44 },
  pause: { key: "Pause", code: "Pause", vk: 19 },
  contextmenu: { key: "ContextMenu", code: "ContextMenu", vk: 93 },
  menu: { key: "ContextMenu", code: "ContextMenu", vk: 93 },
  volumemute: { key: "AudioVolumeMute", code: "AudioVolumeMute", vk: 173 },
  mute: { key: "AudioVolumeMute", code: "AudioVolumeMute", vk: 173 },
  volumeup: { key: "AudioVolumeUp", code: "AudioVolumeUp", vk: 175 },
  volumedown: { key: "AudioVolumeDown", code: "AudioVolumeDown", vk: 174 },
  mediaplaypause: { key: "MediaPlayPause", code: "MediaPlayPause", vk: 179 },
  medianext: { key: "MediaTrackNext", code: "MediaTrackNext", vk: 176 },
  mediaprev: { key: "MediaTrackPrevious", code: "MediaTrackPrevious", vk: 177 },
  mediastop: { key: "MediaStop", code: "MediaStop", vk: 178 },
};
for (let i = 1; i <= 24; i++) {
  KEY_DEFS["f" + i] = { key: "F" + i, code: "F" + i, vk: 111 + i };
}
function namedKey(name) {
  const n = String(name).toLowerCase().replace(/_/g, "");
  return KEY_DEFS[n] || null;
}

// Numeric keypad: name -> [code, key, windowsVirtualKeyCode, text]. Sent with
// location 3 so the page sees event.code "Numpad5" / event.location 3, not the
// main row. Accepts "Numpad5", "num5", "kp_5", "numpad+", "NumpadAdd",
// "kp_add", "NumpadEnter", "KP_Enter", …
const NUMPAD = {
  numpad0: ["Numpad0", "0", 96, "0"], numpad1: ["Numpad1", "1", 97, "1"],
  numpad2: ["Numpad2", "2", 98, "2"], numpad3: ["Numpad3", "3", 99, "3"],
  numpad4: ["Numpad4", "4", 100, "4"], numpad5: ["Numpad5", "5", 101, "5"],
  numpad6: ["Numpad6", "6", 102, "6"], numpad7: ["Numpad7", "7", 103, "7"],
  numpad8: ["Numpad8", "8", 104, "8"], numpad9: ["Numpad9", "9", 105, "9"],
  "numpad.": ["NumpadDecimal", ".", 110, "."], numpaddecimal: ["NumpadDecimal", ".", 110, "."],
  "numpad+": ["NumpadAdd", "+", 107, "+"], numpadadd: ["NumpadAdd", "+", 107, "+"],
  "numpad-": ["NumpadSubtract", "-", 109, "-"], numpadsubtract: ["NumpadSubtract", "-", 109, "-"],
  "numpad*": ["NumpadMultiply", "*", 106, "*"], numpadmultiply: ["NumpadMultiply", "*", 106, "*"],
  "numpad/": ["NumpadDivide", "/", 111, "/"], numpaddivide: ["NumpadDivide", "/", 111, "/"],
  numpadenter: ["NumpadEnter", "Enter", 13, "\r"],
  numpadequal: ["NumpadEqual", "=", 187, "="],
};
function numpadKey(name) {
  let n = String(name).toLowerCase().replace(/[\s_]/g, "");
  if (n.startsWith("kp")) n = "numpad" + n.slice(2);
  else if (n.startsWith("num") && !n.startsWith("numpad") && n !== "numlock") n = "numpad" + n.slice(3);
  const e = NUMPAD[n];
  if (!e) return null;
  return { key: e[1], code: e[0], vk: e[2], text: e[3], location: 3 };
}

// US-layout physical keys for printable punctuation: char -> [code, vk]. These
// VK codes (186, 187, …) are what the DOM reports as event.keyCode.
const PUNCT_KEYS = {
  ";": ["Semicolon", 186], "=": ["Equal", 187], ",": ["Comma", 188],
  "-": ["Minus", 189], ".": ["Period", 190], "/": ["Slash", 191],
  "`": ["Backquote", 192], "[": ["BracketLeft", 219], "\\": ["Backslash", 220],
  "]": ["BracketRight", 221], "'": ["Quote", 222], " ": ["Space", 32],
};
// Shifted char -> the unshifted char on the same physical key (US layout).
const SHIFT_BASE = {
  "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7",
  "*": "8", "(": "9", ")": "0", "_": "-", "+": "=", "{": "[", "}": "]",
  "|": "\\", ":": ";", "\"": "'", "<": ",", ">": ".", "?": "/", "~": "`",
};
// Unshifted char -> what Shift produces (US layout).
const SHIFT_OF = {};
for (const k of Object.keys(SHIFT_BASE)) SHIFT_OF[SHIFT_BASE[k]] = k;
function shiftChar(ch) {
  if (/^[a-z]$/.test(ch)) return ch.toUpperCase();
  return SHIFT_OF[ch] || ch;
}

// Names that stand for a printable character.
const CHAR_ALIASES = {
  backtick: "`", grave: "`", backquote: "`", tilde: "~",
  minus: "-", plus: "+", equals: "=", equal: "=",
  comma: ",", period: ".", dot: ".", slash: "/", backslash: "\\",
  semicolon: ";", colon: ":", quote: "'", apostrophe: "'",
  bracketleft: "[", bracketright: "]",
};

// printableKey describes one character's physical key: code + vk of the
// unshifted key and whether the character itself needs Shift (US layout).
function printableKey(ch) {
  let shift = false;
  let base = ch;
  if (/^[A-Z]$/.test(ch)) { shift = true; base = ch.toLowerCase(); }
  else if (SHIFT_BASE[ch]) { shift = true; base = SHIFT_BASE[ch]; }
  let code;
  let vk;
  if (/^[a-z]$/.test(base)) { code = "Key" + base.toUpperCase(); vk = base.toUpperCase().charCodeAt(0); }
  else if (/^[0-9]$/.test(base)) { code = "Digit" + base; vk = base.charCodeAt(0); }
  else if (PUNCT_KEYS[base]) { code = PUNCT_KEYS[base][0]; vk = PUNCT_KEYS[base][1]; }
  return { base, shift, code, vk };
}

const ZOOM_SHORTCUT_ERROR =
  'Page zoom shortcuts (e.g. "cmd+=", "ctrl+-", "cmd+0") are not supported. ' +
  "Use the computer tool's zoom action to magnify a region of the page instead.";

// macOS editing commands for cmd+<key> (Chrome on macOS runs these through the
// `commands` field rather than the key event itself).
const MAC_COMMANDS = { a: "selectAll", c: "copy", x: "cut", v: "paste", z: "undo" };

// resolveBaseKey turns the non-modifier part of a chord into a descriptor
// { key, code, vk, text, location, needShift }. `held` is the modifier mask
// held for it. Returns null for an unknown multi-character name.
function resolveBaseKey(name, held) {
  const np = numpadKey(name);
  if (np) return { ...np, needShift: false };
  if (String(name).length > 1) {
    const d = namedKey(name);
    if (d) return { key: d.key, code: d.code, vk: d.vk, text: d.text, location: 0, needShift: false };
  }
  const ch = String(name).length === 1 ? String(name) : CHAR_ALIASES[String(name).toLowerCase()];
  if (ch === undefined) return null;
  const info = printableKey(ch);
  // A character that needs Shift (e.g. "A", "!") gets Shift pressed for it
  // unless it is already held. With Shift held, an unshifted name produces
  // its shifted character (shift+a -> "A", shift+1 -> "!").
  const shifted = info.shift || (held & MOD_SHIFT) !== 0;
  const produced = info.shift ? ch : shifted ? shiftChar(ch) : ch;
  return {
    key: produced,
    code: info.code,
    vk: info.vk,
    text: produced,
    location: 0,
    needShift: info.shift && (held & MOD_SHIFT) === 0,
    base: info.base,
  };
}

// isZoomShortcut reports ctrl/cmd + "=", "+", "-", "_", "0" (main row or keypad).
function isZoomShortcut(mods, base) {
  if (!(mods & (MOD_CTRL | MOD_META))) return false;
  if (base.location === 3) return ["NumpadAdd", "NumpadSubtract", "Numpad0"].includes(base.code);
  return ["Equal", "Minus", "Digit0"].includes(base.code);
}

// planChord parses one token into { mods: [modifier descriptors], base } or
// throws for an unknown modifier / key or a zoom shortcut. A token that is only
// modifiers ("shift", "ctrl+shift") has base null.
function planChord(token) {
  token = String(token).trim();
  if (!token) return null;
  // Whole-token keys first, so "+", "numpad+", "kp_add" are not split on "+".
  if (token.length === 1 || numpadKey(token) || namedKey(token) || CHAR_ALIASES[token.toLowerCase()]) {
    const base = resolveBaseKey(token, 0);
    return { mods: [], base };
  }
  let keyName;
  let modNames;
  if (token.endsWith("+")) {
    keyName = "+";
    modNames = token.slice(0, -1).split("+");
  } else {
    const segs = token.split("+");
    keyName = segs.pop();
    modNames = segs;
  }
  const mods = [];
  let mask = 0;
  const addMod = (name) => {
    const m = modKey(name.trim());
    if (!m) return false;
    if (!(mask & m.bit)) { mods.push(m); mask |= m.bit; }
    return true;
  };
  for (const name of modNames) {
    if (!name.trim()) continue;
    if (!addMod(name)) throw new Error('Unknown modifier "' + name.trim() + '" in key "' + token + '"');
  }
  // A chord made only of modifiers ("ctrl+shift"): the last one is a key too.
  if (modKey(keyName)) {
    addMod(keyName);
    return { mods, base: null };
  }
  const base = resolveBaseKey(keyName, mask);
  if (!base) throw new Error('Unknown key "' + keyName + '" in "' + token + '"');
  if (isZoomShortcut(mask, base)) throw new Error(ZOOM_SHORTCUT_ERROR);
  return { mods, base };
}

function keyParams(type, d, mask, text) {
  const p = { type, modifiers: mask, key: d.key };
  if (d.code) p.code = d.code;
  if (d.vk) { p.windowsVirtualKeyCode = d.vk; p.nativeVirtualKeyCode = d.vk; }
  if (d.location) p.location = d.location;
  if (d.location === 3) p.isKeypad = true;
  if (text != null) { p.text = text; p.unmodifiedText = text; }
  return p;
}

async function sendKeyEvent(tabId, params) {
  await cdp(tabId, "Input.dispatchKeyEvent", params);
}

// pressChord sends the CDP events for one planned chord.
async function pressChord(tabId, plan) {
  let mask = 0;
  const held = [];
  const down = async (m) => {
    mask |= m.bit;
    held.push(m);
    await sendKeyEvent(tabId, keyParams("rawKeyDown", m, mask, null));
  };
  for (const m of plan.mods) await down(m);
  const base = plan.base;
  if (base) {
    if (base.needShift) await down(MOD_KEYS.shift);
    // Ctrl/Alt/Meta suppress the character (shortcuts insert nothing).
    const produces = base.text != null && (mask & (MOD_CTRL | MOD_ALT | MOD_META)) === 0;
    const p = keyParams(produces ? "keyDown" : "rawKeyDown", base, mask, produces ? base.text : null);
    if (IS_MAC && (mask & MOD_META) && !(mask & (MOD_CTRL | MOD_ALT)) && base.base && MAC_COMMANDS[base.base]) {
      p.commands = [base.base === "z" && (mask & MOD_SHIFT) ? "redo" : MAC_COMMANDS[base.base]];
    }
    await sendKeyEvent(tabId, p);
    await sendKeyEvent(tabId, keyParams("keyUp", base, mask, null));
  }
  for (let i = held.length - 1; i >= 0; i--) {
    mask &= ~held[i].bit;
    await sendKeyEvent(tabId, keyParams("keyUp", held[i], mask, null));
  }
}

// pressKeys presses a space-separated key sequence `repeat` times. Every token
// is parsed before any event is sent, so a bad token presses nothing. An
// unknown multi-character name with no modifiers is inserted as text (be
// forgiving; prefer the type action for text).
async function pressKeys(tabId, text, repeat) {
  const tokens = String(text).trim().split(/\s+/).filter(Boolean);
  if (tokens.length === 0) throw new Error("key action requires text (the key(s) to press)");
  const n = repeat === undefined || repeat === null ? 1 : Number(repeat);
  if (!Number.isInteger(n) || n < 1 || n > 100) throw new Error("repeat must be an integer between 1 and 100");
  const plans = tokens.map((t) => {
    try {
      return planChord(t);
    } catch (e) {
      if (/^Unknown key/.test(e.message) && !t.includes("+")) return { literal: t };
      throw e;
    }
  });
  for (let r = 0; r < n; r++) {
    for (const plan of plans) {
      if (!plan) continue;
      if (plan.literal !== undefined) await typeText(tabId, plan.literal);
      else await pressChord(tabId, plan);
    }
  }
}

async function mouseEvent(tabId, type, x, y, extra) {
  await cdp(tabId, "Input.dispatchMouseEvent", { type, x, y, ...(extra || {}) });
}

async function mouseClick(tabId, coord, button, clickCount, modifiers) {
  if (!coord) throw new Error("click requires a coordinate or ref");
  const [x, y] = coord;
  const mods = modifierMask(modifiers);
  // CDP `buttons` is the bitmask of buttons CURRENTLY down: left=1, right=2,
  // middle=4. It is set on press and cleared (0) on release.
  const btMask = button === "right" ? 2 : button === "middle" ? 4 : 1;
  await showCursor(tabId, x, y);
  await mouseEvent(tabId, "mouseMoved", x, y, { modifiers: mods, buttons: 0 });
  await mouseEvent(tabId, "mousePressed", x, y, { button, buttons: btMask, clickCount, modifiers: mods });
  await mouseEvent(tabId, "mouseReleased", x, y, { button, buttons: 0, clickCount, modifiers: mods });
}

async function mouseMove(tabId, coord) {
  if (!coord) throw new Error("mouse move requires a coordinate or ref");
  await showCursor(tabId, coord[0], coord[1]);
  await mouseEvent(tabId, "mouseMoved", coord[0], coord[1], { buttons: 0 });
}

async function mouseDrag(tabId, start, end) {
  if (!start || !end) throw new Error("left_click_drag requires start_coordinate and coordinate");
  await showCursor(tabId, start[0], start[1]);
  await mouseEvent(tabId, "mouseMoved", start[0], start[1], { buttons: 0 });
  await mouseEvent(tabId, "mousePressed", start[0], start[1], { button: "left", buttons: 1, clickCount: 1 });
  await showCursor(tabId, end[0], end[1]);
  await mouseEvent(tabId, "mouseMoved", end[0], end[1], { button: "left", buttons: 1 });
  await mouseEvent(tabId, "mouseReleased", end[0], end[1], { button: "left", buttons: 0, clickCount: 1 });
}

async function mouseScroll(tabId, coord, direction, amount) {
  const [x, y] = coord || (await viewportCenter(tabId));
  const ticks = (amount || 3) * 100;
  let deltaX = 0, deltaY = 0;
  switch (direction) {
    case "up": deltaY = -ticks; break;
    case "down": deltaY = ticks; break;
    case "left": deltaX = -ticks; break;
    case "right": deltaX = ticks; break;
    default: deltaY = ticks;
  }
  await mouseEvent(tabId, "mouseWheel", x, y, { deltaX, deltaY });
}

async function viewportCenter(tabId) {
  const r = await chrome.scripting.executeScript({
    target: { tabId },
    func: () => [Math.floor(window.innerWidth / 2), Math.floor(window.innerHeight / 2)],
  });
  return (r && r[0] && r[0].result) || [0, 0];
}

async function typeText(tabId, text) {
  await cdp(tabId, "Input.insertText", { text: String(text) });
}

// refCenter resolves an element ref (a data-gocode-ref value set by find, or a
// CSS selector fallback) to its viewport-center [x, y].
async function refCenter(tabId, ref) {
  const r = await chrome.scripting.executeScript({
    target: { tabId },
    args: [ref],
    func: (ref) => {
      let el = document.querySelector('[data-gocode-ref="' + ref + '"]');
      if (!el) { try { el = document.querySelector(ref); } catch (e) {} }
      if (!el) return null;
      const b = el.getBoundingClientRect();
      return [Math.round(b.left + b.width / 2), Math.round(b.top + b.height / 2)];
    },
  });
  const c = r && r[0] && r[0].result;
  if (!c) throw new Error("ref not found: " + ref);
  return c;
}

async function resolveCoord(tabId, params) {
  if (Array.isArray(params.coordinate)) return params.coordinate;
  if (params.ref) return await refCenter(tabId, params.ref);
  return null;
}

async function scrollToRef(tabId, ref) {
  if (!ref) throw new Error("scroll_to requires a ref");
  const r = await chrome.scripting.executeScript({
    target: { tabId },
    args: [ref],
    func: (ref) => {
      let el = document.querySelector('[data-gocode-ref="' + ref + '"]');
      if (!el) { try { el = document.querySelector(ref); } catch (e) {} }
      if (!el) return false;
      el.scrollIntoView({ block: "center", inline: "center" });
      return true;
    },
  });
  if (!(r && r[0] && r[0].result)) throw new Error("ref not found: " + ref);
}

// Computer actions that only observe the page (site check "read").
const OBSERVE_ACTIONS = new Set(["screenshot", "zoom", "wait"]);

async function doComputer(params, ctx, policy) {
  const action = params.action;
  if (!action) throw new Error("computer requires an action");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(tabId, policy, OBSERVE_ACTIONS.has(action) ? "read" : "act");

  // Screenshot/zoom capture via CDP on the (possibly background) gocode tab,
  // reusing this call's debugger session. zoom's region crop is approximated by
  // a full viewport capture.
  if (action === "screenshot" || action === "zoom") {
    return await captureTab(tabId, ctx);
  }
  if (action === "wait") {
    await sleep(Math.max(0, (params.duration || 1) * 1000));
    return { action, ok: true };
  }
  if (action === "scroll_to") {
    await scrollToRef(tabId, params.ref);
    return { action, ok: true };
  }

  // Input actions require CDP; attach via the shared session.
  await ensureAttached(ctx, tabId);
  const coord = await resolveCoord(tabId, params);
  switch (action) {
    case "left_click":
    case "click":
      await mouseClick(tabId, coord, "left", 1, params.modifiers);
      break;
    case "right_click":
      await mouseClick(tabId, coord, "right", 1, params.modifiers);
      break;
    case "double_click":
      await mouseClick(tabId, coord, "left", 2, params.modifiers);
      break;
    case "triple_click":
      await mouseClick(tabId, coord, "left", 3, params.modifiers);
      break;
    case "hover":
    case "mouse_move":
      await mouseMove(tabId, coord);
      break;
    case "left_click_drag":
      await mouseDrag(tabId, params.start_coordinate, coord);
      break;
    case "scroll":
      await mouseScroll(tabId, coord, params.scroll_direction, params.scroll_amount);
      break;
    case "type":
      await typeText(tabId, params.text || "");
      break;
    case "key":
    case "keypress":
      await pressKeys(tabId, params.text || "", params.repeat);
      break;
    default:
      throw new Error("unsupported computer action: " + action);
  }
  return { action, tabId, ok: true };
}

// --- find (DOM query, read-only, no debugger) -------------------------------

async function doFind(params, policy) {
  const query = params.query;
  if (!query) throw new Error("find requires a query");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(tabId, policy, "read");
  const r = await chrome.scripting.executeScript({
    target: { tabId },
    args: [query, 20],
    func: (query, limit) => {
      const q = String(query).toLowerCase();
      const out = [];
      const seen = new Set();
      const candidates = document.querySelectorAll(
        "a,button,input,textarea,select,[role],[aria-label],label,h1,h2,h3,summary,[onclick]"
      );
      let n = 0;
      for (const el of candidates) {
        if (out.length >= limit) break;
        const rect = el.getBoundingClientRect();
        if (rect.width === 0 || rect.height === 0) continue;
        const hay = [
          el.innerText || el.textContent || "",
          el.getAttribute("aria-label") || "",
          el.getAttribute("placeholder") || "",
          el.getAttribute("title") || "",
          el.getAttribute("name") || "",
          el.value || "",
          el.getAttribute("role") || "",
        ].join(" ").toLowerCase();
        if (!hay.includes(q)) continue;
        if (seen.has(el)) continue;
        seen.add(el);
        const ref = "ref_" + ++n;
        el.setAttribute("data-gocode-ref", ref);
        const text = (el.innerText || el.textContent || el.value || el.getAttribute("aria-label") || "").trim().slice(0, 120);
        out.push({
          ref,
          text,
          role: el.getAttribute("role") || el.tagName.toLowerCase(),
          x: Math.round(rect.left + rect.width / 2),
          y: Math.round(rect.top + rect.height / 2),
        });
      }
      return out;
    },
  });
  const matches = (r && r[0] && r[0].result) || [];
  return { tabId, matches, truncated: matches.length >= 20 };
}

// --- form_input (value set + events, no debugger) ---------------------------

async function doFormInput(params, policy) {
  const ref = params.ref;
  if (!ref) throw new Error("form_input requires a ref");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(tabId, policy, "act");
  const r = await chrome.scripting.executeScript({
    target: { tabId },
    args: [ref, params.value],
    func: (ref, value) => {
      let el = document.querySelector('[data-gocode-ref="' + ref + '"]');
      if (!el) { try { el = document.querySelector(ref); } catch (e) {} }
      if (!el) return { ok: false, error: "ref not found: " + ref };
      const tag = el.tagName.toLowerCase();
      if (el.type === "checkbox" || el.type === "radio") {
        el.checked = !!value;
      } else if (tag === "select") {
        const want = String(value);
        let matched = false;
        for (const opt of el.options) {
          if (opt.value === want || opt.text === want) {
            el.value = opt.value;
            matched = true;
            break;
          }
        }
        if (!matched) el.value = want;
      } else {
        el.value = value == null ? "" : String(value);
      }
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
      return { ok: true, ref };
    },
  });
  const res = r && r[0] && r[0].result;
  if (!res || !res.ok) throw new Error((res && res.error) || "form_input failed");
  return { ref, ok: true };
}

// --- browser_batch (sequence, one shared debugger session) ------------------

async function callSub(name, input, ctx, policy) {
  switch (name) {
    case "computer": return await doComputer(input, ctx, policy);
    case "navigate": return await doNavigate(input, policy);
    case "read_page":
    case "get_page_text": return await doReadPage(input, policy);
    case "tabs_context":
    case "tabs_context_mcp": return await doTabsContext(input, policy);
    case "tabs_create":
    case "tabs_create_mcp": return await doTabsCreate(input, policy);
    case "tabs_close":
    case "tabs_close_mcp": return await doTabsClose(input);
    case "screenshot": return await doScreenshot(input, ctx, policy);
    case "find": return await doFind(input, policy);
    case "form_input": return await doFormInput(input, policy);
    case "javascript_tool": return await doJavascript(input, ctx, policy);
    case "resize_window": return await doResizeWindow(input);
    case "browser_batch": throw new Error("browser_batch cannot be nested");
    default: throw new Error("unknown batch tool: " + name);
  }
}

async function doBatch(params, policy) {
  const actions = Array.isArray(params.actions) ? params.actions : [];
  if (actions.length === 0) throw new Error("browser_batch requires at least one action");
  const ctx = { attached: new Set() };
  const results = [];
  try {
    for (const act of actions) {
      const name = act && act.name;
      const input = { ...((act && act.input) || {}), sessionId: sessionOf(params) };
      try {
        const r = await callSub(name, input, ctx, policy);
        const entry = { name, ok: true };
        if (r && r.dataUrl) entry.dataUrl = r.dataUrl;
        results.push(entry);
      } catch (e) {
        results.push({ name, error: String((e && e.message) || e) });
        break; // stop on the first error
      }
    }
  } finally {
    // One detach for the whole batch, guaranteed even on error/abort.
    await detachAll(ctx);
  }
  return { results };
}

async function doNavigate(params, policy) {
  if (!params.url) throw new Error("navigate requires params.url");
  // Default target: gocode's own current/group tab — never the user's
  // foreground tab. If gocode has no tab yet, open a new one.
  const sid = sessionOf(params);
  let tabId = params.tabId;
  if (tabId === undefined || tabId === null) {
    tabId = await gocodeTabOrNull(sid);
  } else {
    await assertOwned(tabId); // never navigate a tab outside the gocode group
  }
  const hist = String(params.url).trim().toLowerCase();
  if (hist === "back" || hist === "forward") {
    if (params.tabId === undefined || params.tabId === null) throw new Error('navigate url:"' + hist + '" requires a tabId');
    if (hist === "back") await chrome.tabs.goBack(tabId);
    else await chrome.tabs.goForward(tabId);
    await waitForTabLoad(tabId, 20000);
    await checkTabSite(tabId, policy, "read");
    await setCurrentTab(tabId, sid);
    await applyKeepOpen(params, tabId);
    const t = await getTab(tabId);
    return { tabId, url: (t && t.url) || "", title: t && t.title };
  }
  const url = navTarget(params.url);
  checkNavigation(policy, url);
  let tab;
  if (tabId == null) {
    tab = await chrome.tabs.create({ url, active: params.active === true });
    await addToGocodeGroup(tab.id, sid);
  } else {
    tab = await chrome.tabs.update(tabId, { url });
  }
  // Wait for the navigation to finish so a following read_page/screenshot sees
  // the loaded page, and return the settled url/title.
  await waitForTabLoad(tab.id, 20000);
  await setCurrentTab(tab.id, sid);
  await applyKeepOpen(params, tab.id);
  const settled = await getTab(tab.id);
  return {
    tabId: tab.id,
    url: (settled && settled.url) || url,
    title: settled && settled.title,
  };
}

async function doReadPage(params, policy) {
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(tabId, policy, "read");
  const results = await chrome.scripting.executeScript({
    target: { tabId },
    func: () => document.body ? document.body.innerText : "",
  });
  let text = (results && results[0] && results[0].result) || "";
  let truncated = false;
  if (text.length > MAX_PAGE_TEXT) {
    text = text.slice(0, MAX_PAGE_TEXT);
    truncated = true;
  }
  // Report the page URL so the host can label the untrusted-content fence with
  // where the text came from.
  const tab = await getTab(tabId);
  return { tabId, url: (tab && tab.url) || "", text, truncated };
}

// doTabsContext lists ONLY gocode's tabs and groups — never the user's other
// tabs. With a sessionId it lists only that session's groups and their tabs.
// createIfEmpty opens a tab first when there is none; without it nothing is
// created. Each tab says whether gocode opened it (openedByGocode) and
// whether it is marked keep_open (keepOpen).
async function doTabsContext(params, policy) {
  const sid = sessionOf(params);
  if (params && params.createIfEmpty === true && (await ownedTabList(sid)).length === 0) {
    await doTabsCreate({ sessionId: sid }, policy);
  }
  const own = await ownership();
  const tabs = await ownedTabList(sid);
  const opened = await tabIdList("openedTabs");
  const kept = await tabIdList("keptTabs");
  const groups = [];
  for (const gid of sid ? await sessionGroupIds(sid) : own.groups) {
    try {
      const g = await chrome.tabGroups.get(gid);
      groups.push({ id: g.id, title: g.title || "", color: g.color, windowId: g.windowId });
    } catch (e) {
      // The group was closed.
    }
  }
  return {
    groups,
    tabs: tabs.map((t) => ({
      id: t.id,
      url: t.url || "",
      title: t.title || "",
      active: !!t.active,
      groupId: t.groupId,
      openedByGocode: opened.includes(t.id),
      keepOpen: kept.includes(t.id),
    })),
  };
}

async function doTabsCreate(params, policy) {
  // Open in the BACKGROUND by default (don't steal the user's focus), add it to
  // the shared "gocode" tab group, wait for load, and track it as gocode's
  // current tab so later tool calls target it.
  const url = params.url ? navTarget(params.url) : undefined;
  if (url) checkNavigation(policy, url);
  const tab = await chrome.tabs.create({
    url,
    active: params.active === true,
  });
  const sid = sessionOf(params);
  await addToGocodeGroup(tab.id, sid);
  if (params.url) await waitForTabLoad(tab.id, 20000);
  await setCurrentTab(tab.id, sid);
  await applyKeepOpen(params, tab.id);
  const settled = await getTab(tab.id);
  return {
    tabId: tab.id,
    url: (settled && settled.url) || tab.url || params.url || "",
    title: settled && settled.title,
  };
}

// doTabsClose closes one tab (tabId) or several (tabIds) of gocode's tabs.
// Every tab is checked before anything is closed:
//   - it must be gocode's (any other tab is refused);
//   - groupId pins the close to one gocode group (and, with a sessionId, to
//     that session's group): a tab that has left the group since it was
//     listed is refused rather than closed;
//   - cleanup:true (an end-of-session cleanup) also refuses a tab gocode did
//     not open itself and a tab marked keep_open.
// With tabId a refused tab is an error. With tabIds the allowed tabs are
// closed in one call and the result lists them (closed) and the refused ones
// (refused, with the reason).
async function doTabsClose(params) {
  const many = Array.isArray(params.tabIds);
  const id = params.tabId !== undefined ? params.tabId : params.id;
  if (!many && (id === undefined || id === null)) throw new Error("tabs_close requires params.tabId");
  const ids = many ? params.tabIds : [id];
  if (many && ids.length > 200) throw new Error("tabs_close accepts at most 200 tabIds");
  const sid = sessionOf(params);
  const pinned = params.groupId !== undefined && params.groupId !== null;
  const otherSession = pinned && sid && (await groupSession(params.groupId)) !== sid;
  const cleanup = params.cleanup === true;
  const opened = cleanup ? await tabIdList("openedTabs") : [];
  const kept = cleanup ? await tabIdList("keptTabs") : [];
  const allowed = [];
  const refused = [];
  for (const tabId of ids) {
    try {
      if (typeof tabId !== "number") throw new Error("tab id " + JSON.stringify(tabId) + " is not a number");
      const tab = await assertOwned(tabId); // never close a tab outside the gocode group
      if (pinned && tab.groupId !== params.groupId) {
        throw new Error("tab " + tabId + " is not in tab group " + params.groupId);
      }
      if (otherSession) throw new Error("tab group " + params.groupId + " belongs to another gocode session");
      if (cleanup && !opened.includes(tabId)) throw new Error("tab " + tabId + " was not opened by gocode");
      if (cleanup && kept.includes(tabId)) throw new Error("tab " + tabId + " is marked keep_open");
      if (!allowed.includes(tabId)) allowed.push(tabId);
    } catch (e) {
      if (!many) throw e;
      refused.push({ tabId, error: String((e && e.message) || e) });
    }
  }
  if (!many) {
    await chrome.tabs.remove(id);
    return { closed: id };
  }
  if (!allowed.length) return { closed: [], refused };
  try {
    await chrome.tabs.remove(allowed);
    return { closed: allowed, refused };
  } catch (e) {
    // One tab failed (e.g. it closed meanwhile): close the rest one by one.
    const closed = [];
    for (const tabId of allowed) {
      try {
        await chrome.tabs.remove(tabId);
        closed.push(tabId);
      } catch (e2) {
        refused.push({ tabId, error: String((e2 && e2.message) || e2) });
      }
    }
    return { closed, refused };
  }
}

// captureTab screenshots a specific tab via CDP Page.captureScreenshot, so it
// works on a BACKGROUND/grouped gocode tab and never captures the user's
// foreground tab. The image is downscaled to CSS pixels so coordinates read off
// it match CDP mouse input. If ctx is given the attach is reused (e.g. inside a
// computer call/batch); otherwise a short-lived debugger session is used.
async function captureTab(tabId, ctx) {
  const run = async (c) => {
    await ensureAttached(c, tabId);
    const info = await viewportInfo(tabId);
    const shot = await cdp(tabId, "Page.captureScreenshot", {
      format: "png",
      captureBeyondViewport: false,
    });
    let dataUrl = "data:image/png;base64," + shot.data;
    if (info.dpr && info.dpr !== 1 && info.w && info.h) {
      dataUrl = await dataUrlToCssPixels(dataUrl, info.w, info.h);
    }
    return { dataUrl, width: info.w || undefined, height: info.h || undefined, devicePixelRatio: info.dpr };
  };
  if (ctx) return await run(ctx);
  return await withSession((c) => run(c));
}

async function doScreenshot(params, ctx, policy) {
  params = params || {};
  const tabId = await resolveTabId(params.tabId, sessionOf(params)); // only gocode's tabs
  await checkTabSite(tabId, policy, "read");
  return await captureTab(tabId, ctx);
}

// javascript_tool: evaluate code in a gocode tab with REPL semantics (the last
// expression's value is returned, top-level await works) via CDP
// Runtime.evaluate. It acts on the page, so the site must be allowed.
const MAX_JS_RESULT = 100000;

async function doJavascript(params, ctx, policy) {
  if (params.action !== undefined && params.action !== "javascript_exec") {
    throw new Error("javascript_tool: action must be 'javascript_exec'");
  }
  const code = params.text;
  if (typeof code !== "string" || !code.trim()) throw new Error("javascript_tool requires text (the code to run)");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(tabId, policy, "act");
  await ensureAttached(ctx, tabId);
  const r = await cdp(tabId, "Runtime.evaluate", {
    expression: code,
    awaitPromise: true,
    returnByValue: true,
    replMode: true,
    userGesture: true,
  });
  const tab = await getTab(tabId);
  const out = { tabId, url: (tab && tab.url) || "" };
  if (r && r.exceptionDetails) {
    const ex = r.exceptionDetails.exception;
    out.exception = String((ex && (ex.description || ex.value)) || r.exceptionDetails.text || "error");
    return out;
  }
  const res = (r && r.result) || {};
  let value;
  if (res.type === "undefined") value = "undefined";
  else if (Object.prototype.hasOwnProperty.call(res, "value")) {
    value = typeof res.value === "string" ? res.value : JSON.stringify(res.value);
  } else value = String(res.description || res.type || "");
  if (value && value.length > MAX_JS_RESULT) value = value.slice(0, MAX_JS_RESULT) + "\n[truncated]";
  out.value = value;
  return out;
}

// resize_window: resize the window that holds a gocode tab.
async function doResizeWindow(params) {
  const width = Math.round(Number(params.width));
  const height = Math.round(Number(params.height));
  if (!(width > 0) || !(height > 0)) throw new Error("resize_window requires a positive width and height");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  const tab = await getTab(tabId);
  const win = await chrome.windows.update(tab.windowId, { state: "normal", width, height });
  return { tabId, width: (win && win.width) || width, height: (win && win.height) || height };
}

// Lifecycle: connect eagerly, and keep a heartbeat alarm so the worker
// reconnects after an idle eviction. The live native port itself keeps the
// worker alive while a session is active.
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.alarms.create("gocode-keepalive", { periodInMinutes: 0.4 });
chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === "gocode-keepalive" && !port) connect();
});

// Show the red badge immediately on load, then attempt to connect (which
// clears it on success, or keeps it via onDisconnect if the host is missing).
setConnected(false);
connect();
