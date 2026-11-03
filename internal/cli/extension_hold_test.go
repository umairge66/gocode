package cli

import (
	"regexp"
	"strconv"
	"testing"
)

// holdMs is the idle grace the hold harnesses run the extension with.
const holdMs = 200

// holdHarness checks that the debugger attachment is kept between commands:
// a click / type / key / screenshot sequence on one tab attaches (and enables
// Page, and turns on a background tab's focus emulation) once; the attachment
// is detached when the grace runs out, and at once when the tab closes,
// navigates to a page gocode may not act on or leaves the group, the native
// host disconnects, a cancel arrives, the user cancels the banner, or the
// command failed; the tab's lock is never held over the grace; a page whose
// renderer was replaced gets its focus emulation again.
const holdHarness = `
const HOLD = Number(process.env.HARNESS_HOLD_MS);
const detachLog = [];
const realDetach = chrome.debugger.detach;
chrome.debugger.detach = async (t) => { detachLog.push({ tabId: t.tabId, at: Date.now() }); return realDetach(t); };
const count = (m) => cdpLog.filter((c) => c.method === m).length;
const atts = () => ext("attachments");
function page(body, E) { body.appendChild(E("input", { id: "q", box: [10, 10, 200, 20] })); }
async function sequence(tab) {
  await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  await call("computer", { action: "type", text: "hi", tabId: tab });
  await call("computer", { action: "key", text: "Enter", tabId: tab });
  return await call("computer", { action: "screenshot", tabId: tab });
}
async function heldTab(state) {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  const doc = loadPage(tab, page);
  Object.assign(doc, state || {});
  await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  if (!attached.has(tab) || !atts().has(tab)) fails.push("attachment not kept after a click");
  return { tab, doc };
}

(async () => {
  // One attach for the whole sequence; detached once the grace is over.
  let { tab, doc } = await heldTab();
  attachLog.length = 0; cdpLog.length = 0; detachLog.length = 0;
  await sequence(tab);
  if (attachLog.length !== 0 || count("Page.enable") !== 0) fails.push("sequence re-attached: attaches " + attachLog.length + " Page.enable " + count("Page.enable"));
  if (!doc.body.children[0].value.startsWith("hi")) fails.push("typed into the kept session: " + JSON.stringify(doc.body.children[0].value));
  const ended = Date.now();
  if (!attached.has(tab)) fails.push("detached before the grace ran out");
  await sleepMs(HOLD + 100);
  if (attached.has(tab) || atts().size) fails.push("still attached after the grace");
  const d = detachLog.find((x) => x.tabId === tab);
  if (!d || d.at - ended < HOLD - 20 || d.at - ended > HOLD + 80) fails.push("banner time: detached " + (d ? d.at - ended : "never") + "ms after the last command (grace " + HOLD + ")");
  const stats = ext("attachStats");
  if (stats.reused < 4) fails.push("reuse count: " + JSON.stringify(stats));

  // A background tab: focus emulation once for the sequence.
  ({ tab, doc } = await heldTab({ hidden: true }));
  cdpLog.length = 0;
  const r = await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  if (count("Emulation.setFocusEmulationEnabled") !== 0 || r.delivery !== "input" || !r.background) fails.push("kept emulation: " + count("Emulation.setFocusEmulationEnabled") + " " + JSON.stringify(r));
  // A navigation swapped the renderer and the page is hidden again: emulation goes on again.
  doc = loadPage(tab, page);
  doc.hidden = true;
  cdpLog.length = 0;
  const r2 = await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  if (count("Emulation.setFocusEmulationEnabled") !== 1 || r2.delivery !== "input") fails.push("stale emulation: " + count("Emulation.setFocusEmulationEnabled") + " " + JSON.stringify(r2));
  await sleepMs(HOLD + 50);
  if (doc.visibilityState !== "hidden") fails.push("emulation left on after the grace");

  // The lock is free while an attachment is kept: parallel commands take turns on one attachment.
  ({ tab } = await heldTab());
  attachLog.length = 0;
  const t0 = Date.now();
  const shots = await Promise.allSettled([call("screenshot", { tabId: tab }), call("screenshot", { tabId: tab }), call("computer", { action: "screenshot", tabId: tab })]);
  if (shots.some((s) => s.status !== "fulfilled") || attachLog.length !== 0) fails.push("parallel on a kept attachment: " + attachLog.length + " " + JSON.stringify(shots.map((s) => s.reason && s.reason.message)));
  if (Date.now() - t0 > HOLD) fails.push("a command waited for the grace: " + (Date.now() - t0) + "ms");

  // Tab closed.
  ({ tab } = await heldTab());
  await chrome.tabs.remove(tab);
  await sleepMs(5);
  if (atts().has(tab) || attached.has(tab)) fails.push("kept after the tab closed");

  // Navigated to a page gocode may not act on.
  ({ tab } = await heldTab());
  tabs.get(tab).url = "chrome://settings/";
  for (const f of listeners.updated) f(tab, { url: "chrome://settings/" }, tabs.get(tab));
  await sleepMs(5);
  if (atts().has(tab) || attached.has(tab)) fails.push("kept after navigating to chrome://");
  // ... or to a site the policy denies.
  const tab2 = (await call("tabs_create", { url: "https://shop.example/", sitePolicy: { allowed: [], denied: ["bank.example"], requireAllowed: false } })).tabId;
  await call("computer", { action: "left_click", coordinate: [5, 5], tabId: tab2 });
  if (!attached.has(tab2)) fails.push("not kept on an allowed page");
  tabs.get(tab2).url = "https://bank.example/";
  for (const f of listeners.updated) f(tab2, { url: "https://bank.example/" }, tabs.get(tab2));
  await sleepMs(20);
  if (atts().has(tab2) || attached.has(tab2)) fails.push("kept after navigating to a denied site");
  // A command whose click starts loading such a page detaches at once.
  tabs.get(tab2).url = "https://shop.example/";
  cdpHook = (t, method, params) => { if (method === "Input.dispatchMouseEvent" && params.type === "mouseReleased") tabs.get(tab2).pendingUrl = "https://bank.example/next"; return false; };
  await call("computer", { action: "left_click", coordinate: [5, 5], tabId: tab2 });
  cdpHook = null;
  if (attached.has(tab2)) fails.push("kept while a denied page loads");
  delete tabs.get(tab2).pendingUrl;
  session.sitePolicy = null;

  // Left the gocode group.
  ({ tab } = await heldTab());
  tabs.get(tab).groupId = -1;
  for (const f of listeners.updated) f(tab, { groupId: -1 }, tabs.get(tab));
  await sleepMs(20);
  if (atts().has(tab) || attached.has(tab)) fails.push("kept after the tab left the group");

  // The native host disconnects.
  ({ tab } = await heldTab());
  ports[0].disconnect();
  await sleepMs(5);
  if (atts().has(tab) || attached.has(tab)) fails.push("kept after the native host disconnected");
  ext("connect")();

  // A cancel arrives.
  ({ tab } = await heldTab());
  ports[ports.length - 1].deliver({ method: "cancel", params: { id: "gone" } });
  await sleepMs(5);
  if (atts().has(tab) || attached.has(tab)) fails.push("kept after a cancel");

  // The user cancels the debugging banner; the next command attaches anew.
  ({ tab } = await heldTab());
  attached.delete(tab);
  for (const f of dbgDetach) f({ tabId: tab }, "canceled_by_user");
  if (atts().has(tab)) fails.push("kept after the user cancelled the banner");
  attachLog.length = 0; cdpLog.length = 0;
  await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  if (attachLog.length !== 1 || count("Page.enable") !== 1) fails.push("no fresh attach after the banner was cancelled: " + attachLog.length);

  // A failed command detaches at once.
  cdpHook = (t, method) => method === "Input.insertText";
  try { await call("computer", { action: "type", text: "x", tabId: tab }); fails.push("hung type returned"); } catch (e) {}
  cdpHook = null;
  if (attached.has(tab) || atts().has(tab)) fails.push("kept after a failed command");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionDebuggerHold(t *testing.T) {
	runExtensionHarnessHold(t, holdHarness, holdMs)
}

// holdTimingHarness runs click, type, key, screenshot on one tab with fake
// Chrome latencies for what attaching costs (attach 40ms, Page.enable and
// setFocusEmulationEnabled 10ms each, detach 20ms) and prints the time and
// the attach-related calls, for TestExtensionDebuggerHoldTiming to compare.
const holdTimingHarness = `
const realAttach = chrome.debugger.attach, realDetach = chrome.debugger.detach, realSend = chrome.debugger.sendCommand;
chrome.debugger.attach = async (t, v) => { await sleepMs(40); return realAttach(t, v); };
chrome.debugger.detach = async (t) => { await sleepMs(20); return realDetach(t); };
chrome.debugger.sendCommand = (t, m, p, cb) => {
  if (m === "Page.enable" || m === "Emulation.setFocusEmulationEnabled") return setTimeout(() => realSend(t, m, p, cb), 10);
  return realSend(t, m, p, cb);
};
(async () => {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  const doc = loadPage(tab, (body, E) => body.appendChild(E("input", { id: "q", box: [10, 10, 200, 20] })));
  doc.hidden = true;
  attachLog.length = 0; cdpLog.length = 0;
  const t0 = Date.now();
  await call("computer", { action: "left_click", coordinate: [20, 20], tabId: tab });
  await call("computer", { action: "type", text: "hi", tabId: tab });
  await call("computer", { action: "key", text: "Enter", tabId: tab });
  await call("computer", { action: "screenshot", tabId: tab });
  const ms = Date.now() - t0;
  const n = (m) => cdpLog.filter((c) => c.method === m).length;
  console.error("TIMING ms=" + ms + " attaches=" + attachLog.length + " enables=" + n("Page.enable") + " emulations=" + n("Emulation.setFocusEmulationEnabled"));
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

var timingLine = regexp.MustCompile(`TIMING ms=(\d+) attaches=(\d+) enables=(\d+) emulations=(\d+)`)

// TestExtensionDebuggerHoldTiming compares the sequence with and without the
// idle grace: without it every action attaches, enables Page and turns on the
// background tab's focus emulation; with it the sequence does that once.
func TestExtensionDebuggerHoldTiming(t *testing.T) {
	parse := func(out string) []int {
		m := timingLine.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no timing line in:\n%s", out)
		}
		v := make([]int, 4)
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		return v
	}
	before := parse(runExtensionHarnessHold(t, holdTimingHarness, 0))
	after := parse(runExtensionHarnessHold(t, holdTimingHarness, 1500))
	t.Logf("detach after every command: %dms, %d attaches, %d Page.enable, %d focus emulations", before[0], before[1], before[2], before[3])
	t.Logf("kept for the idle grace:    %dms, %d attaches, %d Page.enable, %d focus emulations", after[0], after[1], after[2], after[3])
	if before[1] != 4 || before[2] != 4 || before[3] != 4 {
		t.Errorf("without the grace: want 4 attaches/enables/emulations, got %v", before[1:])
	}
	if after[1] != 1 || after[2] != 1 || after[3] != 1 {
		t.Errorf("with the grace: want 1 attach/enable/emulation, got %v", after[1:])
	}
	if after[0] >= before[0] {
		t.Errorf("the grace did not save time: %dms vs %dms", after[0], before[0])
	}
}
