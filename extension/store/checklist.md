# Publishing to the Chrome Web Store

Apart from the package icons, nothing here has been done yet: there is no
developer account and the extension has not been uploaded. Work through the steps in order.

## 1. Developer account (one time)

- [ ] Sign in at https://chrome.google.com/webstore/devconsole with the Google
      account that should own the extension (consider a dedicated account,
      and turn on 2-step verification, which the store requires).
- [ ] Pay the one-time developer registration fee (USD 5 at the time of
      writing) and accept the developer agreement.
- [ ] Fill in the publisher name and contact e-mail, and verify the e-mail.

## 2. Prepare the package

- [x] Add icons. `extension/icons/` holds `icon-16.png`, `icon-32.png`,
      `icon-48.png` and `icon-128.png` (the Prompty mascot; SVG sources in
      `extension/icons/src/`, which are not packaged). `manifest.json` lists
      them under `"icons"` and `"action"."default_icon"`, and they are in the
      `//go:embed` line and `RuntimeFiles` (`go test ./extension` checks the
      paths, sizes and that embed and disk agree).
- [ ] Bump `"version"` in `extension/manifest.json` (every upload needs a
      higher version than the last one).
- [ ] Check the manifest `"description"` is at most 132 characters
      (`go test ./extension` checks it).
- [ ] `node --check extension/background.js` and `go test ./...`.
- [ ] Build the upload zip:

      scripts/package-extension.sh

      It prints `dist/gocode-chrome-extension-<version>.zip` and its sha256
      (also in the `.sha256` file next to it). The zip contains only
      `manifest.json` (without the `"key"` field) and the runtime files
      (`background.js` and the PNG icons).
      Keep the sha256 with the release notes so the uploaded file can be
      matched to the source.

## 3. Create the item

- [ ] Developer Dashboard -> Items -> **New item** -> upload the zip.
- [ ] **Store listing** tab: paste the texts from `listing.md` (description,
      category Developer Tools, language English), upload the store icon
      (128x128 PNG) and the screenshots below. A small promo tile
      (440x280 PNG/JPEG) is optional but helps the listing.
- [ ] Screenshots: 1 to 5 images, 1280x800 (or 640x400) PNG/JPEG, no
      personal data visible. Suggested:
  1. A terminal running gocode next to Chrome with the green "gocode" tab
     group, gocode having opened a page (e.g. a local dev server).
  2. gocode reading a page: the terminal shows the page text summary while
     the page is visible in the gocode tab group.
  3. gocode filling in a form or clicking a button, with Chrome's "started
     debugging this browser" bar visible (shows the user can see control).
  4. `chrome://extensions` with "gocode browser control" loaded, and the
     Prompty icon in the toolbar with no red dot (connected).
  5. The terminal output of `gocode chrome install` / `gocode chrome status`.
- [ ] **Privacy practices** tab: single purpose, the permission
      justifications (one per permission and for the `<all_urls>` host
      permission) and the remote-code answer from `listing.md`; tick no
      data-collection categories; tick the three certifications; paste the
      privacy policy URL (publish `privacy-policy.md`, e.g. as a page in the
      GitHub repository).
- [ ] **Distribution** tab: Public (or Unlisted for a first trial), all
      regions, free.
- [ ] **Submit for review.** Expect review to take from days up to a few weeks;
      `debugger`, `<all_urls>` and `nativeMessaging` usually get a closer
      look. Answer reviewer questions with the justifications in
      `listing.md`.

## 4. After approval

- [ ] Copy the item id (32 letters a-p, shown in the dashboard and as the last
      part of the store URL
      `https://chromewebstore.google.com/detail/<name>/<id>`).
- [ ] Set it in `internal/chromehost/embedded.go`:

      const StoreExtensionID = "<id>"

      `go test ./internal/chromehost` checks the id's format.
- [ ] Release a new gocode version. From then on `gocode chrome install`
      allows both the store id and the unpacked id in the host manifest and
      mentions the store as an install option; `gocode chrome status` labels
      both ids. Users of an older gocode must run `gocode chrome install`
      once with the new version before the store extension can connect.
- [ ] Update the store link in `README.md` and `extension/README.md`.

## Later updates

1. Change the extension, bump `"version"`, rebuild gocode (it embeds the new
   files) and run `scripts/package-extension.sh`.
2. Dashboard -> the item -> **Package** -> upload the new zip -> submit.
3. Store users get the update automatically after review; users of the
   unpacked copy run `gocode chrome install` and click Reload.

The store id never changes, so `StoreExtensionID` is set only once. No private
key is involved at any point: the store signs packages itself.
