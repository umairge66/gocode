package cli

import "testing"

// signupPage is a form page for the find / form_input harnesses: labelled
// inputs (label[for] and a wrapping label), a password, a search box, a
// select, a checkbox in its label, a textarea, a hidden and a zero-size input,
// a button, a link, an input in an open shadow root and one in a same-origin
// iframe below the fold.
const signupPage = `
const byAttr = (doc, k, v) => allElements(doc).find((e) => e.attrs[k] === v);
function signupPage(body, E) {
  body.appendChild(E("h1", { box: [0, 0, 800, 40] }, "Sign up"));
  body.appendChild(E("form", { id: "signup", box: [0, 50, 800, 400] },
    E("label", { for: "name", box: [10, 60, 100, 20] }, "Full name"),
    E("input", { id: "name", name: "fullName", type: "text", box: [120, 60, 300, 24] }),
    E("label", { box: [10, 100, 420, 24] }, "Email ", E("input", { type: "email", name: "email", box: [120, 100, 300, 24] })),
    E("label", { for: "pw", box: [10, 140, 100, 20] }, "Password"),
    E("input", { id: "pw", type: "password", box: [120, 140, 300, 24] }),
    E("input", { type: "search", placeholder: "Search products", box: [10, 180, 300, 24] }),
    E("label", { for: "country", box: [10, 220, 100, 20] }, "Country"),
    E("select", { id: "country", box: [120, 220, 200, 24] }, E("option", { value: "us" }, "United States"), E("option", { value: "fr" }, "France")),
    E("label", { box: [10, 260, 300, 20] }, E("input", { type: "checkbox", id: "agree", box: [10, 260, 16, 16] }), " I agree to the terms"),
    E("textarea", { "aria-label": "Comments", box: [10, 300, 400, 60] }),
    E("input", { type: "hidden", name: "csrf", value: "x" }),
    E("input", { type: "text", name: "ghost", placeholder: "Ghost name", box: [0, 0, 0, 0] }),
    E("button", { type: "submit", box: [10, 400, 80, 30] }, "Submit"),
  ));
  body.appendChild(E("a", { href: "/help", box: [10, 460, 60, 20] }, "Help"));
  const host = E("div", { id: "widget", box: [450, 400, 300, 40] });
  host.attachShadow().appendChild(E("input", { "aria-label": "Shadow field", box: [460, 405, 200, 24] }));
  body.appendChild(host);
  body.appendChild(frame(E, { box: [0, 800, 400, 100] }, (fb, FE) => fb.appendChild(FE("input", { placeholder: "Card number", box: [10, 10, 200, 24] }))));
}
`

