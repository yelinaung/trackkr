# Phase 12: Browser Idle From the Daemon

## Context

The browser extension counts time the user was not there. One sway
session produced this:

```text
producer  duration  span          title
firefox     2453s   12:45-13:26   Timeline · trackkr
firefox       40s   12:45-12:45   Timeline · trackkr
desktop      229s                 firefox-beta      <- longest desktop record
```

The user stopped at 12:48. The daemon noticed at 12:53 and closed its
segment at 12:48, backdated to the last moment of activity. The
extension held the same tab in one record until 13:26, closing it some
43 minutes after the user left.

Every desktop record in that database stays under the 300s idle
threshold, because an idle transition cuts them. The extension's
next-longest record is 40 seconds. Nothing stopped the 2453-second one.

The extension's own logic is sound. `IDLE_SECONDS` is 300, matching the
daemon. It listens on `idle.onStateChanged`, backdates through
`idleEndsAt`, and refuses to seed a segment on window focus alone,
carrying a comment that explains why focus is not presence.

What it rests on fails. `browser.idle` on Linux reads the X screensaver
extension's counter. XWayland maintains that counter from the events
XWayland itself receives, so native Wayland input never touches it, and
the state change fires late or never.

Phase 10 removed that failure from the daemon, where `xprintidle` reads
the same counter. It survives in the extension, and costs more there:
the daemon stops at 12:48 while the extension runs to 13:26, so the
dashboard counts one span of wall-clock minutes twice, once as absence
and once as activity.

## Decisions

### The daemon is the source of idle, not the browser

The extension already reports to a daemon that knows the answer. That
daemon runs an idle detector chosen for the session it is in: swayidle
on Wayland, xprintidle on X11, CoreGraphics on macOS. Each of them sees
input the browser cannot.

The extension has good presence signals of its own. Tab activation,
navigation, and window focus all prove the user *is* there. None of them
prove the user left. `browser.idle` was supplying that one missing fact
and supplying it wrongly, and the daemon holds it already.

The change costs nothing. An extension with no daemon to ask has nowhere
to send records either, so a daemon is present whenever this matters.

### The daemon reports a timestamp, not a boolean

`GET /extension/idle` answers with the moment activity stopped:

```json
{"idle": true, "idle_since": "2026-08-02T04:48:02Z", "threshold_s": 300}
```

A boolean would tie the recorded end time to how soon the extension
asked. A timestamp lets it close the segment at 12:48:02 whenever it
learns, so polling granularity governs how quickly the record gets
written and never what the record says. The polling interval below is
therefore a free choice, not an accuracy tradeoff.

`idle_since` comes from the `IdleTime` the tracker already uses, so both
producers derive their boundaries from one clock and one detector. Today
they disagree by 38 minutes.

`IdleTime` returns an error alongside its duration, and `xprintidle` can
start failing long after startup. Reporting that as `idle: false` would
be the worst answer available: the extension would hold its segment open
for as long as the detector stayed broken, which is the failure this
phase removes. A detector error answers 503, and the extension treats
any non-2xx as no answer.

### The daemon needs an idle detector it does not have yet

`cmd/trackkrd/main.go` builds the idle detector inside the branch that
runs only when window detection succeeded, because nothing else read it
until now. A second reader changes that.

Phase 10 introduced the combination that breaks. An unsupported Wayland
compositor, or sway IPC failing, yields no window detector while the
daemon keeps running on the strength of the extension alone. It then
serves `/extension/idle` with nothing behind it.

So the detector moves out of that branch. One gets constructed
unconditionally, the tracker and the extension server share it, and
`closerFor` closes it once. It owns a swayidle child, so two owners must
not become two `Close` calls or two processes.

### Polling on an alarm, not a timer

Chrome evicts an MV3 service worker after roughly 30 seconds idle, and
`setInterval` dies with it. Firefox event pages suspend too. Only
`chrome.alarms` survives to wake the worker for a poll.

Alarms fire at most every 30 seconds and may be delayed an arbitrary
amount beyond that. Neither bound matters, because the daemon returns a
timestamp: a poll at 12:54, or one Chrome defers to 13:26, still closes
the segment at 12:48:02. Delay costs latency in writing the record and
never accuracy in what it holds.

