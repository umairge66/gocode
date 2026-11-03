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
// { id, result } or { id, error:{ code?, message } } echoing the id, on the
// connection the command arrived on (a reply for a connection that has gone
// is dropped). On connect we announce ourselves with a Hello event
// { method:"hello", name, version }. The host may also send
// { method:"cancel", params:{ id } } (no id of its own, no reply) when the
// caller of command <id> gave up: what is left of that command is aborted —
// the remaining steps of a batch, a pending CDP call or wait — and the
// debugger is detached.
//
// Milestone 3 adds the CDP-backed interaction verbs (computer / find /
// form_input / browser_batch). chrome.debugger is attached to the target tab
// ONLY while input is actually dispatched and ALWAYS detached afterwards (in a
// finally), so Chrome's "started debugging this browser" banner appears only
// during active control and never lingers — even when an action fails or is
// aborted. A browser_batch shares one attach across its steps and detaches once
// at the end. Only one gocode command at a time holds the debugger on a tab
// (others wait their turn), every CDP call and page script has a timeout, and
// a JavaScript dialog (alert/confirm/prompt/beforeunload) that opens while
// gocode holds the debugger is dismissed and reported in the result instead of
// wedging the tab. find and form_input use chrome.scripting (a DOM query / value
// set with input+change events), so they need no debugger and raise no banner.
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

// Chrome passes at most 64 MiB from the extension to the host, and the host
// drops a bigger frame. A reply over this size is replaced by an error reply,
// so the waiting command fails at once instead of timing out.
const MAX_REPLY_BYTES = 48 * 1024 * 1024;

// Total size of the images one browser_batch reply may carry; later images
// are left out (the step says so).
const MAX_BATCH_IMAGE_BYTES = 24 * 1024 * 1024;

// Timeouts (ms). A CDP call or page script that does not answer in time fails
// with an error naming what it was doing, rather than hanging the command.
const CDP_TIMEOUT_MS = 15000;
const SCREENSHOT_TIMEOUT_MS = 20000;
const SCRIPT_TIMEOUT_MS = 15000;
const JS_TIMEOUT_MS = 30000;
const JS_GRACE_MS = 2000; // past JS_TIMEOUT_MS, for Chrome's own evaluation timeout to answer
const DEBUGGER_WAIT_MS = 30000; // waiting for another gocode command on the tab
const LOAD_TIMEOUT_MS = 20000;
const MAX_WAIT_SECONDS = 30;

let port = null;
let reconnectDelay = 1000; // backoff floor (ms)
const MAX_RECONNECT_DELAY = 30000;

// Commands in flight: id -> { port, op }.
const inflight = new Map();

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
  let p;
  try {
    p = chrome.runtime.connectNative(HOST_NAME);
  } catch (e) {
    log("connectNative threw", e);
    setConnected(false);
    scheduleReconnect();
    return;
  }
  port = p;
  p.onMessage.addListener((msg) => onMessage(msg, p));
  p.onDisconnect.addListener(() => onDisconnect(p));
  // Announce ourselves. Any command the host already queued arrives after.
  post(p, { method: "hello", name: EXTENSION_NAME, version: EXTENSION_VERSION });
  reconnectDelay = 1000; // reset backoff on a successful connect
  setConnected(true);
  log("connected to native host");
}

// utf8Length returns the UTF-8 byte length of s.
function utf8Length(s) {
  if (s.length * 3 <= MAX_REPLY_BYTES) return s.length; // cannot be over the limit
  if (typeof TextEncoder !== "undefined") return new TextEncoder().encode(s).length;
  return s.length;
}

// post sends msg on connection p. A message for a connection that is no
// longer the live one is dropped: its host is gone, and the new host never
// sent that id. A reply that is too large, or that postMessage refuses, is
// replaced by an error reply so the waiting command fails at once.
function post(p, msg) {
  if (!p || p !== port) return false;
  if (msg.id != null) {
    let size = 0;
    try {
      size = utf8Length(JSON.stringify(msg));
    } catch (e) {
      msg = { id: msg.id, error: { message: "the reply could not be encoded: " + String((e && e.message) || e) } };
    }
    if (size > MAX_REPLY_BYTES) {
      msg = {
        id: msg.id,
        error: {
          message: "the reply is too large to send (" + Math.round(size / 1048576) + " MiB, limit " +
            Math.round(MAX_REPLY_BYTES / 1048576) + " MiB). Ask for less at once (fewer screenshots, a smaller scale).",
        },
      };
    }
  }
  try {
    p.postMessage(msg);
    return true;
  } catch (e) {
    log("postMessage failed", e);
    if (msg.id != null && !msg.error) {
      try {
        p.postMessage({ id: msg.id, error: { message: "the reply could not be sent: " + String((e && e.message) || e) } });
      } catch (e2) {}
    }
    return false;
  }
}

function onDisconnect(p) {
  const err = chrome.runtime.lastError;
  log("native port disconnected", err && err.message);
  // The host that sent these commands is gone: stop them.
  for (const [id, entry] of inflight) {
    if (entry.port === p) {
      abortOp(entry.op, new Error("the gocode native host disconnected"));
      inflight.delete(id);
    }
  }
  if (port !== p) return;
  port = null;
  setConnected(false);
  scheduleReconnect();
}

function scheduleReconnect() {
  const delay = reconnectDelay;
  reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY);
  setTimeout(connect, delay);
}

// onMessage handles one message from the host on connection p: a cancel
// notice, or a command.
function onMessage(msg, p) {
  if (!msg || typeof msg !== "object") return;
  if (msg.method === "cancel") {
    const id = msg.params && msg.params.id;
    const entry = id != null ? inflight.get(String(id)) : undefined;
    if (entry && entry.port === p) {
      abortOp(entry.op, new Error("cancelled: gocode stopped waiting for this command (it was cancelled or timed out)"));
    }
    return;
  }
  onCommand(msg, p);
}

// onCommand dispatches one host command and always replies, echoing the id,
// on the connection it arrived on.
async function onCommand(msg, p) {
  const { id, method, params } = msg;
  // A message with a method but no id would be an event, not a command; ignore.
  if (id === undefined || id === null) return;
  const key = String(id);
  const op = newOp();
  const entry = { port: p, op };
  inflight.set(key, entry);
  try {
    const result = await dispatch(method, params || {}, op);
    post(p, { id, result });
  } catch (e) {
    post(p, { id, error: { message: String((e && e.message) || e) } });
  } finally {
    if (inflight.get(key) === entry) inflight.delete(key);
  }
}

// --- cancellation and timeouts ------------------------------------------------
//
// An "op" is one command's cancellation state: abortOp marks it aborted and
// rejects everything waiting on it through bounded().

function newOp() {
  return { aborted: null, onAbort: new Set() };
}

function abortOp(op, err) {
  if (!op || op.aborted) return;
  op.aborted = err;
  for (const f of Array.from(op.onAbort)) f(err);
}

function checkOp(op) {
  if (op && op.aborted) throw op.aborted;
}

// bounded resolves like promise, but rejects after ms (naming what timed
// out) or as soon as op is aborted.
function bounded(promise, ms, what, op) {
  return new Promise((resolve, reject) => {
    let done = false;
    let timer = null;
    const onAbort = (err) => finish(reject, err);
    const finish = (f, v) => {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      if (op) op.onAbort.delete(onAbort);
      f(v);
    };
    if (op && op.aborted) return finish(reject, op.aborted);
    if (op) op.onAbort.add(onAbort);
    timer = setTimeout(() => {
      const err = new Error(what + " timed out after " + Math.round(ms / 1000) + "s");
      err.timedOut = true;
      finish(reject, err);
    }, ms);
    Promise.resolve(promise).then((v) => finish(resolve, v), (e) => finish(reject, e));
  });
}

// --- dispatch -------------------------------------------------------------------
//
// Every command runs with a ctx: { op, attached, releases, notes }. attached
// holds the tabs it attached the debugger to (detached when the command ends,
// whatever happens); notes collects what the user should hear about (a
// dismissed dialog), returned with the result.

function newCtx(op) {
  return { op: op || newOp(), attached: new Set(), releases: new Map(), notes: [], onDrag: null };
}

async function dispatch(method, params, op) {
  const ctx = newCtx(op);
  try {
    const result = await run(method, params || {}, ctx);
    if (ctx.notes.length && result && typeof result === "object" && !Array.isArray(result)) {
      result.notes = (result.notes || []).concat(ctx.notes);
    }
    return result;
  } finally {
    // One detach for the whole command, guaranteed even on error/abort.
    await detachAll(ctx);
  }
}

