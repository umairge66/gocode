# gocode browser control (MV3 extension)

A **local-only** Chrome extension that lets the `gocode` CLI drive your browser
through a native-messaging host running on *your* machine.

- **No account, no sign-in, no OAuth.** It never talks to any server.
- **No network egress, no telemetry.** The extension makes no network
  requests (its only `fetch` reads a `data:` URL of a screenshot it just
  took). The only thing it talks to is the local native host, over Chrome's
  stdio pipe.
- **Security without a port or token.** Chrome spawns the native host process
  locally and only lets an extension whose id is listed in the host
  manifest's `allowed_origins` connect to it. That origin check is the entire
  trust boundary: no TCP port is opened and no shared secret is exchanged.

## Files

| File | Purpose |
| --- | --- |
| `manifest.json`, `background.js`, `icons/*.png` | The extension itself (the runtime files); the icons show Prompty, gocode's mascot. |
| `icons/src/` | SVG sources of the icons (not packaged). |
| `embed.go` | Embeds the runtime files into the gocode binary (`extension.RuntimeFiles`). |
| `store/` | Chrome Web Store listing text, privacy policy and publishing checklist. |
| `README.md` | This file. |

Only the runtime files are embedded, written by `gocode chrome install` and put
into the store zip. A new runtime file (an icon, another script) must be added
to the `//go:embed` line and `RuntimeFiles` in `embed.go`; a test fails if a
file in this directory is neither embedded nor known documentation.

## Installing

### From the gocode binary (now)

```
gocode chrome install
```

writes the embedded extension to `<gocode data dir>/chrome-extension/`
(`~/.config/gocode/chrome-extension` unless `XDG_CONFIG_HOME` or
`GOCODE_CONFIG_DIR` says otherwise), replacing an older copy in one step,
owner-only permissions. It also writes the native-messaging host manifest
`com.gocode.gocode.json` for your OS and browser (`--browser`), pointing at the
absolute path of the `gocode` binary. On Windows the manifest goes to
`%LOCALAPPDATA%\gocode\NativeMessagingHosts` and is registered under each
Chromium-family browser's
`HKCU\Software\...\NativeMessagingHosts\com.gocode.gocode` key (no admin
rights needed; if a key cannot be written the equivalent `reg add` command is
printed).

Then load it once:

1. Open `chrome://extensions` and turn on **Developer mode** (top right).
2. Click **Load unpacked** and choose the folder `gocode chrome install`
   printed.
3. Restart Chrome so it picks up the host manifest.

After updating gocode, run `gocode chrome install` again (it rewrites the
folder with the new version) and click **Reload** on "gocode browser control"
in `chrome://extensions`. `gocode chrome status` compares the written version
with the one built into gocode and warns when it is outdated; during a
session gocode also says once when the connected extension's version differs
from its own. `gocode chrome uninstall` removes the host manifest, the
registry entries and the written folder (remove the extension in
`chrome://extensions` yourself).

### From the Chrome Web Store (later)

Once the extension is published, install "gocode browser control" from the
store instead of loading it unpacked; Chrome then keeps it up to date. You
still run `gocode chrome install` for the native-host manifest. Publishing is
described in `store/checklist.md`.

## Extension ids and trust

Chrome derives an unpacked extension's id from the `"key"` field in
`manifest.json`: the base64 DER of an RSA public key. Because that key is
committed, the unpacked extension always gets the same id on every machine:

```
enlbkekddkndgijidajppabineoekbfh
```

The key is a *public* key; it is safe to commit and publish. No private key is
needed to develop, load or publish the extension: unpacked loading needs only
the public key, and the Chrome Web Store signs published packages with its own
key and assigns its own id (the upload zip therefore has the `"key"` removed).

`gocode chrome install` puts these ids into the host manifest's
`allowed_origins`:

- the unpacked id, computed from the manifest key (or `--extension-id`);
- the Chrome Web Store id, once `StoreExtensionID` in
  `internal/chromehost/embedded.go` is filled in after publishing;