// findHarness checks what find matches (role and type words, labels folded
// into their controls, placeholder/aria-label/name attributes, shadow DOM,
// same-origin iframes, visible only) and that its refs are exact: unique per
// tab across finds and documents, stale after a navigation.
const findHarness = signupPage + `
(async () => {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  let doc = loadPage(tab, signupPage);
  const find = (q, extra) => call("find", { query: q, tabId: tab, ...(extra || {}) });
  const agentRefs = () => pages.get(tab).ctx.__gocodeAgent.refs;
  const elOf = (ref) => agentRefs().get(ref);
  async function expect(q, test, what, extra) {
    const r = await find(q, extra);
    if (!test(r.matches, r)) fails.push(q + ": " + what + "\n    got " + JSON.stringify(r.matches.map((m) => [m.ref, m.role, m.text, m.tag, m.type, m.name])));
    return r.matches;
  }
  const roles = (ms) => ms.map((m) => m.role);

  // The live failure: refs from two finds collided, and form_input on the
  // second wrote into the first one's element.
  const email = (await expect("email", (ms) => ms[0] && ms[0].type === "email", "email input first"))[0];
  const name = (await expect("name", (ms) => ms[0] && ms[0].name === "fullName", "name input first"))[0];
  if (!email || !name || email.ref === name.ref) fails.push("refs collide: " + JSON.stringify([email, name]));
  await call("form_input", { ref: name.ref, value: "Ada Lovelace", tabId: tab });
  await call("form_input", { ref: email.ref, value: "ada@example.com", tabId: tab });
  if (byAttr(doc, "id", "name").value !== "Ada Lovelace") fails.push("name input: " + byAttr(doc, "id", "name").value);
  if (byAttr(doc, "name", "email").value !== "ada@example.com") fails.push("email input: " + byAttr(doc, "name", "email").value);

  // Role / type words and labels.
  await expect("input fields", (ms) => ms.length >= 8 && !roles(ms).some((r) => r === "button" || r === "link" || r === "label") &&
    !ms.some((m) => m.name === "csrf" || m.name === "ghost"), "all visible fields, no buttons/hidden");
  await expect("text input", (ms) => ms.length >= 4 && roles(ms).every((r) => r === "textbox" || r === "searchbox"), "text boxes only");
  await expect("textbox", (ms) => ms.length >= 4 && roles(ms).every((r) => r === "textbox" || r === "searchbox"), "text boxes only");
  await expect("form", (ms) => ms[0] && ms[0].role === "form", "the form");
  await expect("input", (ms) => ms.length >= 8, "every field");
  await expect("email field", (ms) => ms[0] && ms[0].type === "email", "the email input first");
  await expect("Full name input", (ms) => ms[0] && ms[0].name === "fullName", "the input, not its label");
  await expect("Full name", (ms) => ms[0] && ms[0].name === "fullName" && !roles(ms).includes("label"), "the input; label folded in");
  await expect("Submit button", (ms) => ms[0] && ms[0].role === "button" && ms[0].text === "Submit", "the button");
  await expect("checkbox", (ms) => ms.length === 1 && ms[0].role === "checkbox", "the checkbox");
  await expect("I agree", (ms) => ms[0] && ms[0].role === "checkbox" && ms[0].value === "unchecked", "the checkbox with its state");
  await expect("country dropdown", (ms) => ms[0] && ms[0].tag === "select" && ms[0].value === "United States", "the select");
  await expect("search box", (ms) => ms[0] && ms[0].role === "searchbox", "the search input");
  await expect("password", (ms) => ms[0] && ms[0].type === "password" && ms[0].value === "", "the password (value never shown)");
  await expect("Comments", (ms) => ms[0] && ms[0].tag === "textarea", "the textarea by aria-label");
  await expect("help link", (ms) => ms[0] && ms[0].role === "link", "the link");
  await expect("Sign up", (ms) => ms[0] && ms[0].role === "heading", "the heading");
  await expect("Shadow field", (ms) => ms[0] && ms[0].tag === "input", "an input in a shadow root");
  await expect("Card number", (ms) => ms[0] && ms[0].x === 110 && ms[0].y === 822 && ms[0].offscreen === true, "an iframe input, in page coordinates");
  await expect("Ghost name", (ms) => ms.length === 0, "hidden fields are left out");
  await expect("Ghost name", (ms) => ms[0] && ms[0].hidden === true, "hidden fields on request", { includeHidden: true });
  const capped = await find("input", { limit: 3 });
  if (capped.matches.length !== 3 || !capped.truncated) fails.push("limit: " + JSON.stringify(capped));

  // Parallel finds never hand out the same ref for different elements.
  const [c, d] = await Promise.all([find("Country"), find("Comments")]);
  if (c.matches[0].ref === d.matches[0].ref || elOf(c.matches[0].ref) === elOf(d.matches[0].ref)) fails.push("parallel refs collide");
  const seen = new Map();
  for (const [ref, el] of agentRefs()) {
    for (const [r2, e2] of seen) if (e2 === el) fails.push("element has two refs " + ref + " " + r2);
    seen.set(ref, el);
  }
  // The same element keeps its ref.
  const again = (await find("email"))[0] || (await find("email")).matches[0];
  if (again.ref !== email.ref) fails.push("email ref changed: " + again.ref + " vs " + email.ref);

  // A removed element's ref says so.
  const pw = (await find("password")).matches[0];
  const pwEl = byAttr(doc, "id", "pw");
  pwEl.parentNode.removeChild(pwEl);
  try { await call("form_input", { ref: pw.ref, value: "x", tabId: tab }); fails.push("removed element filled"); }
  catch (e) { if (!/no longer on the page/.test(e.message)) fails.push("removed: " + e.message); }

  // After a navigation old refs are stale, and new refs never reuse them.
  doc = loadPage(tab, signupPage);
  try { await call("form_input", { ref: name.ref, value: "x", tabId: tab }); fails.push("stale ref filled"); }
  catch (e) { if (!/not on this page.*Run find again/.test(e.message)) fails.push("stale: " + e.message); }
  try { await call("computer", { action: "left_click", ref: email.ref, tabId: tab }); fails.push("stale ref clicked"); }
  catch (e) { if (!/not on this page/.test(e.message)) fails.push("stale click: " + e.message); }
  const fresh = (await find("name")).matches[0];
  if (fresh.ref === name.ref || fresh.ref === email.ref) fails.push("a new page reused ref " + fresh.ref);
  if (byAttr(doc, "id", "name").value !== "") fails.push("stale form_input reached the new page");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionFind(t *testing.T) {
	runExtensionHarness(t, findHarness)
}

// formHarness checks form_input: React-controlled inputs (native value setter),
// checkboxes from string booleans, contenteditable, <select multiple>, radios
// by label, date formats, and that a value the page rejects is an error.
const formHarness = `
(async () => {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  let reactState = "";
  const doc = loadPage(tab, (body, E) => {
    body.appendChild(E("input", { id: "react", "aria-label": "React field", box: [0, 0, 200, 20] }));
    body.appendChild(E("input", { id: "cb", type: "checkbox", "aria-label": "Newsletter", box: [0, 30, 16, 16] }));
    body.appendChild(E("div", { id: "ed", contenteditable: "true", "aria-label": "Editor", box: [0, 60, 300, 100] }));
    body.appendChild(E("select", { id: "multi", multiple: "", "aria-label": "Toppings", box: [0, 170, 200, 60] },
      E("option", { value: "a" }, "Cheese"), E("option", { value: "b" }, "Ham"), E("option", { value: "c" }, "Olives")));
    body.appendChild(E("div", {},
      E("input", { type: "radio", name: "size", value: "s", id: "rs", box: [0, 240, 16, 16] }), E("label", { for: "rs", box: [20, 240, 50, 16] }, "Small"),
      E("input", { type: "radio", name: "size", value: "l", id: "rl", box: [0, 260, 16, 16] }), E("label", { for: "rl", box: [20, 260, 50, 16] }, "Large")));
    body.appendChild(E("input", { id: "when", type: "date", "aria-label": "Start date", box: [0, 280, 200, 20] }));
    body.appendChild(E("input", { id: "stubborn", "aria-label": "Stubborn", box: [0, 310, 200, 20] }));
    body.appendChild(E("span", { id: "plain", "aria-label": "Plain text", box: [0, 340, 100, 20] }, "just text"));
  });
  const el = (id) => allElements(doc).find((e) => e.attrs.id === id);
  // React's value tracker: setting .value on the instance updates the tracker,
  // so React sees no change and re-renders its old state.
  const react = el("react");
  const proto = Object.getOwnPropertyDescriptor(FInput.prototype, "value");
  let tracker = "";
  Object.defineProperty(react, "value", { configurable: true, get() { return proto.get.call(this); }, set(v) { tracker = String(v); proto.set.call(this, v); } });
  react.addEventListener("input", () => { const cur = react.value; if (cur !== tracker) { reactState = cur; tracker = cur; } proto.set.call(react, reactState); });
  el("stubborn").addEventListener("input", function () { proto.set.call(this, ""); });
  let edited = 0;
  el("ed").addEventListener("input", () => edited++);

  const ref = async (q) => (await call("find", { query: q, tabId: tab })).matches[0].ref;
  const fill = (r, value) => call("form_input", { ref: r, value, tabId: tab });
  async function fillFails(r, value, re, what) {
    try { await fill(r, value); fails.push(what + ": no error"); } catch (e) { if (!re.test(e.message)) fails.push(what + ": " + e.message); }
  }

  await fill(await ref("React field"), "Ada");
  if (reactState !== "Ada" || react.value !== "Ada") fails.push("react: state " + JSON.stringify(reactState) + " value " + JSON.stringify(react.value));

  const cb = await ref("Newsletter checkbox");
  await fill(cb, true);
  if (!el("cb").checked) fails.push("checkbox true");
  await fill(cb, "false");
  if (el("cb").checked) fails.push("checkbox \"false\" left it checked");
  await fill(cb, "on");
  if (!el("cb").checked) fails.push("checkbox \"on\"");
  await fillFails(cb, "maybe", /true or false/, "checkbox maybe");

  await fill(await ref("Editor"), "Hello there");
  if (el("ed").textContent !== "Hello there" || !edited) fails.push("contenteditable: " + el("ed").textContent + " events " + edited);

  const multi = await ref("Toppings");
  const r = await fill(multi, "Cheese, Olives");
  const sel = el("multi").options.filter((o) => o.selected).map((o) => o.value).join(",");
  if (sel !== "a,c" || r.value !== "Cheese, Olives") fails.push("select multiple: " + sel + " " + JSON.stringify(r));
  await fillFails(multi, "Pineapple", /no option "Pineapple".*Cheese/, "missing option");

  const small = await ref("Small");
  await fill(small, true);
  if (!el("rs").checked) fails.push("radio true");
  await fill(small, "Large");
  if (!el("rl").checked || el("rs").checked) fails.push("radio by label");
  await fill(small, "s");
  if (!el("rs").checked) fails.push("radio by value");
  await fillFails(small, false, /can't be unchecked/, "radio false");
  await fillFails(small, "Medium", /without the option "Medium".*Small.*Large/, "radio missing option");

  const when = await ref("Start date");
  await fillFails(when, "03/11/2026", /YYYY-MM-DD/, "bad date");
  await fill(when, "2026-11-03");
  if (el("when").value !== "2026-11-03") fails.push("date: " + el("when").value);

  await fillFails(await ref("Stubborn"), "x", /did not take the value/, "page reverted value");
  await fillFails(await ref("Plain text"), "x", /not a form field/, "plain span");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionFormInput(t *testing.T) {
	runExtensionHarness(t, formHarness)
}

// clickRefHarness checks clicking by ref: an element below the fold is
// scrolled into view first and clicked inside the viewport; one under an
// overlay is refused, naming the overlay.
const clickRefHarness = `
(async () => {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  const doc = loadPage(tab, (body, E) => {
    body.appendChild(E("button", { id: "far", box: [100, 1500, 100, 40] }, "Far away"));
    body.appendChild(E("button", { id: "under", box: [100, 100, 100, 40] }, "Covered"));
    body.appendChild(E("label", { for: "terms", box: [300, 100, 100, 20] }, "Terms"));
    body.appendChild(E("input", { id: "terms", type: "checkbox", box: [280, 100, 16, 16] }));
    body.appendChild(E("div", { id: "cookie", class: "banner", "data-fixed": "", box: [0, 80, 250, 100] }, "We use cookies"));
  });
  const first = async (q) => (await call("find", { query: q, tabId: tab })).matches[0];
  const presses = () => cdpLog.filter((c) => c.method === "Input.dispatchMouseEvent" && c.params.type === "mousePressed");

  const far = await first("Far away");
  if (!far.offscreen) fails.push("far button not flagged offscreen: " + JSON.stringify(far));
  cdpLog.length = 0;
  await call("computer", { action: "left_click", ref: far.ref, tabId: tab });
  const p = presses()[0];
  if (!p || p.params.y < 0 || p.params.y >= 600 || doc.scrolls === 0) fails.push("far click: " + JSON.stringify(p && p.params) + " scrolls " + doc.scrolls);

  const under = await first("Covered");
  cdpLog.length = 0;
  try { await call("computer", { action: "left_click", ref: under.ref, tabId: tab }); fails.push("covered element clicked"); }
  catch (e) { if (!/covered by <div#cookie\.banner> "We use cookies"/.test(e.message)) fails.push("covered: " + e.message); }
  if (presses().length) fails.push("a covered click still pressed the mouse");
  if (attached.size) fails.push("debugger left attached");

  // A label over its own checkbox is not "covering" it.
  const terms = await first("Terms checkbox");
  await call("computer", { action: "left_click", ref: terms.ref, tabId: tab });
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionClickByRef(t *testing.T) {
	runExtensionHarness(t, clickRefHarness)
}

// robustnessHarness checks that nothing wedges a tab: a click that opens a
// confirm() gets the dialog dismissed and reported; a script that never
// finishes and a CDP call that never answers fail fast and detach; wait is
// capped; a page blocked by a dialog is unblocked; parallel screenshots on one
// tab take turns; DevTools on the tab gives a clear error.
const robustnessHarness = `
(async () => {
  const tab = (await call("tabs_create", { url: "https://example.com/" })).tabId;

  let pendingPress = null;
  cdpHook = (t, method, params, cb) => {
    if (method === "Input.dispatchMouseEvent" && params.type === "mousePressed") {
      pendingPress = cb;
      emitCdp(t.tabId, "Page.javascriptDialogOpening", { type: "confirm", message: "Delete everything?" });
      return true;
    }
    if (method === "Page.handleJavaScriptDialog") {
      cb({});
      if (pendingPress) { const f = pendingPress; pendingPress = null; f({}); }
      return true;
    }
    return false;
  };
  cdpLog.length = 0;
  const res = await call("computer", { action: "left_click", coordinate: [5, 5], tabId: tab });
  if (!res.notes || !/confirm dialog .*"Delete everything\?".*dismissed it \(answered Cancel\)/.test(res.notes[0])) fails.push("dialog note: " + JSON.stringify(res));
  const handled = cdpLog.find((c) => c.method === "Page.handleJavaScriptDialog");
  if (!handled || handled.params.accept !== false) fails.push("dialog not dismissed: " + JSON.stringify(handled));
  if (!cdpLog.some((c) => c.method === "Page.enable")) fails.push("Page.enable not sent");

  // A promise that never settles.
  cdpHook = (t, method) => method === "Runtime.evaluate";
  let t0 = Date.now();
  try { await call("javascript_tool", { action: "javascript_exec", text: "new Promise(() => {})", tabId: tab }); fails.push("hung script returned"); }
  catch (e) { if (!/did not finish within/.test(e.message)) fails.push("hung script: " + e.message); }
  if (Date.now() - t0 > 4000) fails.push("hung script took " + (Date.now() - t0) + "ms");
  if (attached.size) fails.push("debugger left attached after a hung script");
  // A CDP call that never answers.
  cdpHook = (t, method) => method === "Input.insertText";
  t0 = Date.now();
  try { await call("computer", { action: "type", text: "x", tabId: tab }); fails.push("hung CDP returned"); }
  catch (e) { if (!/Input\.insertText timed out/.test(e.message)) fails.push("hung CDP: " + e.message); }
  if (Date.now() - t0 > 2000) fails.push("hung CDP took " + (Date.now() - t0) + "ms");
  cdpHook = null;
  // The tab is usable afterwards.
  await call("computer", { action: "left_click", coordinate: [1, 1], tabId: tab });

  // wait is capped.
  const w = await call("computer", { action: "wait", duration: 100, tabId: tab });
  if (!w.notes || !/capped/.test(w.notes[0])) fails.push("wait cap: " + JSON.stringify(w));

  // A page blocked by a dialog: the script times out, the dialog is dismissed.
  scriptHook = (t, f, args) => (f.name === "pageAgent" && args[0] === "find" ? new Promise(() => {}) : undefined);
  cdpLog.length = 0;
  try { await call("find", { query: "x", tabId: tab }); fails.push("blocked page answered"); }
  catch (e) { if (!/blocked by a JavaScript dialog, which gocode dismissed/.test(e.message)) fails.push("blocked page: " + e.message); }
  if (!cdpLog.some((c) => c.method === "Page.handleJavaScriptDialog")) fails.push("stuck dialog not dismissed");
  scriptHook = null;

  // Parallel screenshots on one tab take turns on the debugger.
  attachLog.length = 0;
  const shots = await Promise.allSettled([
    call("screenshot", { tabId: tab }), call("screenshot", { tabId: tab }), call("computer", { action: "screenshot", tabId: tab }),
  ]);
  if (shots.some((s) => s.status !== "fulfilled")) fails.push("parallel screenshots: " + JSON.stringify(shots.map((s) => s.reason && s.reason.message)));
  if (attachLog.length !== 3 || attached.size) fails.push("attach log " + JSON.stringify(attachLog) + " attached " + attached.size);

  // DevTools (another debugger) on the tab.
  attached.add(tab);
  try { await call("screenshot", { tabId: tab }); fails.push("screenshot with DevTools attached"); }
  catch (e) { if (!/DevTools .*attached to tab/.test(e.message)) fails.push("devtools: " + e.message); }
  attached.delete(tab);
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionRobustness(t *testing.T) {
	runExtensionHarness(t, robustnessHarness)
}

// portHarness drives the extension through its native port: a cancel notice
// stops a batch mid-way and detaches; a reply for a command from a host that
// has since disconnected is never sent to the new host; a reply that
// postMessage refuses or that is too large becomes an error reply at once.
const portHarness = `
(async () => {
  const p1 = ports[0];
  if (!p1 || p1.sent[0].method !== "hello") fails.push("no hello on connect");
  const tab = (await call("tabs_create", { url: "https://example.com/" })).tabId;

  let pressed = 0;
  cdpHook = (t, method, params) => method === "Input.dispatchMouseEvent" && params.type === "mousePressed" && ++pressed === 2;
  const click = { name: "computer", input: { action: "left_click", coordinate: [5, 5], tabId: tab } };
  p1.deliver({ id: "41", method: "browser_batch", params: { actions: [click, click, click] } });
  await sleepMs(30);
  p1.deliver({ method: "cancel", params: { id: "41" } });
  await sleepMs(30);
  const reply = p1.sent.find((m) => m.id === "41");
  const steps = reply && reply.result && reply.result.results;
  if (!steps || steps.length !== 2 || !/cancelled/.test(steps[1].error || "")) fails.push("cancelled batch: " + JSON.stringify(reply));
  if (pressed !== 2) fails.push("presses after cancel: " + pressed);
  if (attached.size) fails.push("debugger left attached after cancel");
  cdpHook = null;

  // A command still running when its host disconnects: its reply is dropped.
  cdpHook = (t, method) => method === "Input.insertText";
  p1.deliver({ id: "50", method: "computer", params: { action: "type", text: "x", tabId: tab } });
  await sleepMs(20);
  p1.disconnect();
  ext("connect")();
  const p2 = ports[1];
  await sleepMs(50);
  cdpHook = null;
  if (p1.sent.some((m) => m.id === "50") || p2.sent.some((m) => m.id === "50")) fails.push("reply for a gone host was sent");
  if (attached.size) fails.push("debugger left attached after disconnect");
  p2.deliver({ id: "1", method: "ping" });
  await sleepMs(10);
  if (!p2.sent.some((m) => m.id === "1" && m.result && m.result.pong)) fails.push("new host got no reply: " + JSON.stringify(p2.sent));

  // postMessage refuses the reply: an error reply follows at once.
  p2.throwOnce = true;
  p2.deliver({ id: "2", method: "ping" });
  await sleepMs(10);
  const e2 = p2.sent.find((m) => m.id === "2");
  if (!e2 || !e2.error || !/could not be sent/.test(e2.error.message)) fails.push("refused reply: " + JSON.stringify(e2));
  // A reply over the size limit (shortened for the harness) becomes an error.
  scriptHook = (t, f) => (f.name === "" ? [{ result: "x".repeat(30000) }] : undefined);
  p2.deliver({ id: "3", method: "read_page", params: { tabId: tab } });
  await sleepMs(20);
  scriptHook = null;
  const e3 = p2.sent.find((m) => m.id === "3");
  if (!e3 || !e3.error || !/too large/.test(e3.error.message)) fails.push("oversized reply: " + JSON.stringify(e3).slice(0, 200));
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionPortProtocol(t *testing.T) {
	runExtensionHarness(t, portHarness)
}

// captureHarness checks browser_batch step results, zoom's region crop and
// scale, screenshot scale, the HiDPI ratio from CDP when page scripts are
// blocked, and left_click_drag (stepped moves; HTML5 drag-and-drop through
// CDP drag interception).
const captureHarness = `
(async () => {
  const tab = (await call("tabs_create", { url: "https://example.com/" })).tabId;

  // Batch steps return their results.
  const page = (await call("tabs_create", { url: "https://example.com/p" })).tabId;
  loadPage(page, (body, E) => body.appendChild(E("button", { box: [0, 0, 50, 20] }, "Go")));
  const b = await call("browser_batch", { actions: [
    { name: "find", input: { query: "Go button", tabId: page } },
    { name: "javascript_tool", input: { action: "javascript_exec", text: "1+1", tabId: page } },
    { name: "tabs_create", input: {} },
    { name: "computer", input: { action: "screenshot", tabId: page } },
  ] });
  const rs = b.results;
  if (!rs[0].result || !rs[0].result.matches || !/^ref_\d+$/.test(rs[0].result.matches[0].ref)) fails.push("batch find: " + JSON.stringify(rs[0]));
  if (!rs[1].result || rs[1].result.value !== "2") fails.push("batch javascript: " + JSON.stringify(rs[1]));
  if (!rs[2].result || typeof rs[2].result.tabId !== "number") fails.push("batch tabs_create: " + JSON.stringify(rs[2]));
  if (!rs[3].dataUrl || (rs[3].result && rs[3].result.dataUrl)) fails.push("batch screenshot: " + JSON.stringify(rs[3]));

  // zoom crops the region and magnifies it; scale shrinks the image.
  cdpLog.length = 0; resized.length = 0;
  const z = await call("computer", { action: "zoom", region: [100, 50, 300, 150], tabId: tab });
  const cap = cdpLog.find((c) => c.method === "Page.captureScreenshot");
  const clip = cap && cap.params.clip;
  if (!clip || clip.x !== 100 || clip.y !== 50 || clip.width !== 200 || clip.height !== 100 || clip.scale !== 4) fails.push("zoom clip: " + JSON.stringify(clip));
  if (JSON.stringify(resized) !== "[[800,400]]" || z.zoom !== 4 || z.width !== 800 || JSON.stringify(z.region) !== "[100,50,300,150]") fails.push("zoom: " + JSON.stringify(resized) + " " + JSON.stringify({ ...z, dataUrl: undefined }));
  resized.length = 0;
  const zs = await call("computer", { action: "zoom", region: [100, 50, 300, 150], scale: 0.5, tabId: tab });
  if (JSON.stringify(resized) !== "[[400,200]]" || zs.zoom !== 2) fails.push("zoom scale: " + JSON.stringify(resized) + " " + zs.zoom);
  try { await call("computer", { action: "zoom", tabId: tab }); fails.push("zoom without region"); } catch (e) { if (!/region/.test(e.message)) fails.push("zoom no region: " + e.message); }
  resized.length = 0;
  const s = await call("screenshot", { tabId: tab, scale: 0.5 });
  if (JSON.stringify(resized) !== "[[400,300]]" || s.scale !== 0.5 || s.width !== 800) fails.push("screenshot scale: " + JSON.stringify(resized) + " " + JSON.stringify({ ...s, dataUrl: undefined }));

  // HiDPI page with scripting blocked: the ratio comes from CDP.
  scriptHook = (t, f, args) => { if (f.name === "pageAgent" && args[0] === "info") throw new Error("Cannot access contents of the page"); };
  cdpHook = (t, method, params, cb) => {
    if (method !== "Page.getLayoutMetrics") return false;
    cb({ cssLayoutViewport: { clientWidth: 800, clientHeight: 600 }, layoutViewport: { clientWidth: 1600, clientHeight: 1200 },
      cssVisualViewport: { pageX: 0, pageY: 0, clientWidth: 800, clientHeight: 600 } });
    return true;
  };
  resized.length = 0;
  const hd = await call("screenshot", { tabId: tab });
  if (hd.devicePixelRatio !== 2 || JSON.stringify(resized) !== "[[800,600]]") fails.push("hidpi: " + hd.devicePixelRatio + " " + JSON.stringify(resized));
  scriptHook = null; cdpHook = null;

  // A plain drag moves in steps.
  cdpLog.length = 0;
  const d1 = await call("computer", { action: "left_click_drag", start_coordinate: [10, 10], coordinate: [300, 200], tabId: tab });
  const moves = cdpLog.filter((c) => c.method === "Input.dispatchMouseEvent" && c.params.type === "mouseMoved" && c.params.buttons === 1);
  if (d1.drag !== "mouse drag" || moves.length < 5 || moves[moves.length - 1].params.x !== 300) fails.push("plain drag: " + d1.drag + " moves " + moves.length);
  // An HTML5 drag is intercepted and dropped through CDP.
  let n = 0;
  cdpHook = (t, method, params, cb) => {
    if (method === "Input.dispatchMouseEvent" && params.type === "mouseMoved" && params.buttons === 1 && ++n === 2) {
      cb({});
      emitCdp(t.tabId, "Input.dragIntercepted", { data: { items: [{ mimeType: "text/plain", data: "card-7" }], dragOperationsMask: 1 } });
      return true;
    }
    return false;
  };
  cdpLog.length = 0;
  const d2 = await call("computer", { action: "left_click_drag", start_coordinate: [10, 10], coordinate: [300, 200], tabId: tab });
  cdpHook = null;
  const drags = cdpLog.filter((c) => c.method === "Input.dispatchDragEvent");
  const intercept = cdpLog.filter((c) => c.method === "Input.setInterceptDrags").map((c) => c.params.enabled);
  if (d2.drag !== "drag-and-drop" || drags.map((c) => c.params.type).join(",") !== "dragEnter,dragOver,drop" ||
    drags[2].params.x !== 300 || drags[2].params.data.items[0].data !== "card-7") fails.push("html5 drag: " + d2.drag + " " + JSON.stringify(drags.map((c) => c.params)));
  if (JSON.stringify(intercept) !== "[true,false]") fails.push("setInterceptDrags: " + JSON.stringify(intercept));
  if (!cdpLog.some((c) => c.method === "Input.dispatchMouseEvent" && c.params.type === "mouseReleased" && c.params.x === 300)) fails.push("drag not released at the end");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionCaptureAndDrag(t *testing.T) {
	runExtensionHarness(t, captureHarness)
}

// stateHarness checks that parallel commands do not lose each other's updates
// to the session state (tab and group ownership).
const stateHarness = `
(async () => {
  const made = await Promise.all([1, 2, 3, 4, 5].map(() => call("tabs_create", { sessionId: "P" })));
  const gs = new Set(made.map((m) => tabs.get(m.tabId).groupId));
  if (gs.size !== 1) fails.push("parallel tabs_create made " + gs.size + " groups");
  for (const m of made) if (!(session.openedTabs || []).includes(m.tabId)) fails.push("tab " + m.tabId + " not recorded as opened");
  const listed = await call("tabs_context", { sessionId: "P" });
  if (listed.tabs.length !== 5) fails.push("listed " + listed.tabs.length + " tabs");
  await Promise.all(made.map((m, i) => call("navigate", { tabId: m.tabId, url: "https://p.example/" + i, keep_open: true })));
  if ((session.keptTabs || []).length !== 5) fails.push("keep_open marks lost: " + JSON.stringify(session.keptTabs));
  await chrome.tabs.remove(made.map((m) => m.tabId));
  await sleepMs(20);
  if ((session.keptTabs || []).length || (session.openedTabs || []).length) fails.push("closed tabs still recorded: " + JSON.stringify(session));
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionStateUpdates(t *testing.T) {
	runExtensionHarness(t, stateHarness)
}

// historySiteHarness checks the per-site policy where the page is not the
// navigation target: back/forward is checked at its destination and undone
// when refused; a tab is checked at its pending URL too; an about:blank popup
// takes its creator's site, a blob: URL its origin; file: pages can be read
// but not acted on.
const historySiteHarness = `
async function refused(name, m, p, re) {
  try { await call(m, p); fails.push(name + ": was allowed"); }
  catch (e) { if (!re.test(e.message)) fails.push(name + ": wrong error: " + e.message); }
}
async function allowed(name, m, p) {
  try { return await call(m, p); } catch (e) { fails.push(name + ": refused: " + e.message); }
}
const pol = { allowed: ["example.com"], denied: ["bank.example"], requireAllowed: true };
(async () => {
  const t = (await call("tabs_create", { url: "https://example.com/a", sitePolicy: pol })).tabId;
  let undone = 0;
  chrome.tabs.goBack = async (id) => { tabs.get(id).url = "https://bank.example/"; };
  chrome.tabs.goForward = async (id) => { undone++; tabs.get(id).url = "https://example.com/a"; };
  await refused("back to a denied site", "navigate", { url: "back", tabId: t }, /denied on bank\.example/);
  if (undone !== 1 || tabs.get(t).url !== "https://example.com/a") fails.push("denied back not undone: " + undone + " " + tabs.get(t).url);
  chrome.tabs.goBack = async (id) => { tabs.get(id).url = "https://other.example/"; };
  await refused("back to a site that needs approval", "navigate", { url: "back", tabId: t }, /\[chrome_site=other\.example\]/);
  chrome.tabs.goBack = async (id) => { tabs.get(id).url = "chrome://newtab/"; };
  await refused("back to a non-web page", "navigate", { url: "back", tabId: t }, /browser-internal/);
  chrome.tabs.goBack = async (id) => { tabs.get(id).url = "https://example.com/prev"; };
  await allowed("back to an allowed page", "navigate", { url: "back", tabId: t });

  // The page a tab is loading counts too.
  tabs.get(t).pendingUrl = "https://bank.example/x";
  await refused("acting while a denied page loads", "computer", { action: "type", text: "x", tabId: t }, /denied on bank\.example/);
  delete tabs.get(t).pendingUrl;
  await allowed("acting on the allowed page", "computer", { action: "type", text: "x", tabId: t });

  // An about:blank popup opened from a page takes that page's site.
  const pop = await linkFrom(t, "about:blank");
  loadPage(pop.id, () => {}, "https://bank.example");
  await refused("about:blank written by a denied site", "computer", { action: "type", text: "x", tabId: pop.id }, /denied on bank\.example/);
  loadPage(pop.id, () => {}, "null");
  await allowed("an empty about:blank", "computer", { action: "key", text: "x", tabId: pop.id });
  pages.delete(pop.id);
  tabs.get(pop.id).url = "blob:https://bank.example/1234";
  await refused("blob: of a denied site", "read_page", { tabId: pop.id }, /denied on bank\.example/);
  tabs.get(pop.id).url = "file:///etc/passwd";
  await refused("acting on file:", "computer", { action: "type", text: "x", tabId: pop.id }, /browser-internal/);
  await refused("form_input on file:", "form_input", { ref: "ref_1", value: "x", tabId: pop.id }, /browser-internal/);
  await allowed("reading file:", "read_page", { tabId: pop.id });
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionHistoryAndPageSites(t *testing.T) {
	runExtensionHarness(t, historySiteHarness)
}

// redirectPagingHarness checks that a navigation a redirect lands on a denied
// site is cleared (the denied page's title never reaches the caller), and that
// read_page pages a long text by offset.
const redirectPagingHarness = `
const pol = { allowed: ["example.com"], denied: ["bank.example"], requireAllowed: true };
(async () => {
  const t = (await call("tabs_create", { url: "https://example.com/a", sitePolicy: pol })).tabId;
  const realUpdate = chrome.tabs.update;
  chrome.tabs.update = async (id, p) => {
    if (p.url === "https://example.com/go") p = { url: "https://bank.example/private", title: "Bank balance 1234" };
    return realUpdate(id, p);
  };
  try {
    await call("navigate", { url: "https://example.com/go", tabId: t, sitePolicy: pol });
    fails.push("redirect to a denied site was allowed");
  } catch (e) {
    if (!/denied on bank\.example/.test(e.message) || /1234/.test(e.message)) fails.push("redirect error: " + e.message);
  }
  if (tabs.get(t).url !== "about:blank") fails.push("redirected tab not cleared: " + tabs.get(t).url);
  chrome.tabs.update = realUpdate;

  await call("navigate", { url: "https://example.com/long", tabId: t, sitePolicy: pol });
  const text = "x".repeat(95) + "END";
  loadPage(t, (body, E) => body.appendChild(E("p", {}, text)));
  const a = await call("read_page", { tabId: t, max_chars: 40, sitePolicy: pol });
  if (a.text.length !== 40 || a.offset !== 0 || a.next !== 40 || a.total !== 98 || !a.truncated) fails.push("first page: " + JSON.stringify(a));
  const b = await call("read_page", { tabId: t, offset: 80, max_chars: 40, sitePolicy: pol });
  if (!b.text.endsWith("END") || b.next !== 98 || b.truncated) fails.push("last page: " + JSON.stringify(b));
  const c = await call("read_page", { tabId: t, sitePolicy: pol });
  if (c.text !== text || c.truncated) fails.push("whole page: " + JSON.stringify(c));
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionRedirectAndPaging(t *testing.T) {
	runExtensionHarness(t, redirectPagingHarness)
}

// backgroundHarness checks input and screenshots on a gocode tab the user is
// not looking at (document hidden), the live failure where a click returned
// "done" but the page got nothing: a visible tab takes real input and says so;
// a hidden tab gets focus emulation and real input, staying in the
// background; when Chrome still drops the input the click, keys and text go
// in as DOM events and the result says so; what cannot be delivered is an
// error; scrolling a hidden tab never sends a CDP wheel; screenshots of a
// hidden tab come through a screencast or fail fast saying why; a tab a
// background click opens comes up behind the user's tab.
const backgroundHarness = `
function cookiePage(body, E) {
  body.appendChild(E("form", { id: "f", box: [0, 0, 400, 40] }, E("input", { id: "q", name: "q", box: [10, 10, 200, 20] })));
  body.appendChild(E("div", { id: "cookie", "data-fixed": "", box: [0, 300, 800, 200] }, E("button", { id: "accept", box: [300, 350, 200, 60] }, "Accept all")));
  body.appendChild(E("iframe", { id: "ad", box: [600, 0, 200, 100] })); // another site's frame
}
async function fresh(state) {
  const tab = (await call("tabs_create", { url: "https://shop.example/" })).tabId;
  const doc = loadPage(tab, cookiePage);
  Object.assign(doc, state || {});
  const el = (id) => allElements(doc).find((e) => e.attrs.id === id);
  const p = { tab, doc, el, clicks: [], submitted: 0, onClick: null };
  el("accept").addEventListener("click", (e) => {
    p.clicks.push(e.isTrusted);
    const c = el("cookie");
    if (c && c.parentNode) c.parentNode.removeChild(c);
    if (p.onClick) p.onClick();
  });
  el("f").requestSubmit = () => p.submitted++;
  return p;
}
const sent = (m, type) => cdpLog.filter((c) => c.method === m && (!type || c.params.type === type));
async function clickAccept(p) {
  const ref = (await call("find", { query: "Accept all", tabId: p.tab })).matches[0].ref;
  cdpLog.length = 0;
  return await call("computer", { action: "left_click", ref, tabId: p.tab });
}
async function failsWith(what, m, params, re) {
  try { await call(m, params); fails.push(what + ": no error"); }
  catch (e) { if (!re.test(e.message)) fails.push(what + ": wrong error: " + e.message); }
}

(async () => {
  // A visible tab: real input, reported as delivered; no emulation.
  let p = await fresh();
  let r = await clickAccept(p);
  if (r.delivery !== "input" || r.background || p.el("cookie") || p.clicks.join() !== "true") fails.push("visible click: " + JSON.stringify(r) + " clicks " + p.clicks);
  if (sent("Emulation.setFocusEmulationEnabled").length) fails.push("focus emulation on a visible tab");

  // A hidden tab: focus emulation makes Chrome deliver real input, without
  // bringing the tab to the front; detaching ends the emulation.
  p = await fresh({ hidden: true });
  r = await clickAccept(p);
  if (r.delivery !== "input" || !r.background || p.el("cookie") || p.clicks.join() !== "true") fails.push("hidden click: " + JSON.stringify(r) + " clicks " + p.clicks);
  if (sent("Emulation.setFocusEmulationEnabled").length !== 1) fails.push("no focus emulation on a hidden tab");
  if (p.doc.visibilityState !== "hidden" || attached.size) fails.push("emulation or debugger left on: " + p.doc.visibilityState + " " + attached.size);
  if (tabs.get(p.tab).active || !tabs.get(1).active) fails.push("the hidden tab was brought to the front");

  // The live failure: Chrome takes the CDP click for a hidden tab and drops
  // it. The click goes in as DOM events, and the result says so.
  p = await fresh({ hidden: true, noFocusEmulation: true });
  const accept = p.el("accept");
  r = await clickAccept(p);
  if (r.delivery !== "dom" || r.domReason !== "not-delivered" || !r.background) fails.push("dropped click: " + JSON.stringify(r));
  if (p.el("cookie") || p.clicks.join() !== "false") fails.push("DOM click did not reach the button: clicks " + p.clicks);
  if (!sent("Input.dispatchMouseEvent", "mousePressed").length) fails.push("CDP was not tried first");
  if (!cdpLog.some((c) => c.method === "Runtime.evaluate" && c.params.userGesture === true)) fails.push("no user activation before the DOM click");
  if (p.doc.activeElement !== accept) fails.push("the DOM click did not focus the button");
  // Nothing can be delivered (another site's frame): an error, never "done".
  await failsWith("click into another site's frame", "computer", { action: "left_click", coordinate: [650, 50], tabId: p.tab },
    /left_click was not delivered: the tab is in the background \(hidden\).*frame from another site/);

  // Keys on a hidden tab: real input with emulation; DOM key events without,
  // with the default actions pages rely on (typing, Backspace, Enter submits).
  p = await fresh({ hidden: true });
  p.el("q").focus();
  r = await call("computer", { action: "key", text: "a", tabId: p.tab });
  if (r.delivery !== "input" || p.el("q").value !== "a") fails.push("hidden key: " + JSON.stringify(r) + " value " + p.el("q").value);
  p = await fresh({ hidden: true, noFocusEmulation: true });
  p.el("q").focus();
  const keys = [];
  p.el("q").addEventListener("keydown", (e) => keys.push(e.key + ":" + e.isTrusted));
  r = await call("computer", { action: "key", text: "a b Backspace c Enter", tabId: p.tab });
  if (r.delivery !== "dom" || p.el("q").value !== "ac" || p.submitted !== 1) fails.push("dropped keys: " + JSON.stringify(r) + " value " + p.el("q").value + " submitted " + p.submitted);
  if (keys.join() !== "a:false,b:false,Backspace:false,c:false,Enter:false") fails.push("DOM keys: " + keys);

  // Text: Chrome inserts it into a hidden page; with nothing focused it fails.
  p.el("q").value = "";
  r = await call("computer", { action: "type", text: "hi", tabId: p.tab });
  if (r.delivery !== "input" || p.el("q").value !== "hi") fails.push("hidden type: " + JSON.stringify(r) + " value " + p.el("q").value);
  p.doc.activeElement = null;
  await failsWith("type with nothing focused", "computer", { action: "type", text: "zz", tabId: p.tab },
    /type was not delivered: nothing on the page took the text.*no text field has focus/);

  // Scrolling a hidden tab is done in the page: no CDP wheel, which could
  // switch the user's tabs.
  p = await fresh({ hidden: true });
  cdpLog.length = 0;
  r = await call("computer", { action: "scroll", coordinate: [100, 100], scroll_direction: "down", scroll_amount: 3, tabId: p.tab });
  if (r.delivery !== "dom" || r.domReason !== "background-scroll" || p.doc.scrollY !== 300) fails.push("hidden scroll: " + JSON.stringify(r) + " scrollY " + p.doc.scrollY);
  if (sent("Input.dispatchMouseEvent", "mouseWheel").length) fails.push("a CDP wheel was sent to a hidden tab");

  // A page gocode cannot check (extension scripts blocked): sent, unconfirmed.
  p = await fresh();
  scriptHook = (t, f, args) => { if (f.name === "pageAgent" && (args[0] === "watch" || args[0] === "seen")) throw new Error("Cannot access contents of the page"); };
  r = await call("computer", { action: "left_click", coordinate: [400, 380], tabId: p.tab });
  scriptHook = null;
  if (r.delivery !== "unconfirmed" || p.clicks.join() !== "true") fails.push("unchecked click: " + JSON.stringify(r));

  // Screenshots of a hidden tab: with a screencast running, not a 20s hang.
  p = await fresh({ hidden: true });
  cdpLog.length = 0;
  const s1 = await call("screenshot", { tabId: p.tab });
  if (!s1.dataUrl || !s1.background || s1.frame) fails.push("hidden screenshot: " + JSON.stringify({ ...s1, dataUrl: !!s1.dataUrl }));
  if (sent("Page.startScreencast").length !== 1 || sent("Page.stopScreencast").length !== 1) fails.push("screencast not started and stopped");
  // The capture never answers, but frames come: the last frame is used.
  p.doc.shotNeverAnswers = true;
  const s2 = await call("computer", { action: "screenshot", tabId: p.tab });
  if (!s2.dataUrl || !s2.frame) fails.push("screenshot from a frame: " + JSON.stringify({ ...s2, dataUrl: !!s2.dataUrl }));
  resized.length = 0;
  const z = await call("computer", { action: "zoom", region: [100, 50, 300, 150], tabId: p.tab });
  if (!z.frame || JSON.stringify(resized) !== "[[800,400]]") fails.push("zoom from a frame: " + JSON.stringify(resized) + " " + z.frame);
  // Nothing renders at all: it fails fast, saying why.
  p.doc.noFrames = true;
  await failsWith("hidden tab that renders nothing", "screenshot", { tabId: p.tab },
    /tab \d+: the tab is in the background \(hidden\) and Chrome rendered no image of it/);
  if (attached.size) fails.push("debugger left attached after screenshots");

  // A link a background click follows opens its tab in front (and Chrome
  // names the user's tab as its opener): the user's tab comes back to the
  // front, and the new tab is gocode's.
  p = await fresh({ hidden: true });
  let opened = null;
  p.onClick = () => {
    opened = addTab({ id: nextTab++, url: "https://shop.example/popup", openerTabId: 1, groupId: tabs.get(p.tab).groupId, active: true });
    tabs.get(1).active = false;
    for (const f of listeners.created) f(opened);
    for (const f of listeners.activated) f({ tabId: opened.id, windowId: 1 });
  };
  await clickAccept(p);
  await sleepMs(30);
  if (!opened || !tabs.get(1).active) fails.push("the user's tab was not brought back to the front");
  if (opened && !(session.openedTabs || []).includes(opened.id)) fails.push("the tab the click opened is not recorded as gocode's");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionBackgroundTabs(t *testing.T) {
	runExtensionHarness(t, backgroundHarness)
}