Chrome promises no alarm survives a worker restart before Chrome 150, so
the worker checks for its alarm on every startup and recreates a missing
one, as the alarms documentation recommends. An extension that silently
lost its alarm would behave exactly like the bug this phase removes.

The poll runs only while a segment is open. A browser timing nothing has
nothing to close, and waking a service worker to learn that would spend
battery for no result.

### Which source wins, precisely

"Prefer the daemon" is not an implementable rule. The existing
`idle.onStateChanged` handler calls `finalize()` the moment it fires,
and no later daemon reply can withdraw a record written that way. Let
two sources end a segment and whichever speaks first decides, which is
how the extension came to trust the one that was wrong.

One source holds authority at a time, kept in `storage.session` so it
survives service-worker eviction:

```text
daemon   the default, and the state after any usable reply
browser  entered on an unusable reply, left on the next usable one
```

A reply is unusable when the request fails, the status is not 2xx, the
body does not parse, or `idle_since` lies in the future.

Under the `daemon` source, a `browser.idle` transition to `idle`
finalizes nothing by itself. It queries the daemon and acts on that
answer: end the segment at `idle_since` when the daemon agrees, do
nothing when it disagrees, because on Wayland the browser's own notion
of idle is the unreliable one. Only an unusable query drops the handler
through to today's `idleEndsAt` behaviour.

`locked` keeps its authority and its current semantics under either
source. A lock is an explicit act, so it ends the segment at the moment
it arrives with no backdating, for the reason the existing comment
gives: backdating a lock would end a young segment before it began. The
daemon has nothing better to say about a deliberate act, and asking it
would only delay a correct finalize.

Every path runs inside the existing `serialize()`, so an alarm firing
and an idle event cannot interleave and finalize one segment twice.

### Failure keeps the old behaviour, and says so

A daemon that cannot be reached, or one too old to serve the route,
leaves the extension on `browser.idle`. That degrades Wayland and stays
correct on X11, which is where an unreachable daemon is most likely to
be an old one.

The options page already classifies daemon connectivity for its status
display, and gains one line for this state. A user on Wayland can then
see that the overcounting mechanism is measuring their browser time,
instead of discovering it in a 41-minute record.

### Not capping segments at the threshold

The cheap fix refuses any browser segment longer than `IDLE_SECONDS`
without an intervening activity event. It needs no protocol and no
daemon.

It also destroys the records that are correct. Reading a long article,
watching a video, and sitting in a call are all real activity with no
tab events, and the daemon's idle inhibitor handling already treats a
full-screen video as non-idle. Capping would cut every one of them at
five minutes and call that accuracy.

## New Files

```text
internal/tracker/extension_idle_test.go  # the route, its auth, its shape
extension/tests/idle.test.js             # pure helpers for the poll
```

## Changed Files

```text
cmd/trackkrd/main.go                 # one idle detector, two readers
cmd/trackkrd/main_test.go            # built without a window detector
internal/tracker/extension.go        # GET /extension/idle
internal/tracker/extension_test.go   # route coverage
extension/background-core.js         # alarm, poll, source arbitration
extension/logic.js                   # pure: parse the reply, pick the end
extension/manifest.json              # alarms permission
extension/manifest.chrome.json       # alarms permission
extension/options.js                 # surface the fallback state
extension/tests/background.test.js   # alarm lifecycle, arbitration
extension/tests/chrome.test.js       # worker restart restores the alarm
docs/plan.md                         # how browser idle is decided
```

## Steps

1. `cmd/trackkrd/main.go`: construct the idle detector once, outside the
   `window != nil` branch, and hand it to the tracker and the extension
   server. Close it once through `closerFor`.

2. `extension.go`: add `GET /extension/idle` beside the existing status
   route, behind the same `authorized` check, answering 503 on a
   detector error. The server reads a small interface that answers "idle
   since when", defined in the consuming package as the repo does
   elsewhere.

3. `logic.js`: the pure part, which is what can be tested without a
   browser. Parse the reply, reject a malformed or future `idle_since`,
   and return the moment to close at, or null to keep the segment open.