- any ids in the `GOCODE_CHROME_EXTENSION_IDS` environment variable
  (comma-separated), for an extension you built and signed yourself.

`gocode chrome status` lists the trusted ids.

## Developing

1. Edit `background.js` / `manifest.json` here. Check syntax with
   `node --check extension/background.js`.
2. Point the host at this folder instead of the embedded copy:

   ```
   gocode chrome install --extension-dir ./extension
   ```

   and load this folder unpacked. It has the same id as the embedded copy, so
   use one or the other, not both at once.
3. Click **Reload** in `chrome://extensions` after each change.
4. Bump `"version"` in `manifest.json` for every change you ship: the version
   is how gocode tells the user that their loaded extension is outdated.

Rebuilding gocode embeds the edited files. To build the Chrome Web Store
upload zip:

```
scripts/package-extension.sh            # -> dist/gocode-chrome-extension-<version>.zip
```

The zip holds only the runtime files, without the `"key"`, and is
reproducible (same files, same sha256). It uses only the local Go toolchain.

## Protocol

On launch the service worker calls
`chrome.runtime.connectNative("com.gocode.gocode")`, which starts the `gocode`
native host; the extension sends a `hello` with its name and manifest version
and then answers the commands the host sends (`ping`, `navigate`, `read_page`
/ `get_page_text`, `tabs_context`, `tabs_create`, `tabs_close`, `screenshot`,
`computer`, `find`, `form_input`, `browser_batch`, `javascript_tool`,
`resize_window`). Interaction commands attach `chrome.debugger` to the target
tab only while they run, one command per tab at a time. If the host
disconnects, the worker stops its commands and reconnects with backoff; a
reply for a command from a host that has gone is dropped.

Every CDP call and page script has a timeout, so a hung page fails the call
instead of wedging the tab. A JavaScript dialog that opens while gocode holds
the debugger is dismissed (Cancel for `confirm()`) and reported in the result.
When gocode stops waiting for a command (the call was cancelled or timed out
after 120 seconds), the host sends `{"method":"cancel","params":{"id":…}}` and
the extension aborts what is left of it — the remaining steps of a batch — and
detaches.

`find` tags the elements it returns with refs (`ref_<n>`) that are unique per
tab; they live in the extension's isolated world, not in the page's DOM, and a
ref from before a navigation or reload is reported as stale.

## Background tabs

gocode opens its tabs in the background, and the window may be minimized or
covered, so the page gocode drives is usually hidden
(`document.visibilityState` is `"hidden"`). Chrome does not deliver input to a
hidden page: tested on Chrome 156 through `chrome.debugger`, a CDP mouse press
or key press is accepted and reported as done, but the page gets no
`pointerdown`, `click` or `keydown` (`Input.insertText` does arrive), and
`Page.captureScreenshot` answers late or not at all because the page renders
no frames. Up to 0.4.0 this meant a click that "worked" but did nothing, and
screenshots that timed out after 20 seconds.

From 0.4.1 the extension handles it like this, least intrusive first:

1. **Focus emulation.** While gocode holds the debugger on a hidden (or
   unfocused) tab, it turns on `Emulation.setFocusEmulationEnabled` — what
   Playwright does for its pages. Chrome then treats the page as visible and
   focused (as if it were being captured): it renders, hit-tests, and takes
   clicks, keys and drags as real, trusted input, while the tab stays in the
   background and the window is not raised. Detaching the debugger ends it.
   Tested: hidden-tab clicks (40 of 40), keys and text arrive as trusted
   events and the user's tab stays in front; a minimized window behaves the
   same.
2. **Screenshots** of a hidden tab are taken with a screencast running
   (`Page.startScreencast` keeps frames coming, which `captureScreenshot`
   waits for; tested at 130–400 ms), falling back to the last screencast
   frame. When Chrome renders nothing at all the screenshot fails after 8
   seconds saying why, instead of hanging.