async function run(method, params, ctx) {
  // The site policy the host sent with this command (or the last one it sent);
  // it can only restrict what the command may do.
  const policy = await resolvePolicy(params);
  switch (method) {
    case "ping":
      return { pong: true };
    case "navigate":
      return await doNavigate(params, policy, ctx);
    case "read_page":
    case "get_page_text":
      return await doReadPage(params, policy, ctx);
    case "tabs_context":
      return await doTabsContext(params, policy, ctx);
    case "tabs_create":
      return await doTabsCreate(params, policy, ctx);
    case "tabs_close":
      return await doTabsClose(params);
    case "screenshot":
      return await doScreenshot(params, policy, ctx);
    case "computer":
      return await doComputer(params, policy, ctx);
    case "find":
      return await doFind(params, policy, ctx);
    case "form_input":
      return await doFormInput(params, policy, ctx);
    case "javascript_tool":
      return await doJavascript(params, policy, ctx);
    case "resize_window":
      return await doResizeWindow(params);
    case "browser_batch":
      return await doBatch(params, policy, ctx);
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
//   - a tab is checked at both the page it shows and the page it is loading
//     (pendingUrl), so an action cannot slip onto a page that is committing;
//   - about:blank / about:srcdoc pages and blob: URLs take the site of the
//     page that created them (an about:blank popup opened from a site is that
//     site); other pages that are not http(s) (chrome://, file:, data:) name
//     no site: reading them is allowed, acting on them is refused;
//   - back/forward is checked at the page it lands on, and undone when that
//     page is refused.
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

function isBlankUrl(url) {
  return /^about:(blank|srcdoc)\b/i.test(String(url || ""));
}

// effectiveUrl returns the URL whose site governs a tab's page: a blob: URL's
// origin, the creator's origin for about:blank / about:srcdoc, else url.
async function effectiveUrl(ctx, tab, url) {
  url = String(url || "");
  if (/^blob:/i.test(url)) return url.slice(5);
  if (!isBlankUrl(url)) return url;
  // An about:blank document inherits its creator's origin.
  try {
    const origin = await runInPage(ctx, tab.id, pageAgent, ["origin"], "reading the page origin", 3000);
    if (origin && origin !== "null") return origin + "/";
  } catch (e) {}
  if (tab.openerTabId != null) {
    const opener = await getTab(tab.openerTabId);
    if (opener && opener.url) return opener.url;
  }
  return url;
}

// checkTabSite enforces the policy for a tab's page (and the page it is
// loading, if any). kind is "act" or "read".
async function checkTabSite(ctx, tabId, policy, kind) {
  if (!policy) return;
  const tab = await getTab(tabId);
  if (!tab) throw new Error("tab " + tabId + " does not exist");
  const urls = [tab.url || ""];
  if (tab.pendingUrl && tab.pendingUrl !== tab.url) urls.push(tab.pendingUrl);
  for (const raw of urls) {
    const url = await effectiveUrl(ctx, tab, raw);
    if (!siteOf(url)) {
      // Acting on a page that names no site is refused, except a blank page
      // with no creator (there is nothing on it to act on).
      if (kind === "act" && !isBlankUrl(url)) throw new Error(NON_WEB_URL_ERROR);
      continue;
    }
    checkSite(policy, url, kind);
  }
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
// A command attaches the debugger to a tab through ensureAttached and dispatch
// ALWAYS detaches every tab it attached when the command ends — success,
// thrown error, denied action or cancel — so the debugging banner cannot
// linger. Only one command at a time holds the debugger on a tab: Chrome
// allows one attachment per tab per extension, so a second command (a parallel
// screenshot) waits for the first one's detach instead of failing with
// "Another debugger is already attached".

// tabLocks: tabId -> tail of the queue of commands waiting for that tab.
const tabLocks = new Map();
// sessions: tabId -> the ctx that has the debugger attached to it.
const sessions = new Map();

// lockTab waits for the tab's debugger turn and resolves with its release.
function lockTab(tabId, op) {
  const prev = tabLocks.get(tabId) || Promise.resolve();
  let release;
  const mine = new Promise((r) => (release = r));
  const tail = prev.then(() => mine);
  tabLocks.set(tabId, tail);
  tail.then(() => {
    if (tabLocks.get(tabId) === tail) tabLocks.delete(tabId);
  });
  return bounded(prev, DEBUGGER_WAIT_MS, "waiting for another gocode action on tab " + tabId, op).then(
    () => release,
    (e) => {
      release();
      throw e;
    }
  );
}

async function detachAll(ctx) {
  for (const tabId of Array.from(ctx.attached)) {
    if (sessions.get(tabId) === ctx) sessions.delete(tabId);
    try {
      await chrome.debugger.detach({ tabId });
    } catch (e) {
      // Already gone (tab closed, user cancelled) — nothing to clean up.
    }
    ctx.attached.delete(tabId);
    const release = ctx.releases.get(tabId);
    ctx.releases.delete(tabId);
    if (release) release();
  }
}

async function ensureAttached(ctx, tabId) {
  if (ctx.attached.has(tabId)) return;
  const release = await lockTab(tabId, ctx.op);
  try {
    await bounded(chrome.debugger.attach({ tabId }, "1.3"), CDP_TIMEOUT_MS, "attaching the debugger to tab " + tabId, ctx.op);
  } catch (e) {
    release();
    const msg = String((e && e.message) || e);
    if (/already attached/i.test(msg)) {
      throw new Error(
        "Chrome DevTools (or another debugger) is attached to tab " + tabId +
          ". Close DevTools on that tab and try again."
      );
    }
    throw e;
  }
  ctx.attached.add(tabId);
  ctx.releases.set(tabId, release);
  sessions.set(tabId, ctx);
  // Page events report JavaScript dialogs (see handleDialog).
  try {
    await cdp(ctx, tabId, "Page.enable", {}, 5000);
  } catch (e) {}
}

// cdp sends one CDP command and resolves with its result (or rejects). It
// fails after timeoutMs, or as soon as the command is cancelled.
function cdp(ctx, tabId, method, params, timeoutMs) {
  checkOp(ctx.op);
  const call = new Promise((resolve, reject) => {
    chrome.debugger.sendCommand({ tabId }, method, params || {}, (res) => {
      const err = chrome.runtime.lastError;
      if (err) reject(new Error(err.message));
      else resolve(res);
    });
  });
  return bounded(call, timeoutMs || CDP_TIMEOUT_MS, "Chrome DevTools command " + method, ctx.op);
}

// handleDialog dismisses a JavaScript dialog that opened on a tab gocode holds
// the debugger for (a click that runs alert()/confirm() would otherwise block
// every later input on the tab) and records it for the result. Dismissing is
// the safe answer: confirm() gets Cancel, prompt() no answer, beforeunload
// stays on the page.
function handleDialog(ctx, tabId, p) {
  const type = (p && p.type) || "alert";
  const message = String((p && p.message) || "").replace(/\s+/g, " ").trim().slice(0, 300);
  const answer = type === "confirm" ? " (answered Cancel)" : type === "prompt" ? " (gave no answer)" :
    type === "beforeunload" ? " (stayed on the page)" : "";
  ctx.notes.push(
    "A JavaScript " + type + " dialog opened on tab " + tabId + (message ? ' saying "' + message + '"' : "") +
      " and gocode dismissed it" + answer + "."
  );
  try {
    chrome.debugger.sendCommand({ tabId }, "Page.handleJavaScriptDialog", { accept: false }, () => {
      void chrome.runtime.lastError;
    });
  } catch (e) {}
}

if (chrome.debugger.onEvent) {
  chrome.debugger.onEvent.addListener((source, method, params) => {
    const ctx = source && sessions.get(source.tabId);
    if (!ctx) return;
    if (method === "Page.javascriptDialogOpening") handleDialog(ctx, source.tabId, params);
    else if (method === "Input.dragIntercepted" && ctx.onDrag) ctx.onDrag((params && params.data) || null);
  });
}

// If the user detaches via the banner's "Cancel", stop the command that held
// the debugger and drop its bookkeeping, so we do not drive a dead session.
chrome.debugger.onDetach.addListener((source, reason) => {
  const tabId = source && source.tabId;
  log("debugger detached", tabId, reason);
  const ctx = sessions.get(tabId);
  if (!ctx) return;
  sessions.delete(tabId);
  ctx.attached.delete(tabId);
  const release = ctx.releases.get(tabId);
  ctx.releases.delete(tabId);
  if (release) release();
  if (reason === "canceled_by_user") {
    abortOp(ctx.op, new Error("the user stopped gocode's control of tab " + tabId + " (Cancel on the debugging banner)"));
  }
});

// --- page scripts (chrome.scripting) ----------------------------------------

// runInPage runs func(...args) in the tab's top frame and returns its result.
// It fails after timeoutMs or when the command is cancelled. A page that does
// not answer is most often blocked by a JavaScript dialog: one is dismissed
// (through the debugger) and the error says so.
async function runInPage(ctx, tabId, func, args, what, timeoutMs) {
  checkOp(ctx.op);
  const call = chrome.scripting.executeScript({ target: { tabId }, func, args: args || [] });
  try {
    const r = await bounded(call, timeoutMs || SCRIPT_TIMEOUT_MS, what, ctx.op);
    return r && r[0] ? r[0].result : undefined;
  } catch (e) {
    if (e && e.timedOut && (await dismissStuckDialog(ctx, tabId))) {
      throw new Error(what + ": the page was blocked by a JavaScript dialog, which gocode dismissed. Try again.");
    }
    throw e;
  }
}

// dismissStuckDialog dismisses a JavaScript dialog open on the tab, and
// reports whether there was one.
async function dismissStuckDialog(ctx, tabId) {
  if (ctx.op.aborted) return false;
  try {
    await ensureAttached(ctx, tabId);
    await cdp(ctx, tabId, "Page.handleJavaScriptDialog", { accept: false }, 3000);
    ctx.notes.push("A JavaScript dialog was open on tab " + tabId + " and gocode dismissed it.");
    return true;
  } catch (e) {
    return false;
  }
}

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
// (tabs_create, navigate without a tab, a link followed by a gocode action) and
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
//
// Every read-modify-write of this state runs through updateState, one at a
// time, so two commands (or a command and a tab event) cannot overwrite each
// other's update and orphan a tab or group.

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

let stateTail = Promise.resolve();

// updateState reads keys, lets fn compute the changes (an object of new
// values, or nothing) and writes them, serialized with every other update.
function updateState(keys, fn) {
  const run = stateTail.then(async () => {
    const cur = await sessionGet(keys);
    const changes = await fn(cur);
    if (changes && Object.keys(changes).length) await sessionSet(changes);
    return changes;
  });
  stateTail = run.catch(() => {});
  return run;
}

const listOf = (v) => (Array.isArray(v) ? v : []);
const mapOf = (v) => (v && typeof v === "object" && !Array.isArray(v) ? v : {});

// addToList / removeFromList update one id list in the session state.
function addToList(key, id) {
  return updateState([key], (o) => {
    const l = listOf(o[key]);
    return l.includes(id) ? null : { [key]: l.concat([id]) };
  });
}

function removeFromList(key, id) {
  return updateState([key], (o) => {
    const l = listOf(o[key]);
    return l.includes(id) ? { [key]: l.filter((x) => x !== id) } : null;
  });
}

async function ownership() {
  const { ownedGroups, ownedTabs } = await sessionGet(["ownedGroups", "ownedTabs"]);
  return { groups: ownedGroups || [], tabs: ownedTabs || [] };
}

async function tabIdList(key) {
  const o = await sessionGet(key);
  return Array.isArray(o[key]) ? o[key] : [];
}

// setKeepOpen marks tabId to stay open after the session ends (keep true) or
// clears that mark.
function setKeepOpen(tabId, keep) {
  return keep ? addToList("keptTabs", tabId) : removeFromList("keptTabs", tabId);
}

// applyKeepOpen applies a command's keep_open parameter (when it carries one)
// to tabId.
async function applyKeepOpen(params, tabId) {
  if (tabId == null || !params || typeof params.keep_open !== "boolean") return;
  await setKeepOpen(tabId, params.keep_open);
}

// sessionOf returns the session id a command carries ("" when none).
function sessionOf(params) {
  const s = params && params.sessionId;
  return typeof s === "string" && s.length <= 256 ? s : "";
}

async function groupSessionMap() {
  const { groupSessions } = await sessionGet("groupSessions");
  return mapOf(groupSessions);
}

// groupSession returns the session that owns gocode group groupId ("" when the
// group was opened by commands without a session).
async function groupSession(groupId) {
  const m = await groupSessionMap();
  return m[String(groupId)] || "";
}

function rememberGroupSession(groupId, sid) {
  if (!sid) return Promise.resolve();
  return updateState(["groupSessions"], (o) => ({ groupSessions: { ...mapOf(o.groupSessions), [String(groupId)]: sid } }));
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

function setCurrentTab(tabId, sid) {
  if (tabId == null) return Promise.resolve();
  if (!sid) return sessionSet({ currentTabId: tabId });
  return updateState(["currentTabs"], (o) => ({ currentTabs: { ...mapOf(o.currentTabs), [sid]: tabId } }));
}

async function currentTabFor(sid) {
  if (!sid) {
    const { currentTabId } = await sessionGet("currentTabId");
    return currentTabId;
  }
  const { currentTabs } = await sessionGet("currentTabs");
  return mapOf(currentTabs)[sid];
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

// groupTail serializes addToGocodeGroup, so two tabs opened at once for the
// same session end up in one group rather than two.
let groupTail = Promise.resolve();

// addToGocodeGroup records the tab as gocode's (and as opened by gocode — it
// is only called for tabs gocode opened) and puts it in a gocode group:
// preferGroupId when that is one of gocode's open groups, else session sid's
// group in the tab's window, else a new group owned by sid.
function addToGocodeGroup(tabId, sid, preferGroupId) {
  const run = groupTail.then(() => groupTab(tabId, sid || "", preferGroupId));
  groupTail = run.catch(() => {});
  return run;
}

async function groupTab(tabId, sid, preferGroupId) {
  await addToList("ownedTabs", tabId);
  await addToList("openedTabs", tabId);
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
      await addToList("ownedGroups", groupId);
      await rememberGroupSession(groupId, sid);
    }
    // Grouped: from now on the group decides. If the user drags the tab out
    // of the gocode group, it is no longer gocode's.
    await removeFromList("ownedTabs", tabId);
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

// --- tabs opened by gocode's actions ----------------------------------------
//
// A tab opened FROM a gocode tab while gocode is acting on it (a click on a
// target=_blank link, window.open from a script gocode ran, ctrl+click by
// gocode) joins the gocode group, so following a link keeps working inside
// the group. A tab the user opens from a gocode tab themselves (a middle- or
// ctrl-click of their own) is left alone: it is not adopted, so it is never
// closed by gocode's cleanup. "While acting" includes a short grace period
// after the action, since a popup can open a moment after the click.

const OPEN_GRACE_MS = 3000;
// actingTabs: tabId -> { count, until }.
const actingTabs = new Map();

async function acting(tabId, fn) {
  const cur = actingTabs.get(tabId) || { count: 0, until: 0 };
  cur.count++;
  actingTabs.set(tabId, cur);
  try {
    return await fn();
  } finally {
    cur.count--;
    cur.until = Date.now() + OPEN_GRACE_MS;
  }
}

function openedByGocodeAction(openerId) {
  const a = actingTabs.get(openerId);
  return !!a && (a.count > 0 || Date.now() <= a.until);
}

chrome.tabs.onCreated.addListener(async (tab) => {
  if (tab.openerTabId == null || !openedByGocodeAction(tab.openerTabId)) return;
  const opener = await getTab(tab.openerTabId);
  const own = await ownership();
  if (!isOwnedTab(opener, own)) return;
  const grouped = own.groups.includes(opener.groupId);
  const sid = grouped ? await groupSession(opener.groupId) : "";
  await addToGocodeGroup(tab.id, sid, grouped ? opener.groupId : undefined);
});

chrome.tabs.onRemoved.addListener(async (tabId) => {
  actingTabs.delete(tabId);
  await removeFromList("ownedTabs", tabId);
  await removeFromList("openedTabs", tabId);
  await removeFromList("keptTabs", tabId);
  await updateState(["refCounters"], (o) => {
    const m = mapOf(o.refCounters);
    if (!(String(tabId) in m)) return null;
    const next = { ...m };
    delete next[String(tabId)];
    return { refCounters: next };
  });
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
// instead of a still-loading one. A cancel ends the wait with an error.
function waitForTabLoad(ctx, tabId, timeoutMs) {
  const wait = new Promise((resolve) => {
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
  return bounded(wait, (timeoutMs || 15000) + 1000, "waiting for the page to load", ctx.op);
}

// viewportInfo returns the page's CSS viewport size, device-pixel-ratio and
// scroll offset. CDP input coordinates are in CSS pixels, so this lets us keep
// the screenshot coordinate space (which captureVisibleTab renders at dpr)
// aligned with it. Where page scripts are blocked it asks CDP instead
// (Page.getLayoutMetrics), so a HiDPI page still gets its real ratio.
async function viewportInfo(ctx, tabId) {
  try {
    const r = await runInPage(ctx, tabId, pageAgent, ["info"], "reading the viewport size", 5000);
    if (r && r.w) return r;
  } catch (e) {}
  try {
    await ensureAttached(ctx, tabId);
    const m = await cdp(ctx, tabId, "Page.getLayoutMetrics", {}, 5000);
    const css = m && (m.cssLayoutViewport || m.cssVisualViewport);
    const dev = m && m.layoutViewport; // device pixels
    const vis = m && m.cssVisualViewport;
    if (css && css.clientWidth) {
      const dpr = dev && dev.clientWidth ? dev.clientWidth / css.clientWidth : 1;
      return {
        dpr, w: css.clientWidth, h: css.clientHeight,
        sx: (vis && vis.pageX) || 0, sy: (vis && vis.pageY) || 0,
      };
    }
  } catch (e) {}
  return { dpr: 1, w: 0, h: 0, sx: 0, sy: 0 };
}

// showCursor injects (once) and moves a PERSISTENT, visible arrow cursor at CSS
// coords [x,y] so the user can see where gocode's synthetic mouse is. It is
// pointer-events:none (never intercepts the real click) at the top z-index, and
// does NOT mark/ripple the click location. Uses chrome.scripting so it works
// with or without the debugger attached and is purely visual. It is re-created
// automatically after a navigation (which wipes the page DOM).
async function showCursor(ctx, tabId, x, y) {
  try {
    await runInPage(ctx, tabId, (x, y) => {
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
    }, [x, y], "showing the cursor", 2000);
  } catch (e) {
    // Cursor overlay is cosmetic; never fail an action because of it.
  }
}

// resizeImage scales a PNG data URL to w x h CSS pixels, so screenshot pixel
// coords equal CSS/CDP coords (or a requested fraction of them). Runs in the
// service worker via OffscreenCanvas (no FileReader, which SWs lack).
async function resizeImage(dataUrl, w, h) {
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
//   - an upper-case letter in a chord with Ctrl/Alt/Meta ("ctrl+C") is the
//     letter's key, not Shift+letter: write "ctrl+shift+c" for that;
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
  // "ctrl+C" means the C key with Ctrl, as people write shortcuts — not
  // ctrl+shift+c. Only Shift (held or written) makes it upper case.
  if (/^[A-Z]$/.test(keyName) && (mask & (MOD_CTRL | MOD_ALT | MOD_META))) keyName = keyName.toLowerCase();
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

async function sendKeyEvent(ctx, tabId, params) {
  await cdp(ctx, tabId, "Input.dispatchKeyEvent", params);
}

// pressChord sends the CDP events for one planned chord.
async function pressChord(ctx, tabId, plan) {
  let mask = 0;
  const held = [];
  const down = async (m) => {
    mask |= m.bit;
    held.push(m);
    await sendKeyEvent(ctx, tabId, keyParams("rawKeyDown", m, mask, null));
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
    await sendKeyEvent(ctx, tabId, p);
    await sendKeyEvent(ctx, tabId, keyParams("keyUp", base, mask, null));
  }
  for (let i = held.length - 1; i >= 0; i--) {
    mask &= ~held[i].bit;
    await sendKeyEvent(ctx, tabId, keyParams("keyUp", held[i], mask, null));
  }
}

// pressKeys presses a space-separated key sequence `repeat` times. Every token
// is parsed before any event is sent, so a bad token presses nothing. An
// unknown multi-character name with no modifiers is inserted as text (be
// forgiving; prefer the type action for text).
async function pressKeys(ctx, tabId, text, repeat) {
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
      if (plan.literal !== undefined) await typeText(ctx, tabId, plan.literal);
      else await pressChord(ctx, tabId, plan);
    }
  }
}

async function mouseEvent(ctx, tabId, type, x, y, extra) {
  await cdp(ctx, tabId, "Input.dispatchMouseEvent", { type, x, y, ...(extra || {}) });
}

async function mouseClick(ctx, tabId, coord, button, clickCount, modifiers) {
  if (!coord) throw new Error("click requires a coordinate or ref");
  const [x, y] = coord;
  const mods = modifierMask(modifiers);
  // CDP `buttons` is the bitmask of buttons CURRENTLY down: left=1, right=2,
  // middle=4. It is set on press and cleared (0) on release.
  const btMask = button === "right" ? 2 : button === "middle" ? 4 : 1;
  await showCursor(ctx, tabId, x, y);
  await mouseEvent(ctx, tabId, "mouseMoved", x, y, { modifiers: mods, buttons: 0 });
  await mouseEvent(ctx, tabId, "mousePressed", x, y, { button, buttons: btMask, clickCount, modifiers: mods });
  await mouseEvent(ctx, tabId, "mouseReleased", x, y, { button, buttons: 0, clickCount, modifiers: mods });
}

async function mouseMove(ctx, tabId, coord) {
  if (!coord) throw new Error("mouse move requires a coordinate or ref");
  await showCursor(ctx, tabId, coord[0], coord[1]);
  await mouseEvent(ctx, tabId, "mouseMoved", coord[0], coord[1], { buttons: 0 });
}

// mouseDrag presses at start, moves to end in steps (pages that track
// mousemove — sliders, sortable lists, canvases — see a real path) and
// releases there. An HTML5 drag (draggable=true) is intercepted through CDP
// (Input.setInterceptDrags) and completed with dragEnter/dragOver/drop at the
// end point, so the drop target gets its drop event.
async function mouseDrag(ctx, tabId, start, end) {
  if (!start || !end) throw new Error("left_click_drag requires start_coordinate and coordinate");
  let dragData = null;
  ctx.onDrag = (data) => {
    dragData = data || { items: [], dragOperationsMask: 1 };
  };
  let intercepting = false;
  try {
    try {
      await cdp(ctx, tabId, "Input.setInterceptDrags", { enabled: true }, 5000);
      intercepting = true;
    } catch (e) {
      // An older Chrome: a plain mouse drag only.
    }
    await showCursor(ctx, tabId, start[0], start[1]);
    await mouseEvent(ctx, tabId, "mouseMoved", start[0], start[1], { buttons: 0 });
    await mouseEvent(ctx, tabId, "mousePressed", start[0], start[1], { button: "left", buttons: 1, clickCount: 1 });
    const dist = Math.hypot(end[0] - start[0], end[1] - start[1]);
    const steps = Math.max(5, Math.min(20, Math.round(dist / 20)));
    for (let i = 1; i <= steps && !dragData; i++) {
      const x = Math.round(start[0] + ((end[0] - start[0]) * i) / steps);
      const y = Math.round(start[1] + ((end[1] - start[1]) * i) / steps);
      await mouseEvent(ctx, tabId, "mouseMoved", x, y, { button: "left", buttons: 1 });
    }
    if (intercepting && !dragData) await sleep(50); // the drag event may trail the last move
    await showCursor(ctx, tabId, end[0], end[1]);
    if (dragData) {
      for (const type of ["dragEnter", "dragOver", "drop"]) {
        await cdp(ctx, tabId, "Input.dispatchDragEvent", { type, x: end[0], y: end[1], data: dragData, modifiers: 0 });
      }
    }
    await mouseEvent(ctx, tabId, "mouseReleased", end[0], end[1], { button: "left", buttons: 0, clickCount: 1 });
    return dragData ? "drag-and-drop" : "mouse drag";
  } finally {
    ctx.onDrag = null;
    if (intercepting) {
      try { await cdp(ctx, tabId, "Input.setInterceptDrags", { enabled: false }, 5000); } catch (e) {}
    }
  }
}

async function mouseScroll(ctx, tabId, coord, direction, amount) {
  let x, y;
  if (coord) [x, y] = coord;
  else {
    const info = await viewportInfo(ctx, tabId);
    x = Math.floor((info.w || 0) / 2);
    y = Math.floor((info.h || 0) / 2);
  }
  const ticks = (Math.round(Number(amount)) || 3) * 100;
  let deltaX = 0, deltaY = 0;
  switch (direction) {
    case "up": deltaY = -ticks; break;
    case "down": deltaY = ticks; break;
    case "left": deltaX = -ticks; break;
    case "right": deltaX = ticks; break;
    default: deltaY = ticks;
  }
  await mouseEvent(ctx, tabId, "mouseWheel", x, y, { deltaX, deltaY });
}

async function typeText(ctx, tabId, text) {
  await cdp(ctx, tabId, "Input.insertText", { text: String(text) });
}

// --- the page agent (runs inside the page) -----------------------------------
//
// pageAgent is injected with chrome.scripting into the tab's top frame (in the
// extension's isolated world, which the page cannot see) and does every DOM
// job: find, resolving a ref to a point (or a scroll), form filling, reading
// the viewport. It must be self-contained (chrome.scripting serializes it).
//
// Refs: find tags each element it returns with a ref "ref_<n>". The refs live
// in a Map in the isolated world (not in DOM attributes), so a lookup is exact:
// one ref is one element. n is unique per tab across documents — the extension
// hands each find a fresh block of numbers (refBlock) — so a ref from a page
// that has since navigated or reloaded is reported as stale instead of
// silently hitting whatever element got the same number on the new page. The
// same element keeps its ref across finds on one page.
//
// find matches by what models ask for: the element's accessible name (its
// label — label[for], a wrapping label, aria-labelledby, aria-label —
// placeholder, title, alt, text), its role or type in words ("input",
// "text field", "button", "link", "checkbox", "dropdown", "form", …) and its
// name/id attributes. A label is folded into its control: "Full name" returns
// the input, not the <label>. Results are ranked (exact name matches first),
// capped, and only visible elements are returned unless includeHidden is set.
// Open shadow roots and same-origin iframes are searched too.

function pageAgent(op, arg) {
  arg = arg || {};
  const S = (globalThis.__gocodeAgent = globalThis.__gocodeAgent || { next: 0, refs: new Map(), byEl: new WeakMap() });

  const tagOf = (el) => String((el && el.tagName) || "").toLowerCase();
  const words = (s) =>
    String(s == null ? "" : s)
      .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
      .toLowerCase()
      .replace(/[^\p{L}\p{N}]+/gu, " ")
      .trim();
  const clip = (s, n) => {
    s = String(s == null ? "" : s).replace(/\s+/g, " ").trim();
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  };
  const rootOf = (el) => (el.getRootNode ? el.getRootNode() : document);
  const attr = (el, n) => (el.getAttribute && el.getAttribute(n)) || "";
  const LABELABLE = { input: 1, select: 1, textarea: 1, button: 1, meter: 1, progress: 1, output: 1 };

  // walk visits every element in DOM order, including open shadow roots and
  // same-origin iframes.
  function walk(visit) {
    const stack = [document.documentElement];
    while (stack.length) {
      const el = stack.pop();
      if (!el || el.nodeType !== 1) continue;
      const tag = tagOf(el);
      if (tag === "script" || tag === "style" || tag === "noscript" || tag === "template" || tag === "head") continue;
      visit(el);
      const kids = [];
      if (el.shadowRoot) kids.push(...Array.from(el.shadowRoot.children || []));
      kids.push(...Array.from(el.children || []));
      if (tag === "iframe" || tag === "frame") {
        try {
          const d = el.contentDocument;
          if (d && d.documentElement) kids.push(d.documentElement);
        } catch (e) {}
      }
      for (let i = kids.length - 1; i >= 0; i--) stack.push(kids[i]);
    }
  }

  // textOf returns an element's text (up to ~400 chars), skipping the
  // contents of controls and the alt text of images inside it.
  function textOf(el) {
    let out = "";
    const visit = (n) => {
      for (const c of Array.from(n.childNodes || [])) {
        if (out.length > 400) return;
        if (c.nodeType === 3) out += c.data != null ? c.data : c.textContent || "";
        else if (c.nodeType === 1) {
          const t = tagOf(c);
          if (t === "script" || t === "style" || t === "select" || t === "textarea" || t === "input") continue;
          if (t === "img") { out += " " + attr(c, "alt") + " "; continue; }
          out += " ";
          visit(c);
          out += " ";
        }
      }
    };
    visit(el);
    return out;
  }

  function labelControl(label) {
    const f = attr(label, "for");
    if (f) {
      const root = rootOf(label);
      const c = root.getElementById ? root.getElementById(f) : null;
      if (c) return c;
    }
    let found = null;
    const visit = (n) => {
      for (const c of Array.from(n.children || [])) {
        if (found) return;
        if (LABELABLE[tagOf(c)] && attr(c, "type").toLowerCase() !== "hidden") { found = c; return; }
        visit(c);
      }
    };
    visit(label);
    return found;
  }

  // buildIndex walks the page once: ids and labels per root, and the
  // elements find considers.
  function buildIndex(wantCandidates) {
    const ix = { ids: new Map(), labelsOf: new Map(), all: [] };
    walk((el) => {
      const root = rootOf(el);
      const id = attr(el, "id");
      if (id) {
        if (!ix.ids.has(root)) ix.ids.set(root, new Map());
        if (!ix.ids.get(root).has(id)) ix.ids.get(root).set(id, el);
      }
      if (tagOf(el) === "label") {
        const c = labelControl(el);
        if (c) {
          if (!ix.labelsOf.has(c)) ix.labelsOf.set(c, []);
          ix.labelsOf.get(c).push(el);
        }
      }
      if (wantCandidates) ix.all.push(el);
    });
    return ix;
  }

  function nameOf(el, ix) {
    const by = attr(el, "aria-labelledby");
    if (by) {
      const m = ix.ids.get(rootOf(el));
      const t = by.split(/\s+/).map((i) => m && m.get(i)).filter(Boolean).map(textOf).join(" ");
      if (t.trim()) return clip(t, 200);
    }
    const al = attr(el, "aria-label");
    if (al.trim()) return clip(al, 200);
    const tag = tagOf(el);
    const labels = () => clip((ix.labelsOf.get(el) || []).map(textOf).join(" "), 200);
    if (tag === "input" || tag === "select" || tag === "textarea") {
      const lt = labels();
      if (lt) return lt;
      const type = attr(el, "type").toLowerCase();
      if (tag === "input" && (type === "button" || type === "submit" || type === "reset")) {
        return clip(el.value || (type === "submit" ? "Submit" : type === "reset" ? "Reset" : ""), 200);
      }
      if (tag === "input" && type === "image") return clip(attr(el, "alt"), 200);
      return clip(attr(el, "placeholder") || attr(el, "title"), 200);
    }
    if (tag === "img") return clip(attr(el, "alt") || attr(el, "title"), 200);
    const t = clip(textOf(el), 200);
    if (t) return t;
    return labels() || clip(attr(el, "title") || attr(el, "placeholder"), 200);
  }

  function roleOf(el) {
    const explicit = attr(el, "role").trim().split(/\s+/)[0].toLowerCase();
    if (explicit) return explicit;
    const tag = tagOf(el);
    switch (tag) {
      case "a": return el.hasAttribute("href") ? "link" : "generic";
      case "button": case "summary": return "button";
      case "input": {
        const t = (attr(el, "type") || "text").toLowerCase();
        if (t === "button" || t === "submit" || t === "reset" || t === "image") return "button";
        if (t === "checkbox") return "checkbox";
        if (t === "radio") return "radio";
        if (t === "range") return "slider";
        if (t === "search") return "searchbox";
        if (t === "number") return "spinbutton";
        if (t === "hidden") return "hidden";
        if (t === "file") return "file";
        if (el.hasAttribute("list")) return "combobox";
        return "textbox";
      }
      case "textarea": return "textbox";
      case "select": return el.multiple || Number(attr(el, "size")) > 1 ? "listbox" : "combobox";
      case "form": return "form";
      case "label": return "label";
      case "img": return "img";
      case "dialog": return "dialog";
      case "h1": case "h2": case "h3": case "h4": case "h5": case "h6": return "heading";
    }
    if (el.isContentEditable) return "textbox";
    return "generic";
  }

  function frameOffset(el) {
    let x = 0, y = 0;
    let doc = el.ownerDocument;
    for (let i = 0; doc && doc !== document && i < 10; i++) {
      const fe = doc.defaultView && doc.defaultView.frameElement;
      if (!fe) break;
      const r = fe.getBoundingClientRect();
      x += r.left + (fe.clientLeft || 0);
      y += r.top + (fe.clientTop || 0);
      doc = fe.ownerDocument;
    }
    return { x, y };
  }

  function rectOf(el) {
    const r = el.getBoundingClientRect();
    const o = frameOffset(el);
    return { left: r.left + o.x, top: r.top + o.y, width: r.width, height: r.height };
  }

  function isVisible(el, r) {
    if (!(r.width > 0 && r.height > 0)) return false;
    try {
      const view = (el.ownerDocument && el.ownerDocument.defaultView) || window;
      const cs = view.getComputedStyle(el);
      if (cs && (cs.visibility === "hidden" || cs.visibility === "collapse" || cs.display === "none")) return false;
    } catch (e) {}
    return true;
  }

  const STALE = (ref) =>
    ref + " is not on this page: refs from an earlier page (before a navigation or reload) do not carry over. Run find again on the current page.";

  function lookup(ref) {
    ref = String(ref == null ? "" : ref).trim();
    if (/^ref_\d+$/.test(ref)) {
      const el = S.refs.get(ref);
      if (!el) return { error: STALE(ref) };
      if (!el.isConnected) {
        S.refs.delete(ref);
        return { error: ref + " is no longer on the page (it was removed or re-rendered). Run find again to get a fresh ref." };
      }
      return { el };
    }
    let el = null;
    try { el = document.querySelector(ref); } catch (e) {}
    return el ? { el } : { error: "ref not found: " + ref + " — use a ref returned by find (e.g. ref_3)" };
  }

  function refFor(el) {
    let ref = S.byEl.get(el);
    if (ref && S.refs.get(ref) === el) return ref;
    ref = "ref_" + ++S.next;
    S.refs.set(ref, el);
    S.byEl.set(el, ref);
    return ref;
  }

  function describe(el) {
    if (!el) return "nothing";
    let s = "<" + tagOf(el);
    const id = attr(el, "id");
    if (id) s += "#" + id;
    const cls = attr(el, "class").trim().split(/\s+/).filter(Boolean).slice(0, 2);
    if (cls.length) s += "." + cls.join(".");
    s += ">";
    const t = clip(textOf(el), 60);
    return t ? s + ' "' + t + '"' : s;
  }

  // ---- find ----

  // Phrases that name a kind of element, rewritten to a role token "@kind".
  const KIND_PHRASES = [
    [/\b(text ?areas?|multi ?line( text)?( fields?| boxe?s?| inputs?)?)\b/g, " @textarea "],
    [/\bsearch ?(boxe?s?|fields?|bars?|inputs?)\b/g, " search @text "],
    [/\b(text|edit|entry) ?(fields?|boxe?s?|inputs?)\b/g, " @text "],
    [/\binput ?(fields?|boxe?s?|elements?)\b/g, " @input "],
    [/\btext ?boxe?s?\b/g, " @text "],
    [/\b(check ?boxe?s?|tick ?boxe?s?)\b/g, " @checkbox "],
    [/\bradio( buttons?| boxe?s?)?s?\b/g, " @radio "],
    [/\b(drop ?downs?|combo ?boxe?s?|select ?boxe?s?|selects?|pick ?lists?|pickers?|list ?boxe?s?)\b/g, " @select "],
    [/\b(buttons?|btns?)\b/g, " @button "],
    [/\b(links?|hyperlinks?|anchors?)\b/g, " @link "],
    [/\bforms?\b/g, " @form "],
    [/\b(headings?|headers?)\b/g, " @heading "],
    [/\b(images?|imgs?|pictures?)\b/g, " @img "],
    [/\b(toggles?|switch(es)?)\b/g, " @switch "],
    [/\bsliders?\b/g, " @slider "],
    [/\btabs?\b/g, " @tab "],
    [/\b(dialogs?|modals?|popups?)\b/g, " @dialog "],
    [/\blabels?\b/g, " @label "],
    [/\bmenu ?items?\b/g, " @menuitem "],
    [/\b(inputs?|fields?|controls?)\b/g, " @input "],
  ];
  const KINDS = {
    text: (r, el) => r === "textbox" || r === "searchbox" || (r === "combobox" && tagOf(el) === "input"),
    textarea: (r, el) => tagOf(el) === "textarea" || (r === "textbox" && (el.isContentEditable || attr(el, "aria-multiline") === "true")),
    input: (r) => ["textbox", "searchbox", "combobox", "spinbutton", "listbox", "checkbox", "radio", "slider", "switch", "file"].includes(r),
    checkbox: (r) => r === "checkbox" || r === "switch",
    radio: (r) => r === "radio",
    select: (r) => r === "combobox" || r === "listbox",
    button: (r) => r === "button",
    link: (r) => r === "link",
    form: (r) => r === "form",
    heading: (r) => r === "heading",
    img: (r) => r === "img",
    switch: (r) => r === "switch" || r === "checkbox",
    slider: (r) => r === "slider",
    tab: (r) => r === "tab",
    dialog: (r) => r === "dialog" || r === "alertdialog",
    label: (r) => r === "label",
    menuitem: (r) => r.startsWith("menuitem"),
  };
  const STOP = new Set(["the", "a", "an", "for", "of", "to", "in", "on", "at", "with", "my", "your", "our", "this", "that",
    "its", "which", "where", "element", "elements", "item", "items", "please", "box", "area", "thing", "called", "named", "labeled", "labelled"]);
  const INTERACTIVE = new Set(["button", "link", "textbox", "searchbox", "combobox", "spinbutton", "listbox", "checkbox",
    "radio", "slider", "switch", "file", "tab", "menuitem", "menuitemcheckbox", "menuitemradio", "option"]);

  function isCandidate(el) {
    const tag = tagOf(el);
    if (tag === "input" && attr(el, "type").toLowerCase() === "hidden") return false;
    if ({ a: 1, button: 1, input: 1, textarea: 1, select: 1, label: 1, summary: 1, form: 1, img: 1, dialog: 1,
      h1: 1, h2: 1, h3: 1, h4: 1, h5: 1, h6: 1 }[tag]) return true;
    return el.hasAttribute("role") || el.hasAttribute("onclick") || el.hasAttribute("tabindex") ||
      el.hasAttribute("aria-label") || !!el.isContentEditable;
  }

  const hasWords = (hay, phrase) => (" " + hay + " ").includes(" " + phrase + " ");
  const hasPrefix = (hay, phrase) => (" " + hay).includes(" " + phrase);

  function score(name, sec, tokens, phrase) {
    if (!tokens.length) return 50;
    if (name === phrase) return 100;
    if (hasWords(name, phrase) && name.length <= phrase.length + 12) return 90;
    if (name.startsWith(phrase + " ")) return 85;
    if (hasWords(name, phrase)) return 75;
    if (tokens.every((t) => hasWords(name, t))) return 65;
    if (hasPrefix(name, phrase)) return 60;
    if (hasWords(sec, phrase) || tokens.every((t) => hasWords(sec, t))) return 55;
    const both = name + " " + sec;
    if (tokens.every((t) => hasWords(both, t) || hasPrefix(both, t))) return 45;
    return 0;
  }

  function currentValue(el, role) {
    const tag = tagOf(el);
    const type = attr(el, "type").toLowerCase();
    if (role === "checkbox" || role === "radio" || role === "switch") {
      if (tag === "input") return el.checked ? "checked" : "unchecked";
      const c = attr(el, "aria-checked");
      return c ? (c === "true" ? "checked" : c === "false" ? "unchecked" : c) : undefined;
    }
    if (tag === "select") {
      const sel = Array.from(el.options || []).filter((o) => o.selected).map((o) => clip(o.text, 40));
      return sel.join(", ");
    }
    if (tag === "input" || tag === "textarea") {
      if (type === "password") return el.value ? "(hidden)" : "";
      if (type === "button" || type === "submit" || type === "reset" || type === "image") return undefined;
      return clip(el.value, 80);
    }
    return undefined;
  }

  function find() {
    let q = " " + words(arg.query) + " ";
    for (const [re, rep] of KIND_PHRASES) q = q.replace(re, rep);
    const kinds = [];
    const tokens = [];
    for (const w of q.split(/\s+/).filter(Boolean)) {
      if (w.startsWith("@")) { if (!kinds.includes(w.slice(1))) kinds.push(w.slice(1)); }
      else if (!STOP.has(w)) tokens.push(w);
    }
    if (!kinds.length && !tokens.length) return { error: "find: the query names nothing to look for" };
    const phrase = tokens.join(" ");
    const limit = Math.max(1, Math.min(100, Number(arg.limit) || 20));
    S.next = Math.max(S.next, Number(arg.base) || 0);
    const vw = window.innerWidth, vh = window.innerHeight;

    const ix = buildIndex(true);
    const hits = [];
    let order = 0;
    for (const el of ix.all) {
      order++;
      if (!isCandidate(el)) continue;
      const role = roleOf(el);
      if (kinds.length && !kinds.some((k) => KINDS[k] && KINDS[k](role, el))) continue;
      const r = rectOf(el);
      const visible = isVisible(el, r);
      if (!visible && !arg.includeHidden) continue;
      const name = nameOf(el, ix);
      const sec = words([attr(el, "placeholder"), attr(el, "title"), attr(el, "name"), attr(el, "id"), attr(el, "alt"),
        attr(el, "data-testid"), attr(el, "type"), role === "button" ? el.value : ""].join(" "));
      let s = score(words(name), sec, tokens, phrase);
      if (!s) continue;
      if (INTERACTIVE.has(role)) s += 6;
      if (role === "label") s -= 10;
      if (role === "generic") s -= 8;
      if (el.disabled || attr(el, "aria-disabled") === "true") s -= 15;
      hits.push({ el, role, name, r, s, order, visible });
    }

    // No element matched by name/role: look for the text itself.
    if (!hits.length && tokens.length && !kinds.length) {
      order = 0;
      for (const el of ix.all) {
        order++;
        let own = "";
        for (const c of Array.from(el.childNodes || [])) if (c.nodeType === 3) own += c.data != null ? c.data : c.textContent;
        if (!hasWords(words(own), phrase) && !tokens.every((t) => hasWords(words(own), t))) continue;
        const r = rectOf(el);
        const visible = isVisible(el, r);
        if (!visible && !arg.includeHidden) continue;
        hits.push({ el, role: "text", name: clip(own, 200), r, s: 20, order, visible });
      }
    }

    // A label is folded into its control: drop it when the control matched.
    const matched = new Set(hits.map((h) => h.el));
    const kept = hits.filter((h) => {
      if (tagOf(h.el) !== "label") return true;
      const c = labelControl(h.el);
      return !(c && matched.has(c));
    });
    kept.sort((a, b) => b.s - a.s || a.order - b.order);

    const matches = kept.slice(0, limit).map((h) => {
      const x = Math.round(h.r.left + h.r.width / 2);
      const y = Math.round(h.r.top + h.r.height / 2);
      const m = { ref: refFor(h.el), role: h.role, text: clip(h.name, 120), x, y, tag: tagOf(h.el) };
      const type = attr(h.el, "type").toLowerCase();
      if (type && (m.tag === "input" || m.tag === "button")) m.type = type;
      const nm = attr(h.el, "name");
      if (nm) m.name = clip(nm, 60);
      const v = currentValue(h.el, h.role);
      if (v !== undefined) m.value = v;
      if (h.el.disabled || attr(h.el, "aria-disabled") === "true") m.disabled = true;
      if (!h.visible) m.hidden = true;
      else if (x < 0 || y < 0 || x >= vw || y >= vh) m.offscreen = true;
      return m;
    });
    return { matches, total: kept.length, next: S.next };
  }

  // ---- point (a ref -> a clickable viewport point) ----

  function deepHit(x, y) {
    let doc = document, lx = x, ly = y;
    let hit = doc.elementFromPoint(lx, ly);
    for (let i = 0; hit && i < 20; i++) {
      if (hit.shadowRoot && hit.shadowRoot.elementFromPoint) {
        const inner = hit.shadowRoot.elementFromPoint(lx, ly);
        if (inner && inner !== hit) { hit = inner; continue; }
      }
      const t = tagOf(hit);
      if (t === "iframe" || t === "frame") {
        let d = null;
        try { d = hit.contentDocument; } catch (e) {}
        if (d && d.elementFromPoint) {
          const r = hit.getBoundingClientRect();
          lx -= r.left + (hit.clientLeft || 0);
          ly -= r.top + (hit.clientTop || 0);
          doc = d;
          const inner = d.elementFromPoint(lx, ly);
          if (inner) { hit = inner; continue; }
        }
      }
      break;
    }
    return hit;
  }

  function within(el, n) {
    for (let i = 0; n && i < 1000; i++) {
      if (n === el) return true;
      n = n.parentNode || n.host || null;
    }
    return false;
  }

  function reaches(el, hit) {
    if (!hit) return false;
    if (within(el, hit)) return true;
    // A click on an element's label reaches the element, and the other way round.
    for (let n = hit, i = 0; n && i < 50; n = n.parentNode || n.host || null, i++) {
      if (tagOf(n) === "label" && labelControl(n) === el) return true;
    }
    return tagOf(el) === "label" && labelControl(el) === hit;
  }

  function point() {
    const found = lookup(arg.ref);
    if (found.error) return found;
    const el = found.el;
    const vw = window.innerWidth, vh = window.innerHeight;
    let r = rectOf(el);
    const inView = (rr) => rr.top >= 0 && rr.left >= 0 && rr.top + rr.height <= vh && rr.left + rr.width <= vw;
    if (!inView(r) && el.scrollIntoView) {
      el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
      r = rectOf(el);
    }
    if (!(r.width > 0 && r.height > 0)) {
      return { error: arg.ref + " is not visible (it has no size — hidden or collapsed). Run find again or pick another element." };
    }
    const pts = [[0.5, 0.5], [0.25, 0.5], [0.75, 0.5], [0.5, 0.25], [0.5, 0.75], [0.15, 0.15], [0.85, 0.85]];
    let onScreen = false;
    let cover = null, coverAt = null;
    for (const [fx, fy] of pts) {
      const x = Math.round(r.left + r.width * fx);
      const y = Math.round(r.top + r.height * fy);
      if (x < 0 || y < 0 || x >= vw || y >= vh) continue;
      onScreen = true;
      if (arg.noCheck) return { x, y };
      const hit = deepHit(x, y);
      if (reaches(el, hit)) return { x, y };
      if (!cover) { cover = hit; coverAt = [x, y]; }
    }
    if (!onScreen) {
      return { error: arg.ref + " is off-screen even after scrolling it into view (at " + Math.round(r.left) + "," + Math.round(r.top) + "). Scroll the page or resize the window." };
    }
    return {
      error: arg.ref + " is covered by " + describe(cover) + " at (" + coverAt[0] + ", " + coverAt[1] +
        "), so a click would hit that instead. Close or dismiss it (or click it, if that is what you meant) and try again.",
    };
  }

  // ---- fill (form_input) ----

  function boolOf(v) {
    if (typeof v === "boolean") return v;
    if (typeof v === "number") return v !== 0;
    const s = String(v == null ? "" : v).trim().toLowerCase();
    if (["true", "on", "yes", "y", "1", "checked", "check", "selected"].includes(s)) return true;
    if (["false", "off", "no", "n", "0", "", "unchecked", "uncheck", "none", "null"].includes(s)) return false;
    return undefined;
  }

  function fire(el, type, extra) {
    const view = (el.ownerDocument && el.ownerDocument.defaultView) || window;
    let ev;
    try {
      ev = type === "input" && view.InputEvent
        ? new view.InputEvent("input", { bubbles: true, composed: true, ...(extra || {}) })
        : new view.Event(type, { bubbles: true, composed: true });
    } catch (e) {
      ev = new view.Event(type, { bubbles: true });
    }
    el.dispatchEvent(ev);
  }

  // setNative sets a property through the element's prototype setter, so a
  // framework that wraps the instance's own value property (React's value
  // tracker) sees a real change in the input event.
  function setNative(el, prop, v) {
    let proto = Object.getPrototypeOf(el);
    let desc;
    while (proto && !(desc = Object.getOwnPropertyDescriptor(proto, prop))) proto = Object.getPrototypeOf(proto);
    if (desc && desc.set) desc.set.call(el, v);
    else el[prop] = v;
  }

  const settle = () => new Promise((r) => setTimeout(r, 30));
  const FORMATS = { date: "YYYY-MM-DD", time: "HH:MM", "datetime-local": "YYYY-MM-DDTHH:MM", month: "YYYY-MM", week: "YYYY-Www", color: "#rrggbb" };

  async function setChecked(el, want) {
    if (el.checked !== want) el.click();
    await settle();
    if (el.checked !== want) {
      setNative(el, "checked", want);
      fire(el, "input");
      fire(el, "change");
      await settle();
    }
    return el.checked === want;
  }

  async function fill() {
    const found = lookup(arg.ref);
    if (found.error) return found;
    const ref = arg.ref;
    let el = found.el;
    const value = arg.value;
    if (tagOf(el) === "label") el = labelControl(el) || el;
    const tag = tagOf(el);
    const type = tag === "input" ? (attr(el, "type") || "text").toLowerCase() : "";
    const fail = (msg) => ({ error: ref + " " + msg });
    if (el.disabled) return fail("is disabled, so it can't be changed.");

    if (type === "checkbox") {
      const want = boolOf(value);
      if (want === undefined) return fail("is a checkbox: give true or false, not " + JSON.stringify(value) + ".");
      if (!(await setChecked(el, want))) return fail("did not stay " + (want ? "checked" : "unchecked") + " (the page reverted it).");
      return { ok: true, ref, tag, type, value: want ? "checked" : "unchecked" };
    }
    if (type === "radio") {
      let target = el;
      const b = boolOf(value);
      if (b === false) return fail("is a radio button and can't be unchecked: choose another option of its group instead.");
      if (b === undefined) {
        // A value picks the option of the group whose value or label matches.
        const want = words(value);
        const ix = buildIndex(true);
        const group = ix.all.filter((r) => tagOf(r) === "input" && attr(r, "type").toLowerCase() === "radio" &&
          attr(r, "name") === attr(el, "name") && rootOf(r) === rootOf(el) && r.form === el.form);
        target = group.find((r) => words(r.value) === want) || group.find((r) => words(nameOf(r, ix)) === want) || null;
        if (!target) {
          return fail("is a radio group without the option " + JSON.stringify(value) + ". Options: " +
            group.map((r) => JSON.stringify(clip(nameOf(r, ix) || r.value, 40))).join(", ") + ".");
        }
      }
      if (target.disabled) return fail("option " + JSON.stringify(value) + " is disabled.");
      if (!(await setChecked(target, true))) return fail("did not stay selected (the page reverted it).");
      return { ok: true, ref, tag, type, value: clip(target.value, 80) };
    }
    if (tag === "select") {
      const opts = Array.from(el.options || []);
      const pick = (v) => {
        const s = String(v).trim();
        const w = words(s);
        return opts.find((o) => o.value === s) || opts.find((o) => String(o.text).trim() === s) ||
          opts.find((o) => words(o.text) === w) || opts.find((o) => words(o.value) === w) || null;
      };
      const listing = () => "Options: " + opts.slice(0, 30).map((o) => JSON.stringify(clip(o.text, 40))).join(", ") +
        (opts.length > 30 ? ", …" : "") + ".";
      if (el.multiple) {
        const wants = Array.isArray(value) ? value : String(value == null ? "" : value).split(",").map((s) => s.trim()).filter(Boolean);
        const chosen = [];
        for (const w of wants) {
          const o = pick(w);
          if (!o) return fail("has no option " + JSON.stringify(w) + ". " + listing());
          chosen.push(o);
        }
        for (const o of opts) o.selected = chosen.includes(o);
        fire(el, "input");
        fire(el, "change");
        await settle();
        if (!opts.every((o) => o.selected === chosen.includes(o))) return fail("did not keep the selection (the page reverted it).");
        return { ok: true, ref, tag, value: chosen.map((o) => clip(o.text, 40)).join(", ") };
      }
      const o = pick(Array.isArray(value) ? value[0] : value);
      if (!o) return fail("has no option " + JSON.stringify(value) + ". " + listing());
      if (o.disabled) return fail("option " + JSON.stringify(clip(o.text, 40)) + " is disabled.");
      setNative(el, "value", o.value);
      if (!o.selected) o.selected = true;
      fire(el, "input");
      fire(el, "change");
      await settle();
      if (el.value !== o.value) return fail("did not keep the option " + JSON.stringify(clip(o.text, 40)) + " (the page reverted it).");
      return { ok: true, ref, tag, value: clip(o.text, 80) };
    }
    const str = value == null ? "" : typeof value === "object" ? JSON.stringify(value) : String(value);
    if (tag === "input" || tag === "textarea") {
      if (type === "file") return fail("is a file input; form_input can't choose files.");
      if (["button", "submit", "reset", "image"].includes(type)) return fail("is a button; click it instead.");
      if (el.readOnly) return fail("is read-only, so it can't be changed.");
      if (el.focus) el.focus();
      setNative(el, "value", str);
      fire(el, "input", { inputType: "insertReplacementText", data: str });
      fire(el, "change");
      await settle();
      const got = String(el.value);
      const same = got === str || ((type === "number" || type === "range") && got !== "" && Number(got) === Number(str)) ||
        ((type === "email" || type === "url") && got === str.trim());
      if (!same) {
        if (FORMATS[type] && got === "") return fail("is a " + type + " input and needs a value like " + FORMATS[type] + ".");
        const shown = type === "password" ? "a different value" : JSON.stringify(clip(got, 80));
        return fail("did not take the value: it holds " + shown + " now (the page rejected, limited or reformatted it).");
      }
      return { ok: true, ref, tag, type: type || undefined, value: type === "password" ? "(hidden)" : clip(got, 80) };
    }
    if (el.isContentEditable) {
      if (el.focus) el.focus();
      el.textContent = str;
      fire(el, "input", { inputType: "insertText", data: str });
      await settle();
      if (String(el.textContent).trim() !== str.trim()) return fail("did not keep the text (the editor reverted it). Click it and use type instead.");
      return { ok: true, ref, tag, value: clip(str, 80) };
    }
    return fail("is a <" + tag + ">, which is not a form field. Click it, or find the input it belongs to.");
  }

  switch (op) {
    case "find":
      return find();
    case "point":
      return point();
    case "scroll": {
      const found = lookup(arg.ref);
      if (found.error) return found;
      if (found.el.scrollIntoView) found.el.scrollIntoView({ block: "center", inline: "center" });
      return { ok: true };
    }
    case "fill":
      return fill();
    case "info":
      return {
        dpr: window.devicePixelRatio || 1, w: window.innerWidth, h: window.innerHeight,
        sx: window.scrollX || 0, sy: window.scrollY || 0,
      };
    case "origin":
      return String(self.origin);
  }
  return { error: "unknown page operation " + op };
}

// refBlock reserves a block of `size` ref numbers for one find on tabId and
// returns its start. Blocks never overlap — not between parallel finds, and
// not across the tab's documents — so a ref names one element on one page.
async function refBlock(tabId, size) {
  let base = 0;
  await updateState(["refCounters"], (o) => {
    const m = mapOf(o.refCounters);
    base = Number(m[String(tabId)]) || 0;
    return { refCounters: { ...m, [String(tabId)]: base + size } };
  });
  return base;
}

// pageCall runs one pageAgent operation and throws its error.
async function pageCall(ctx, tabId, opName, arg, what) {
  const r = await runInPage(ctx, tabId, pageAgent, [opName, arg], what);
  if (r == null) throw new Error(what + ": the page returned nothing (it may be navigating). Try again.");
  if (r.error) throw new Error(r.error);
  return r;
}

// refPoint resolves a ref to a viewport point to click: the element is
// scrolled into view first, and a point where the element itself (not an
// overlay) is hit is chosen; otherwise the error says what covers it.
async function refPoint(ctx, tabId, ref, noCheck) {
  const r = await pageCall(ctx, tabId, "point", { ref, noCheck: !!noCheck }, "locating " + ref);
  return [r.x, r.y];
}

// coordOf reads a [x, y] coordinate (numbers, rounded to whole pixels).
function coordOf(v, what) {
  if (!Array.isArray(v)) return null;
  const x = Number(v[0]), y = Number(v[1]);
  if (v.length !== 2 || !Number.isFinite(x) || !Number.isFinite(y)) throw new Error(what + " must be [x, y] numbers");
  return [Math.round(x), Math.round(y)];
}

async function resolveCoord(ctx, tabId, params, noCheck) {
  const c = coordOf(params.coordinate, "coordinate");
  if (c) return c;
  if (params.ref) return await refPoint(ctx, tabId, params.ref, noCheck);
  return null;
}

// Computer actions that only observe the page (site check "read").
const OBSERVE_ACTIONS = new Set(["screenshot", "zoom", "wait"]);

async function doComputer(params, policy, ctx) {
  const action = params.action;
  if (!action) throw new Error("computer requires an action");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(ctx, tabId, policy, OBSERVE_ACTIONS.has(action) ? "read" : "act");

  // Screenshot/zoom capture via CDP on the (possibly background) gocode tab,
  // reusing this command's debugger session.
  if (action === "screenshot" || action === "zoom") {
    if (action === "zoom" && !Array.isArray(params.region)) throw new Error("zoom requires region [x0, y0, x1, y1]");
    return await captureTab(ctx, tabId, { region: action === "zoom" ? params.region : null, scale: params.scale });
  }
  if (action === "wait") {
    const secs = Number(params.duration === undefined ? 1 : params.duration);
    if (!Number.isFinite(secs) || secs < 0) throw new Error("wait requires a duration in seconds");
    const capped = Math.min(secs, MAX_WAIT_SECONDS);
    await bounded(sleep(capped * 1000), capped * 1000 + 1000, "wait", ctx.op);
    const out = { action, ok: true };
    if (secs > MAX_WAIT_SECONDS) out.notes = ["wait is capped at " + MAX_WAIT_SECONDS + "s; waited " + MAX_WAIT_SECONDS + "s."];
    return out;
  }
  if (action === "scroll_to") {
    if (!params.ref) throw new Error("scroll_to requires a ref");
    await pageCall(ctx, tabId, "scroll", { ref: params.ref }, "scrolling to " + params.ref);
    return { action, ok: true };
  }

  return await acting(tabId, async () => {
    // Input actions require CDP; attach via the shared session.
    await ensureAttached(ctx, tabId);
    const pointer = !["type", "key", "keypress"].includes(action);
    const coord = pointer ? await resolveCoord(ctx, tabId, params, action === "scroll") : null;
    const out = { action, tabId, ok: true };
    switch (action) {
      case "left_click":
      case "click":
        await mouseClick(ctx, tabId, coord, "left", 1, params.modifiers);
        break;
      case "right_click":
        await mouseClick(ctx, tabId, coord, "right", 1, params.modifiers);
        break;
      case "double_click":
        await mouseClick(ctx, tabId, coord, "left", 2, params.modifiers);
        break;
      case "triple_click":
        await mouseClick(ctx, tabId, coord, "left", 3, params.modifiers);
        break;
      case "hover":
      case "mouse_move":
        await mouseMove(ctx, tabId, coord);
        break;
      case "left_click_drag":
        out.drag = await mouseDrag(ctx, tabId, coordOf(params.start_coordinate, "start_coordinate"), coord);
        break;
      case "scroll":
        await mouseScroll(ctx, tabId, coord, params.scroll_direction, params.scroll_amount);
        break;
      case "type":
        await typeText(ctx, tabId, params.text || "");
        break;
      case "key":
      case "keypress":
        await pressKeys(ctx, tabId, params.text || "", params.repeat);
        break;
      default:
        throw new Error("unsupported computer action: " + action);
    }
    return out;
  });
}

// --- find (DOM query, read-only, no debugger) -------------------------------

async function doFind(params, policy, ctx) {
  const query = params.query;
  if (!query) throw new Error("find requires a query");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(ctx, tabId, policy, "read");
  const limit = Math.max(1, Math.min(100, Math.round(Number(params.limit)) || 20));
  const base = await refBlock(tabId, limit);
  const r = await pageCall(ctx, tabId, "find", { query: String(query), limit, base, includeHidden: params.includeHidden === true }, "find");
  const matches = Array.isArray(r.matches) ? r.matches : [];
  return { tabId, matches, total: r.total, truncated: (r.total || 0) > matches.length };
}

// --- form_input (value set + events, no debugger) ---------------------------

async function doFormInput(params, policy, ctx) {
  const ref = params.ref;
  if (!ref) throw new Error("form_input requires a ref");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(ctx, tabId, policy, "act");
  const r = await acting(tabId, () => pageCall(ctx, tabId, "fill", { ref, value: params.value }, "form_input " + ref));
  return { ref, ok: true, value: r.value };
}

// --- browser_batch (sequence, one shared debugger session) ------------------

async function callSub(name, input, policy, ctx) {
  switch (name) {
    case "computer": return await doComputer(input, policy, ctx);
    case "navigate": return await doNavigate(input, policy, ctx);
    case "read_page":
    case "get_page_text": return await doReadPage(input, policy, ctx);
    case "tabs_context":
    case "tabs_context_mcp": return await doTabsContext(input, policy, ctx);
    case "tabs_create":
    case "tabs_create_mcp": return await doTabsCreate(input, policy, ctx);
    case "tabs_close":
    case "tabs_close_mcp": return await doTabsClose(input);
    case "screenshot": return await doScreenshot(input, policy, ctx);
    case "find": return await doFind(input, policy, ctx);
    case "form_input": return await doFormInput(input, policy, ctx);
    case "javascript_tool": return await doJavascript(input, policy, ctx);
    case "resize_window": return await doResizeWindow(input);
    case "browser_batch": throw new Error("browser_batch cannot be nested");
    default: throw new Error("unknown batch tool: " + name);
  }
}

// doBatch runs the steps in order on one debugger session and stops at the
// first error, or when the command is cancelled. Each step's result comes
// back (find's refs, read_page's text, a javascript value, a new tab's id);
// images are returned as dataUrl, up to MAX_BATCH_IMAGE_BYTES in all.
async function doBatch(params, policy, ctx) {
  const actions = Array.isArray(params.actions) ? params.actions : [];
  if (actions.length === 0) throw new Error("browser_batch requires at least one action");
  const results = [];
  let imageBytes = 0;
  for (const act of actions) {
    const name = act && act.name;
    if (ctx.op.aborted) {
      results.push({ name, error: String(ctx.op.aborted.message || ctx.op.aborted) });
      break;
    }
    const input = { ...((act && act.input) || {}), sessionId: sessionOf(params) };
    const notesBefore = ctx.notes.length;
    try {
      const r = await callSub(name, input, policy, ctx);
      const entry = { name, ok: true };
      if (r && typeof r === "object") {
        const { dataUrl, ...rest } = r;
        if (dataUrl) {
          if (imageBytes + dataUrl.length <= MAX_BATCH_IMAGE_BYTES) {
            entry.dataUrl = dataUrl;
            imageBytes += dataUrl.length;
          } else {
            entry.imageOmitted = "the image was left out: the batch's images reached the reply size limit";
          }
        }
        entry.result = rest;
      }
      const notes = ctx.notes.slice(notesBefore);
      if (notes.length) entry.notes = notes;
      results.push(entry);
    } catch (e) {
      const entry = { name, error: String((e && e.message) || e) };
      const notes = ctx.notes.slice(notesBefore);
      if (notes.length) entry.notes = notes;
      results.push(entry);
      break; // stop on the first error
    }
  }
  // The steps carry their own notes.
  ctx.notes.length = 0;
  return { results };
}

async function doNavigate(params, policy, ctx) {
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
    // The destination is only known once there: check it like a navigation
    // target, and go the other way again when it is refused.
    if (hist === "back") await chrome.tabs.goBack(tabId);
    else await chrome.tabs.goForward(tabId);
    await waitForTabLoad(ctx, tabId, LOAD_TIMEOUT_MS);
    try {
      await checkTabSite(ctx, tabId, policy, "act");
    } catch (e) {
      try {
        if (hist === "back") await chrome.tabs.goForward(tabId);
        else await chrome.tabs.goBack(tabId);
        await waitForTabLoad(ctx, tabId, LOAD_TIMEOUT_MS);
      } catch (e2) {}
      throw e;
    }
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
  await waitForTabLoad(ctx, tab.id, LOAD_TIMEOUT_MS);
  // A redirect can land on a site the policy denies: the checks above only saw
  // the requested URL. Clear the tab rather than return the denied page's
  // title, and name only the site.
  try {
    await checkTabSite(ctx, tab.id, policy, "read");
  } catch (e) {
    try {
      await chrome.tabs.update(tab.id, { url: "about:blank" });
    } catch (e2) {}
    throw new Error("The page redirected to a site gocode in Chrome may not use: " + e.message + " The tab was cleared.");
  }
  await setCurrentTab(tab.id, sid);
  await applyKeepOpen(params, tab.id);
  const settled = await getTab(tab.id);
  return {
    tabId: tab.id,
    url: (settled && settled.url) || url,
    title: settled && settled.title,
  };
}

async function doReadPage(params, policy, ctx) {
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(ctx, tabId, policy, "read");
  let text = (await runInPage(ctx, tabId, () => (document.body ? document.body.innerText : ""), [], "read_page")) || "";
  // Paging: offset/max_chars select a window of the text, so a page longer
  // than one reply can be read to the end. total and next tell the host where
  // the window sits.
  const total = text.length;
  const offset = Math.max(0, Math.min(total, Math.floor(Number(params.offset) || 0)));
  const max = Math.max(1, Math.min(MAX_PAGE_TEXT, Math.floor(Number(params.max_chars) || MAX_PAGE_TEXT)));
  text = text.slice(offset, offset + max);
  const next = offset + text.length;
  const truncated = next < total;
  // Report the page URL so the host can label the untrusted-content fence with
  // where the text came from.
  const tab = await getTab(tabId);
  return { tabId, url: (tab && tab.url) || "", text, truncated, total, offset, next };
}

// doTabsContext lists ONLY gocode's tabs and groups — never the user's other
// tabs. With a sessionId it lists only that session's groups and their tabs.
// createIfEmpty opens a tab first when there is none; without it nothing is
// created. Each tab says whether gocode opened it (openedByGocode) and
// whether it is marked keep_open (keepOpen).
async function doTabsContext(params, policy, ctx) {
  const sid = sessionOf(params);
  if (params && params.createIfEmpty === true && (await ownedTabList(sid)).length === 0) {
    await doTabsCreate({ sessionId: sid }, policy, ctx);
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

async function doTabsCreate(params, policy, ctx) {
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
  if (params.url) await waitForTabLoad(ctx, tab.id, LOAD_TIMEOUT_MS);
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

// scaleOf reads a screenshot/zoom scale: a fraction in [0.1, 1] (default 1).
function scaleOf(v) {
  if (v === undefined || v === null || v === "") return 1;
  const s = Number(v);
  if (!Number.isFinite(s) || s <= 0) throw new Error("scale must be a number in [0.1, 1]");
  return Math.min(1, Math.max(0.1, s));
}

// captureTab screenshots a specific tab via CDP Page.captureScreenshot, so it
// works on a BACKGROUND/grouped gocode tab and never captures the user's
// foreground tab. The image is downscaled to CSS pixels so coordinates read off
// it match CDP mouse input.
//
// opts.region [x0, y0, x1, y1] (viewport CSS pixels) captures only that
// rectangle, magnified to about the viewport's size (at most 4x) for a closer
// look (zoom). opts.scale in [0.1, 1] shrinks the returned image. Either way
// the result reports the full viewport size: coordinates for clicks are always
// in that frame, never in the returned image's own pixels.
async function captureTab(ctx, tabId, opts) {
  opts = opts || {};
  const scale = scaleOf(opts.scale);
  await ensureAttached(ctx, tabId);
  const info = await viewportInfo(ctx, tabId);
  const out = { width: info.w || undefined, height: info.h || undefined, devicePixelRatio: info.dpr };
  if (opts.region) {
    const reg = opts.region.map(Number);
    if (reg.length !== 4 || !reg.every(Number.isFinite)) throw new Error("zoom region must be [x0, y0, x1, y1] numbers");
    let [x0, y0, x1, y1] = reg;
    if (info.w) { x0 = Math.max(0, x0); x1 = Math.min(info.w, x1); }
    if (info.h) { y0 = Math.max(0, y0); y1 = Math.min(info.h, y1); }
    const rw = x1 - x0, rh = y1 - y0;
    if (!(rw >= 1 && rh >= 1)) throw new Error("zoom region must have x1 > x0 and y1 > y0 inside the viewport");
    const fit = Math.min((info.w || rw) / rw, (info.h || rh) / rh);
    const zoom = Math.min(4, Math.max(1, fit));
    // The clip is in document coordinates: add the scroll offset.
    const shot = await cdp(ctx, tabId, "Page.captureScreenshot", {
      format: "png",
      captureBeyondViewport: false,
      clip: { x: x0 + (info.sx || 0), y: y0 + (info.sy || 0), width: rw, height: rh, scale: zoom },
    }, SCREENSHOT_TIMEOUT_MS);
    const iw = Math.max(1, Math.round(rw * zoom * scale));
    const ih = Math.max(1, Math.round(rh * zoom * scale));
    out.dataUrl = await resizeImage("data:image/png;base64," + shot.data, iw, ih);
    out.region = [x0, y0, x1, y1];
    out.zoom = Math.round(zoom * scale * 100) / 100;
    out.imageWidth = iw;
    out.imageHeight = ih;
    return out;
  }
  const shot = await cdp(ctx, tabId, "Page.captureScreenshot", {
    format: "png",
    captureBeyondViewport: false,
  }, SCREENSHOT_TIMEOUT_MS);
  out.dataUrl = "data:image/png;base64," + shot.data;
  if (info.w && info.h && ((info.dpr && info.dpr !== 1) || scale !== 1)) {
    const iw = Math.max(1, Math.round(info.w * scale));
    const ih = Math.max(1, Math.round(info.h * scale));
    out.dataUrl = await resizeImage(out.dataUrl, iw, ih);
    if (scale !== 1) {
      out.scale = scale;
      out.imageWidth = iw;
      out.imageHeight = ih;
    }
  }
  return out;
}

async function doScreenshot(params, policy, ctx) {
  params = params || {};
  const tabId = await resolveTabId(params.tabId, sessionOf(params)); // only gocode's tabs
  await checkTabSite(ctx, tabId, policy, "read");
  return await captureTab(ctx, tabId, { scale: params.scale });
}

// javascript_tool: evaluate code in a gocode tab with REPL semantics (the last
// expression's value is returned, top-level await works) via CDP
// Runtime.evaluate. It acts on the page, so the site must be allowed. The
// evaluation is bounded (JS_TIMEOUT_MS): a script that loops, or a promise that
// never settles, fails the call instead of holding the tab.
const MAX_JS_RESULT = 100000;

async function doJavascript(params, policy, ctx) {
  if (params.action !== undefined && params.action !== "javascript_exec") {
    throw new Error("javascript_tool: action must be 'javascript_exec'");
  }
  const code = params.text;
  if (typeof code !== "string" || !code.trim()) throw new Error("javascript_tool requires text (the code to run)");
  const tabId = await resolveTabId(params.tabId, sessionOf(params));
  await checkTabSite(ctx, tabId, policy, "act");
  let r;
  await acting(tabId, async () => {
    await ensureAttached(ctx, tabId);
    try {
      r = await cdp(ctx, tabId, "Runtime.evaluate", {
        expression: code,
        awaitPromise: true,
        returnByValue: true,
        replMode: true,
        userGesture: true,
        timeout: JS_TIMEOUT_MS,
      }, JS_TIMEOUT_MS + JS_GRACE_MS);
    } catch (e) {
      if (e && e.timedOut) {
        throw new Error(
          "javascript_tool: the script did not finish within " + JS_TIMEOUT_MS / 1000 +
            "s (a loop, or a promise that never settles) and was abandoned."
        );
      }
      throw e;
    }
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