4. `background-core.js`: the alarm and the arbitration above. Create the
   alarm when a segment opens, clear it when none is open, recreate it
   on worker startup when missing, and ignore alarms that are not ours
   by name. Route the alarm and the `idle.onStateChanged` handler
   through `serialize()`, and keep the source in `storage.session`.

5. Manifests: add `alarms` to each. The Chrome build takes it through
   `manifest.chrome.json`, which `mise run ext-build-chrome` stages.

6. `options.js`: report which idle source is in use, so the degraded
   state stays visible instead of silent.

7. `docs/plan.md`: state that browser idle comes from the daemon, with
   the browser API as fallback.

## Tests

The Go side is ordinary handler testing against `httptest`: the route
answers a GET, rejects other methods, requires the token, and returns
`idle_since` only when idle. A fake idle detector supplies the state, as
`internal/tracker` already does for `IdleDetector`. One such fake
returns an error, asserting 503 instead of a cheerful `idle: false`.

`main_test.go` covers wiring that does not exist today: a daemon built
with a failing window detector and the extension enabled still has an
idle detector for the route to read, and still closes it once.

Pure helpers in `logic.js`, run by `mise run ext-test` with no browser:

- a reply with `idle: false` keeps the segment open
- a reply with `idle: true` returns its `idle_since`
- an `idle_since` after now is rejected, never producing a segment that
  ends before it started
- a malformed body, a 401, a 404, and a 503 all fall back without
  throwing, where 404 means a daemon too old for the route

The bug appears in miniature in one case: a segment opened at 12:45 and
a reply of `idle_since = 12:48` gives 12:48 whether the poll happened at
12:54 or at 13:26. A boolean-shaped reply cannot express that, and a
test that polls immediately would pass either way.

Behaviour, in the existing `background.test.js` and `chrome.test.js`
harnesses, since nothing above exercises the lifecycle:

- opening a segment creates the alarm; finalizing clears it
- an alarm event carrying someone else's name is ignored
- a worker that starts with no alarm and an open segment recreates it,
  which is the state Chrome leaves behind before 150
- under the `daemon` source, a `browser.idle` idle event queries the
  daemon and does not finalize when the daemon disagrees
- under the `browser` source, the same event finalizes as it does today
- an unusable reply moves the source to `browser`, and the next usable
  one moves it back
- `locked` finalizes at once under either source, without backdating
- an alarm firing and an idle event arriving together produce one
  finalize, not two

## Out of Scope

The desktop daemon keeps what Phase 10 gave it, a correct detector on
every platform, and the extension now defers to it.

Reconciling the two producers on the dashboard waits for its own phase.
Once the extension stops overcounting, desktop and browser records for
the same minutes still coexist by design, and how the dashboard presents
that is a separate question from whether the numbers underneath are
true.

Title churn stays where it is. Unread counters in window titles fragment
desktop records into hundreds of two-second segments, which is real and
unrelated: it lives in `tracker.go`'s title comparison, not in idle
handling.

## Manual Verification

With `mise run dev` running and both extensions loaded, open a tab and
leave the machine for longer than the threshold.

The check is where the segment *ends*, not how long it runs. A record
longer than the threshold is no failure: reading, a video, and a call
are all real activity with no tab events, and this phase keeps those
records on purpose. Comparing a maximum duration against the threshold
would fail every one of them.

What must hold is that the browser's last record before the gap ends at
the same instant the desktop's does, and that neither reaches into the
gap:

```sh
docker exec <db> psql -U trackkr -d trackkr -tAc \
  "select producer, max(ended_at) from activity_records
   where ended_at < <the moment you returned> group by producer;"
```

Both producers should land within seconds of each other, deriving their
boundary from one detector. On the run that prompted this phase they
were 38 minutes apart.

Then read the gap itself: no record of any producer may overlap the
period you were away. The 2453-second record would have failed that
assertion, and a duration comparison would have missed it had the user
spent the time watching something.

Repeat on X11 with the daemon stopped to confirm the fallback still
works, and check that the options page reports which source is in use.
