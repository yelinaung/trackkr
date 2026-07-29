# trackkr browser extension

The extension reports the active Firefox tab to a `trackkrd` daemon on
the same machine. The daemon stamps each record as `Firefox` and feeds
it through the queue that already carries desktop window activity, so
the server sees ordinary records and needs no changes.

## Running it

```text
mise dev                      # database, server, device, daemon, config
```

Load the extension from `about:debugging` -> This Firefox -> Load
Temporary Add-on, and pick `manifest.json` here. Open the options page,
paste the token `mise dev` printed, and save. Firefox then asks for the
host permission. Accept it, or every request fails.

```text
mise ext-lint                 # web-ext lint, warnings are errors
mise ext-test                 # logic and background tests, no browser
```

## Ignoring sites

The options page takes one host per line, and those hosts are never
recorded. A host covers its subdomains: `gov.sg` ignores
`login.id.singpass.gov.sg`, because listing every host a bank redirects
through defeats the purpose. Blank lines and `#` comments disappear, a
leading `*.` or `www.` falls away, and case never matters.

Rules and page addresses go through the same URL parser. Write
`bücher.de` and the extension stores `xn--bcher-kva.de`, which is what
the browser reports for that host: write the Unicode, match the
punycode. Pasted URLs work too, and `https://bank.example`,
`bank.example:443`, and `bank.example/private` all reduce to
`bank.example`. A line that cannot be a hostname comes back as an
error, so no dead rule sits in storage matching nothing.

The check runs in the browser, before anything reaches storage or the
queue. An ignored page therefore never leaves Firefox, and cannot
surface in the daemon's logs, the database, or the dashboard. A
daemon-side filter would receive the URL first and delete it afterwards.

Adding a rule reaches backwards as well as forwards: it discards the
segment being timed at that moment, purges matching records from the
unsent queue, and filters the queue again just before delivery. Three
guards cover three gaps -- a page open when the rule lands, a record
queued while the daemon was down, and an event page that restarted
without ever seeing the change notification.

## Manifest decisions

JSON has no comments, and Firefox warns about every key it does not
recognise, so the reasoning sits here.

**`strict_min_version: "142.0"`** follows what the manifest keys
require. `optional_host_permissions` arrived in Firefox 128 and
`data_collection_permissions` in 142. A lower floor lets the extension
install on a browser where the runtime permission request cannot work,
and that failure looks exactly like an unreachable daemon. `web-ext
lint` caught the original `115.0`.

**`data_collection_permissions`** marks `browsingActivity` and
`websiteContent` required, not optional. Recording which pages were open
and for how long is the whole point of the extension, and a page title
is content read off the page.

**`optional_host_permissions`** covers `127.0.0.1`, `localhost`, and
`[::1]`, because the daemon accepts any loopback bind. Declare one
address and a config the daemon considers valid leaves the extension
unable to ask for access. A test checks that the manifest declares
whatever `originFor` derives for each of those addresses.

The origin stays optional instead of a fixed `host_permissions` entry
for two reasons. Firefox treats MV3 host permissions as optional and
prompts at runtime anyway. And `extension_addr` is configurable, so a
pinned `http://127.0.0.1:7600/*` breaks the moment someone changes the
port -- silently, because a blocked request looks like a dead daemon.

## Layout

`logic.js` holds the decision rules: which tabs count, when a segment
ends, what gets sent. It touches no browser API, so `node:test` covers
it directly. `background.js` does the plumbing -- storage, events,
delivery. `common.js` keeps the few helpers that need the browser.

One promise chain serialises every handler. Browser events arrive
together, a navigation firing a URL update and a title update back to
back, and each handler reads, modifies, and writes the same two storage
keys. Run two at once and they finalize the same segment twice or
overwrite each other's queue.

The two pieces of state live in different stores for different reasons.
The in-flight tab sits in `storage.session`, which survives event-page
suspension but not a browser restart, and yesterday's focus is stale.
The unsent queue sits in `storage.local`, because session storage
empties exactly when the queue matters most: the daemon unreachable
while Firefox quits.

## Testing

`tests/logic.test.js` covers the pure rules. `tests/background.test.js`
loads `logic.js`, `common.js`, and `background.js` into a `vm` context
with a fake browser (`tests/harness.js`), then drives real events,
including a delivery held open mid-request to force two handlers to
overlap.

Playwright cannot load Firefox extensions; extension loading is
Chromium-only. A fake browser is the ceiling for automated coverage
here, and anything past it needs Selenium's `install_addon` or a person
with `about:debugging` open.
