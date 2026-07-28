# trackkr browser extension

Reports the active Firefox tab to a `trackkrd` daemon on the same
machine. The daemon enriches each record and feeds it through the queue
it already uses for desktop window activity, so nothing about the server
changes.

## Running it

```
mise dev                      # database, server, device, daemon, config
```

Then load the extension: `about:debugging` -> This Firefox -> Load
Temporary Add-on -> pick `manifest.json` in this directory. Open its
options, paste the token `mise dev` printed, and save. Saving triggers
the host permission prompt, which must be accepted.

```
mise ext-lint                 # web-ext lint, warnings are errors
mise ext-test                 # logic and background tests, no browser
```

## Ignoring sites

The options page takes a list of hosts that must never be recorded, one
per line. A host covers its subdomains, so `gov.sg` also ignores
`login.id.singpass.gov.sg` -- listing every host a bank redirects
through would be unusable. Blank lines and `#` comments are dropped, a
leading `*.` or `www.` is stripped, and matching is case-insensitive.

The check runs in the browser, before anything is stored or queued.
That placement is the point: an ignored page never reaches the daemon,
so it cannot appear in the daemon's logs, the database, or the
dashboard. Filtering on the daemon instead would mean the URL had
already left the browser.

## Manifest decisions

JSON has no comments and Firefox warns about any key it does not
recognise, so the reasoning lives here.

**`strict_min_version: "142.0"`** is set by what the manifest keys
actually require, not by guesswork: `optional_host_permissions` landed
in Firefox 128 and `data_collection_permissions` in 142. A lower floor
would let the extension install on a browser where the runtime
permission request cannot work, which presents as the daemon being
unreachable. `web-ext lint` checks this and was what caught the original
`115.0`.

**`data_collection_permissions`** declares `browsingActivity` and
`websiteContent` as *required* rather than optional. Recording which
pages were open and for how long is the entire purpose of the extension,
and page titles are content read off the page.

**`optional_host_permissions`** covers `127.0.0.1`, `localhost`, and
`[::1]` because the daemon accepts any loopback bind. Declaring only one
would mean a daemon config it considers valid leaves the extension
permanently unable to request access. A test asserts these cover
whatever `originFor` derives for each of those addresses.

The origin is optional rather than a fixed `host_permissions` entry for
two reasons: Firefox treats MV3 host permissions as optional anyway and
prompts at runtime, and the daemon port is configurable, so a pinned
`http://127.0.0.1:7600/*` would break silently the moment someone
changed `extension_addr`.

## Layout

`logic.js` holds the decision rules -- which tabs count, when a segment
ends, what is sent -- with no browser APIs, so `node:test` can cover
them directly. `background.js` is the plumbing: storage, events, and
delivery. `common.js` holds the few helpers that need the browser.

Every handler runs through one promise chain. Browser events arrive
concurrently (a navigation fires a URL update and a title update back to
back) and each handler is a read-modify-write over the same two storage
keys, so without serialising them two handlers can finalize the same
segment twice or clobber each other's queue writes.

State is split deliberately. The in-flight tab lives in
`storage.session`, which survives event-page suspension but not a
browser restart -- a focus left over from a previous run is stale. The
unsent queue lives in `storage.local`, because session storage would
lose it exactly when it matters most: the daemon being unreachable while
the browser is quitting.

## Testing

`tests/logic.test.js` covers the pure rules. `tests/background.test.js`
loads `logic.js`, `common.js`, and `background.js` into a `vm` context
with a fake browser (`tests/harness.js`) and drives real events,
including holding a delivery open mid-request to force overlap.

That split exists because Playwright cannot load Firefox extensions --
extension loading is Chromium-only -- so a fake browser is the practical
ceiling for automated coverage. Anything beyond it needs Selenium's
`install_addon` or a person with `about:debugging` open.
