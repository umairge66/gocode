package cli

import "testing"

// sessionsHarness checks the extension's per-session tab groups: each session
// gets its own gocode group, tabs_context with a sessionId lists only that
// session's group, and a group-pinned tabs_close refuses a tab outside the
// named group or another session's group.
const sessionsHarness = `
const ids = (r) => r.tabs.map((t) => t.id).sort((a, b) => a - b).join(",");
async function refused(name, m, p, re) {
  try { await call(m, p); fails.push(name + ": was allowed"); }
  catch (e) { if (!re.test(e.message)) fails.push(name + ": wrong error: " + e.message); }
}
(async () => {
  const a1 = await call("tabs_create", { url: "https://a.example/", sessionId: "A" });
  const b1 = await call("tabs_create", { url: "https://b.example/", sessionId: "B" });
  const a2 = await call("tabs_create", { sessionId: "A" });
  const gA = tabs.get(a1.tabId).groupId, gB = tabs.get(b1.tabId).groupId;
  if (gA === gB) fails.push("sessions share a group");
  if (tabs.get(a2.tabId).groupId !== gA) fails.push("session A's second tab left its group");
  if (groups.get(gA).title !== "gocode" || groups.get(gB).title !== "gocode") fails.push("groups not titled gocode");

  let ctxA = await call("tabs_context", { sessionId: "A" });
  if (ids(ctxA) !== [a1.tabId, a2.tabId].join(",")) fails.push("A lists " + ids(ctxA));
  if (ctxA.groups.length !== 1 || ctxA.groups[0].id !== gA) fails.push("A groups " + JSON.stringify(ctxA.groups));
  const ctxB = await call("tabs_context", { sessionId: "B" });
  if (ids(ctxB) !== String(b1.tabId)) fails.push("B lists " + ids(ctxB));
  const all = await call("tabs_context", {});
  if (ids(all) !== [a1.tabId, b1.tabId, a2.tabId].sort((x, y) => x - y).join(",")) fails.push("unscoped lists " + ids(all));

  // A session without tabs gets nothing created unless it asks.
  const none = await call("tabs_context", { sessionId: "C", createIfEmpty: false });
  if (none.tabs.length || none.groups.length) fails.push("C without createIfEmpty: " + JSON.stringify(none));
  const made = await call("tabs_context", { sessionId: "C", createIfEmpty: true });
  if (made.tabs.length !== 1 || [gA, gB].includes(made.tabs[0].groupId)) fails.push("C createIfEmpty: " + JSON.stringify(made));

  // Defaults follow the session: B reads its own tab, not A's current one.
  const page = await call("read_page", { sessionId: "B" });
  if (page.tabId !== b1.tabId) fails.push("B's read_page used tab " + page.tabId);
  const nav = await call("navigate", { url: "https://a.example/next", sessionId: "A" });
  if (nav.tabId !== a2.tabId) fails.push("A's navigate used tab " + nav.tabId);

  // A link opened from A's tab joins A's group.
  const child = await chrome.tabs.create({ url: "https://a.example/link", openerTabId: a1.tabId });
  await new Promise((r) => setTimeout(r, 10));
  if (tabs.get(child.id).groupId !== gA) fails.push("link from A's tab not in A's group");

  // Group-pinned closes.
  await refused("close A's tab as group B", "tabs_close", { tabId: a1.tabId, groupId: gB, sessionId: "A" }, /not in tab group/);
  await refused("close A's tab from session B", "tabs_close", { tabId: a1.tabId, groupId: gA, sessionId: "B" }, /another gocode session/);
  await refused("close the user's tab", "tabs_close", { tabId: 1, groupId: gA, sessionId: "A" }, /not a gocode tab/);
  if (!tabs.has(a1.tabId) || !tabs.has(1)) fails.push("a refused close removed a tab");
  await call("tabs_close", { tabId: a1.tabId, groupId: gA, sessionId: "A" });
  if (tabs.has(a1.tabId)) fails.push("A's close did not close its tab");

  // A tab the user drags into A's group is shared with A: listed and closable.
  tabs.get(1).groupId = gA;
  ctxA = await call("tabs_context", { sessionId: "A" });
  if (!ctxA.tabs.some((t) => t.id === 1)) fails.push("shared tab not listed for A");
  if (ctxB.tabs.some((t) => t.id === 1)) fails.push("shared tab listed for B");

  // browser_batch steps run in the batch's session.
  const batch = await call("browser_batch", { sessionId: "B", actions: [{ name: "read_page", input: {} }] });
  if (batch.results[0].error) fails.push("batch read_page: " + batch.results[0].error);
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionSessionGroups(t *testing.T) {
	runExtensionHarness(t, sessionsHarness)
}

// cleanupHarness checks what an end-of-session close may touch: tabs_context
// flags the tabs gocode opened (openedByGocode) and the ones marked keep_open
// (keepOpen); tabs_close with cleanup:true closes several tabs in one command
// and refuses a tab the user dragged into the group, a kept tab, a tab outside
// the group and a tab that is not gocode's at all.
const cleanupHarness = `
const byId = (r) => Object.fromEntries(r.tabs.map((t) => [t.id, t]));
(async () => {
  const a = await call("tabs_create", { url: "https://a.example/", sessionId: "A" });
  const kept = await call("tabs_create", { url: "https://report.example/", sessionId: "A", keep_open: true });
  const nav = await call("navigate", { url: "https://n.example/", tabId: a.tabId, sessionId: "A" });
  const g = tabs.get(a.tabId).groupId;
  // A link followed from a gocode tab counts as opened by gocode.
  const link = await chrome.tabs.create({ url: "https://a.example/link", openerTabId: a.tabId });
  await new Promise((r) => setTimeout(r, 10));
  // The user drags their own tab into the group.
  tabs.get(1).groupId = g;
  const b = await call("tabs_create", { url: "https://b.example/", sessionId: "B" });

  let ctx = byId(await call("tabs_context", { sessionId: "A" }));
  if (!ctx[a.tabId] || ctx[a.tabId].openedByGocode !== true || ctx[a.tabId].keepOpen !== false) fails.push("a: " + JSON.stringify(ctx[a.tabId]));
  if (!ctx[kept.tabId] || ctx[kept.tabId].openedByGocode !== true || ctx[kept.tabId].keepOpen !== true) fails.push("kept: " + JSON.stringify(ctx[kept.tabId]));
  if (!ctx[link.id] || ctx[link.id].openedByGocode !== true) fails.push("link: " + JSON.stringify(ctx[link.id]));
  if (!ctx[1] || ctx[1].openedByGocode !== false || ctx[1].keepOpen !== false) fails.push("user tab: " + JSON.stringify(ctx[1]));
  if (nav.tabId !== a.tabId) fails.push("navigate used tab " + nav.tabId);

  // keep_open can be set and cleared by navigate; the mark is in session storage.
  await call("navigate", { url: "https://n.example/2", tabId: a.tabId, sessionId: "A", keep_open: true });
  if (!(session.keptTabs || []).includes(a.tabId)) fails.push("navigate keep_open not stored: " + JSON.stringify(session.keptTabs));
  await call("navigate", { url: "https://n.example/3", tabId: a.tabId, sessionId: "A", keep_open: false });
  await call("navigate", { url: "https://n.example/4", tabId: a.tabId, sessionId: "A" });
  ctx = byId(await call("tabs_context", { sessionId: "A" }));
  if (ctx[a.tabId].keepOpen !== false || ctx[kept.tabId].keepOpen !== true) fails.push("keep_open after clearing: " + JSON.stringify(ctx));

  // An end-of-session close: only a and the link tab go.
  const ids = [link.id, kept.tabId, 1, a.tabId, b.tabId, 99999];
  const res = await call("tabs_close", { tabIds: ids, groupId: g, sessionId: "A", cleanup: true });
  const closed = (res.closed || []).slice().sort((x, y) => x - y).join(",");
  if (closed !== [a.tabId, link.id].sort((x, y) => x - y).join(",")) fails.push("cleanup closed " + JSON.stringify(res));
  const why = Object.fromEntries((res.refused || []).map((r) => [r.tabId, r.error]));
  if (!/was not opened by gocode/.test(why[1] || "")) fails.push("user tab: " + why[1]);
  if (!/keep_open/.test(why[kept.tabId] || "")) fails.push("kept tab: " + why[kept.tabId]);
  if (!/not in tab group/.test(why[b.tabId] || "")) fails.push("B's tab: " + why[b.tabId]);
  if (!/does not exist/.test(why[99999] || "")) fails.push("missing tab: " + why[99999]);
  if (!tabs.has(1) || !tabs.has(kept.tabId) || !tabs.has(b.tabId)) fails.push("a refused tab was closed");
  if (tabs.has(a.tabId) || tabs.has(link.id)) fails.push("an allowed tab is still open");
  if ((session.openedTabs || []).includes(a.tabId) || (session.keptTabs || []).includes(a.tabId)) fails.push("closed tab still recorded");

  // A tab the user never shared is refused even by tabs_close with a list.
  tabs.get(1).groupId = -1;
  const outside = await call("tabs_close", { tabIds: [1], cleanup: true });
  if (outside.closed.length || !/not a gocode tab/.test(outside.refused[0].error) || !tabs.has(1)) fails.push("outside tab: " + JSON.stringify(outside));
  // Another session's group is refused.
  const other = await call("tabs_close", { tabIds: [b.tabId], groupId: tabs.get(b.tabId).groupId, sessionId: "A", cleanup: true });
  if (other.closed.length || !/another gocode session/.test(other.refused[0].error)) fails.push("other session: " + JSON.stringify(other));

  // Without cleanup the model may still close a kept tab it opened itself.
  await call("tabs_close", { tabId: kept.tabId, groupId: g, sessionId: "A" });
  if (tabs.has(kept.tabId)) fails.push("model close of a kept tab refused");
  // tabs_create with keep_open:false records nothing.
  const plain = await call("tabs_create", { sessionId: "A", keep_open: false });
  if ((session.keptTabs || []).includes(plain.tabId)) fails.push("keep_open:false stored a mark");
  finish();
})().catch((e) => { console.error("harness error: " + e.stack); process.exit(2); });
`

func TestExtensionCleanupOwnership(t *testing.T) {
	runExtensionHarness(t, cleanupHarness)
}
