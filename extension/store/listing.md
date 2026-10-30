# Chrome Web Store listing: gocode browser control

Text to paste into the Chrome Web Store Developer Dashboard. Everything here
describes the extension as it is in `../manifest.json` and `../background.js`;
re-check it whenever those change.

## Store listing tab

**Name** (from the manifest): gocode browser control

**Short description** (summary, max 132 characters; this text is 125, the same as the manifest `"description"`):

> Lets the gocode command-line coding agent on your computer control Chrome
> tabs locally. No account, no servers, no telemetry.

Paste as one line:

```
Lets the gocode command-line coding agent on your computer control Chrome tabs locally. No account, no servers, no telemetry.
```

**Category:** Developer Tools

**Language:** English

**Detailed description:**

```
gocode browser control connects Chrome to gocode, an open-source coding agent that runs in your terminal. With it, gocode can open pages, read their text, take screenshots, click, type and fill in forms, so it can check the web app you are building, look things up in documentation, or reproduce a bug in the browser.

How it works
- The extension talks only to the gocode program on your own computer, through Chrome's native messaging. It makes no network requests of its own and has no server, account or sign-in.
- gocode works only in its own green "gocode" tab group. It cannot see or touch your other tabs unless you drag a tab into that group.
- Per-site rules from your gocode settings decide which sites gocode may use; in an interactive session gocode asks before acting on a site you have not allowed.
- When gocode clicks or types, Chrome shows its "started debugging this browser" bar for that tab, so you can see when gocode is in control. The debugger is detached as soon as the action ends.
- No analytics, no telemetry, no data collection.

Setup
1. Install gocode and run: gocode chrome install
   (this registers gocode as the extension's native-messaging host).
2. Install this extension and restart Chrome.

The extension does nothing without the gocode program installed on the same computer.
```

**Single purpose description:**

```
Lets the gocode command-line coding agent, running on the user's own computer, control browser tabs in its own tab group (open pages, read page text, take screenshots, click, type and fill forms) through Chrome native messaging.
```

**Homepage URL / support URL:** the gocode GitHub repository (and its issues
page).

## Privacy practices tab

### Permission justifications

`nativeMessaging`
: The extension's only connection is to the gocode program on the user's
computer, through `chrome.runtime.connectNative("com.gocode.gocode")`. gocode
sends commands (open a page, read text, click) and the extension returns the
results over this local pipe.

`tabs`
: To open, list, navigate, activate and close the tabs gocode works in, and to
read their URL and title so gocode knows which page it is on and so per-site
rules can be applied.

`tabGroups`
: gocode works only in its own "gocode" tab group, one per gocode session. The
extension creates and names that group and checks group membership to keep
gocode away from the user's other tabs.

`scripting`
: To read a page's visible text, find elements and fill in form fields in a
gocode tab. Only functions packaged with the extension are injected (via
`chrome.scripting.executeScript` with `func`); no code strings are injected.

`alarms`
: A periodic alarm wakes the service worker so it reconnects to the local
gocode program after Chrome has suspended the worker.

`storage`
: `chrome.storage.session` keeps the extension's own bookkeeping (which tab
groups and tabs belong to which gocode session, which tabs gocode opened, the
current per-site rules) across service-worker restarts. It is cleared when
the browser closes. Nothing is stored in `local` or `sync` storage.

`debugger`
: Clicking, typing, scrolling, key presses and screenshots are sent to the
gocode tab through the Chrome DevTools Protocol, because synthetic DOM events
are not treated as real user input by many pages. The debugger is attached to
one gocode tab only while an action runs and is always detached afterwards, so
Chrome's debugging bar is shown only during active control. gocode's
`javascript_tool` command also evaluates a JavaScript expression in a gocode
tab through the protocol (`Runtime.evaluate`), at the request of the local
gocode program.

Host permission `<all_urls>`
: gocode may be asked to open and work with any website the user is
developing or researching (including `localhost` development servers), so the
extension needs access to the pages in its tab group whatever their address.
Access is further limited to gocode's own tab group and to the per-site
allow/deny rules in the user's gocode settings.

### Remote code

Answer: **No, I am not using remote code.**

All of the extension's JavaScript is in the package (`background.js`); it
loads no external scripts, uses no `eval`/`new Function` in the extension and
injects only packaged functions with `chrome.scripting`.

Be aware when filling this in: the `javascript_tool` command passes a
JavaScript expression from the local gocode program to the page through
`chrome.debugger` (`Runtime.evaluate`), similar to typing it into DevTools.
That code runs in the web page, not in the extension, and comes from the
user's own computer, not from a server. If a reviewer asks, explain this; if
the review rejects it under the remote-code policy, the fallback is to remove
`javascript_tool` from the store build.

### Data usage

The extension itself collects no user data and sends nothing to the developer
or any server. When gocode asks for it, the extension passes page content
(URLs, titles, page text, screenshots, form values gocode typed) to the gocode
program on the same computer; it does not transmit it anywhere else.

In the dashboard's data-collection checklist, tick nothing (the developer does
not collect or receive any of these categories), and certify:

- I do not sell or transfer user data to third parties, outside of the
  approved use cases.
- I do not use or transfer user data for purposes that are unrelated to my
  item's single purpose.
- I do not use or transfer user data to determine creditworthiness or for
  lending purposes.

**Privacy policy URL:** publish `privacy-policy.md` (for example in the
GitHub repository) and paste its URL.
