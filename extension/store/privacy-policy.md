# Privacy policy: gocode browser control

Applies to the Chrome extension "gocode browser control" and the gocode
command-line program it connects to.

## Summary

The extension collects no personal data, sends nothing to the developer and
makes no network requests. It exchanges messages only with the gocode program
installed on the same computer.

## What the extension does with data

- **Who it talks to.** The extension opens one connection: Chrome's native
  messaging pipe to the gocode program on your computer
  (`chrome.runtime.connectNative("com.gocode.gocode")`). Chrome starts that
  program locally and allows only this extension to connect to it. The
  extension does not use `XMLHttpRequest`, WebSockets or any server; its only
  `fetch` call reads a `data:` URL holding a screenshot it has just taken, which
  never leaves the browser through that call.
- **What it passes to gocode.** When gocode asks, the extension returns
  information about the tabs in gocode's own tab group: their address and
  title, the text of the page, screenshots, the result of a search for page
  elements, and the outcome of clicks, key presses and form input. It does
  not read tabs outside gocode's tab group unless you move a tab into it.
- **What it stores.** It keeps its own bookkeeping (which tabs and tab groups
  belong to which gocode session, which tabs gocode opened, the current
  per-site rules) in `chrome.storage.session`, which Chrome clears when the
  browser closes. It uses no cookies, no `localStorage` and no `sync`
  storage.
- **No analytics, no telemetry, no advertising, no tracking.**

## What gocode does with it

gocode is a coding agent that you run with an AI model provider you choose
and configure with your own API key. Page content the extension returns is
used in your gocode session the same way as the files gocode reads from your
project: it may be included in the requests gocode sends to that provider so
the model can work with it. Those requests go from your computer directly to
your provider under your account and that provider's privacy policy. The
developer of gocode and of this extension runs no server and receives none of
this data. gocode itself sends no telemetry.

## Sharing and sale

No user data is sold, shared with or transferred to the developer or any
third party by the extension.

## Your control

- gocode works only in its own "gocode" tab group; your other tabs stay out of
  its reach.
- Per-site allow/deny rules in your gocode settings limit which sites it may
  use, and an interactive session asks before acting on a site you have not
  allowed.
- Removing the extension in `chrome://extensions` and running
  `gocode chrome uninstall` removes everything it installed.

## Changes

Changes to this policy are published with the gocode source code, in
`extension/store/privacy-policy.md`.

## Contact

Open an issue in the gocode GitHub repository.