3. **Every input action is checked.** Before dispatching, the extension adds
   capture-phase listeners in every frame of the page; afterwards it looks for
   the trusted events the action must cause (a `pointerdown`/`mousedown`/
   `click`, a `keydown`, an `input` event — a release alone does not count).
   If none arrived, it does the same action with DOM events from inside the
   page at the element under the point (through open shadow roots and
   same-origin iframes), with the right `clientX/Y`, `button`, `buttons`,
   `detail` and modifier keys, focusing what is clicked; typed text, Enter
   (submits the form), Backspace/Delete and select-all are carried out too,
   and the page gets transient user activation first, so a popup or
   `target=_blank` link the click opens is not blocked. These events are
   untrusted (`event.isTrusted` is false), and the result says so. When even
   that is impossible (the point is in another site's frame, or `type` with
   no text field focused) the action fails with the reason. A page where
   extension scripts cannot run is reported as "sent, not confirmed". An
   action is never reported as done when the page did not get it.
4. **Scrolling** a hidden tab is done in the page from the start (an
   untrusted `wheel` event, then scrolling the element under the point): in
   testing, a CDP mouse wheel on a background tab now and then made Chrome
   switch the window's tabs, as a wheel over the tab strip does on Linux.

gocode never brings its tab to the front (`chrome.tabs.update` with
`active: true`) to get input through: that would take the user's view of their
window away, and focus emulation works without it. A link a gocode click
follows can still open its tab in front (`target=_blank` does); when the gocode
tab was in the background, the extension shows the user's tab again, and the
new tab (which Chrome puts in the gocode group, while naming the user's tab as
its opener) is gocode's.

## Tab groups per session

Each gocode session sends its session id with every command, and the
extension keeps one green "gocode" tab group per session (per window).
`tabs_context` lists only the calling session's group, and `tabs_close` can be
pinned to a group (`groupId`), so a tab that has left that group, or a group
that belongs to another session, is refused.

When a session ends — you exit gocode (interactive or the end of a `-p` run),
switch to another session (`/resume`, or `--resume`/`--continue` moving away
from it), or `/clear` it — gocode closes the tabs it opened in that session's
group. It never closes:

- a tab **you** dragged into the gocode group (the extension records which
  tabs gocode opened itself — with `tabs_create`, `navigate`, or a link
  gocode's own click followed from a gocode tab — and lists that as
  `openedByGocode`); a tab you open from a gocode tab yourself (a middle- or
  ctrl-click) is not gocode's at all;
- a tab the model marked `keep_open` (an optional flag on `navigate` and
  `tabs_create`, used when you asked to see a page or keep it open; listed as
  `keepOpen`).

When only such tabs remain, the group stays as it is. The extension enforces
this itself: an end-of-session close (`tabs_close` with `cleanup: true`)
refuses any tab gocode did not open and any kept tab. While a background agent
is still running, `/clear` and a session switch close the group only if it is
still empty, since the agent may still be using it. At most 50 tabs are closed
automatically; a group with more is left open with one notice. Closing at exit
takes at most 1.5 seconds and never holds up exit. The model is also asked to
close the tabs it no longer needs before it finishes.

Version 0.3.0 or later of the extension is needed for this: an older
extension does not report which tabs gocode opened, so nothing is closed at
the end of a session.

## Per-site rules

The browser tools honour `gocodeInChromeDomain(<host>)` permission rules from
the gocode settings, e.g.

```json
{ "permissions": {
    "allow": ["gocodeInChromeDomain(github.com)"],
    "deny":  ["gocodeInChromeDomain(bank.example)"] } }
```

A denied site is refused for every tool. In a gocode session, navigating to or
acting on (clicking, typing, filling forms on) a site with no allow rule asks
first; "don't ask again" saves the allow rule. The host sends the current lists
with every command and the extension enforces them as well, so the lists only
ever restrict what the tools can do. They never widen the tab-group isolation:
gocode still only sees its own "gocode" tab group.
