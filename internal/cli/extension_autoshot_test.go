package cli

import "testing"

// autoShotHarness checks the screenshot a page-changing computer action
// returns after itself when asked (screenshot:true): click, type, key, scroll,
// scroll_to, drag and middle click return one (a JPEG, the viewport frame the
// screenshot action reports); without the flag, or with screenshot:false, none
// is taken; screenshot / zoom / wait never take a second one; in a batch only
// the last step does; a background tab is captured through its screencast; a
// capture that fails, or a page now on a denied site, leaves the action done
// with the reason; a navigation the click started is waited for.
const autoShotHarness = `
const shots = () => cdpLog.filter((c) => c.method === "Page.captureScreenshot");
function page(body, E) {
  body.appendChild(E("input", { id: "q", box: [10, 10, 200, 20] }));
  body.appendChild(E("button", { id: "go", box: [10, 100, 100, 30] }, "Go"));
}
async function fresh(state) {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  const doc = loadPage(tab, page);
  Object.assign(doc, state || {});
  return { tab, doc, el: (id) => allElements(doc).find((e) => e.attrs.id === id) };
}
function expectShot(what, r) {
  if (!r || typeof r.dataUrl !== "string" || !r.dataUrl.startsWith("data:image/jpeg;base64,")) { fails.push(what + ": no JPEG screenshot: " + JSON.stringify({ ...r, dataUrl: r && r.dataUrl && r.dataUrl.slice(0, 30) })); return; }
  if (!r.shot || r.shot.width !== 800 || r.shot.height !== 600 || r.shot.dataUrl) fails.push(what + ": shot frame " + JSON.stringify(r.shot));
  if (r.screenshotError) fails.push(what + ": " + r.screenshotError);
}

(async () => {
  let p = await fresh();
  p.el("q").focus();
  const acts = [
    ["left_click", { coordinate: [20, 20] }],
    ["middle_click", { coordinate: [20, 20] }],
    ["type", { text: "hi" }],
    ["key", { text: "Enter" }],
    ["scroll", { coordinate: [100, 100], scroll_direction: "down", scroll_amount: 1 }],
    ["left_click_drag", { start_coordinate: [10, 10], coordinate: [100, 100] }],
    ["hover", { coordinate: [20, 110] }],
  ];
  for (const [action, extra] of acts) {
    cdpLog.length = 0;
    const r = await call("computer", { action, tabId: p.tab, screenshot: true, ...extra });
    expectShot(action, r);
    const s = shots();
    if (s.length !== 1 || s[0].params.format !== "jpeg" || s[0].params.quality !== 80) fails.push(action + ": capture params " + JSON.stringify(s.map((c) => c.params)));
    if (r.delivery !== "input") fails.push(action + ": delivery " + r.delivery);
  }
  const ref = (await call("find", { query: "Go button", tabId: p.tab })).matches[0].ref;
  expectShot("scroll_to", await call("computer", { action: "scroll_to", ref, tabId: p.tab, screenshot: true }));

  // Without the flag, or with screenshot:false: no capture.
  for (const flag of [undefined, false]) {
    cdpLog.length = 0;
    const r = await call("computer", { action: "left_click", coordinate: [20, 110], tabId: p.tab, screenshot: flag });
    if (r.dataUrl || r.shot || shots().length) fails.push("screenshot " + flag + ": captured anyway");
  }
  // Observing actions take no second image; typing nothing takes none.
  cdpLog.length = 0;
  const own = await call("computer", { action: "screenshot", tabId: p.tab, screenshot: true });
  if (shots().length !== 1 || own.shot || !own.dataUrl.startsWith("data:image/png")) fails.push("screenshot action: " + shots().length + " captures");
  cdpLog.length = 0;
  await call("computer", { action: "wait", duration: 0, tabId: p.tab, screenshot: true });
  await call("computer", { action: "type", text: "", tabId: p.tab, screenshot: true });
  if (shots().length) fails.push("wait / empty type captured");

  // A batch: only the last step's screenshot.
  cdpLog.length = 0;
  const b = await call("browser_batch", { actions: [
    { name: "computer", input: { action: "left_click", coordinate: [20, 20], tabId: p.tab, screenshot: true } },
    { name: "computer", input: { action: "type", text: "x", tabId: p.tab, screenshot: true } },
    { name: "computer", input: { action: "key", text: "Tab", tabId: p.tab, screenshot: true } },
  ] });
  const imgs = b.results.map((x) => !!x.dataUrl);
  if (JSON.stringify(imgs) !== "[false,false,true]" || shots().length !== 1 || !b.results[2].result.shot) fails.push("batch: images " + JSON.stringify(imgs) + " captures " + shots().length);
  // ... none extra when the last step is a screenshot.
  cdpLog.length = 0;
  const b2 = await call("browser_batch", { actions: [
    { name: "computer", input: { action: "left_click", coordinate: [20, 20], tabId: p.tab, screenshot: true } },
    { name: "computer", input: { action: "screenshot", tabId: p.tab } },
  ] });
  if (JSON.stringify(b2.results.map((x) => !!x.dataUrl)) !== "[false,true]" || shots().length !== 1) fails.push("batch ending in a screenshot: " + shots().length + " captures");

  // A background tab: captured with the screencast going.
  p = await fresh({ hidden: true });
  cdpLog.length = 0;
  let r = await call("computer", { action: "left_click", coordinate: [20, 110], tabId: p.tab, screenshot: true });
  expectShot("hidden click", r);
  if (!r.background || !r.shot.background || !cdpLog.some((c) => c.method === "Page.startScreencast")) fails.push("hidden: " + JSON.stringify({ ...r, dataUrl: undefined }));
  // Nothing renders: the click is done, the screenshot error says why.
  p.doc.noFrames = true; p.doc.shotNeverAnswers = true;
  r = await call("computer", { action: "left_click", coordinate: [20, 110], tabId: p.tab, screenshot: true });
  if (!r.ok || r.dataUrl || !/rendered no image/.test(r.screenshotError || "")) fails.push("failed capture: " + JSON.stringify(r));

  // The click took the tab to a denied site: no screenshot of it.
  p = await fresh();
  const pol = { allowed: [], denied: ["bank.example"], requireAllowed: false };
  p.el("go").addEventListener("click", () => { tabs.get(p.tab).url = "https://bank.example/account"; });
  cdpLog.length = 0;
  r = await call("computer", { action: "left_click", coordinate: [20, 110], tabId: p.tab, screenshot: true, sitePolicy: pol });
  if (r.dataUrl || shots().length || !/denied on bank\.example/.test(r.screenshotError || "")) fails.push("denied site after click: " + JSON.stringify(r));
  session.sitePolicy = null;

  // A navigation the click started: the screenshot waits for the load.
  p = await fresh();
  let loadedAt = 0;
  p.el("go").addEventListener("click", () => {
    const t = tabs.get(p.tab);
    t.status = "loading";
    setTimeout(() => { t.status = "complete"; loadedAt = Date.now(); for (const f of listeners.updated) f(p.tab, { status: "complete" }, t); }, 60);
  });
  let shotAt = 0;
  cdpHook = (t, method) => { if (method === "Page.captureScreenshot") shotAt = Date.now(); return false; };
  r = await call("computer", { action: "left_click", coordinate: [20, 110], tabId: p.tab, screenshot: true });
  cdpHook = null;
  expectShot("after a navigation", r);
  if (!loadedAt || shotAt < loadedAt) fails.push("screenshot did not wait for the load: loaded " + loadedAt + " shot " + shotAt);
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionAutoScreenshot(t *testing.T) {
	runExtensionHarness(t, autoShotHarness)
}
