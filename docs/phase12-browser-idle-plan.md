# Phase 12: Browser Idle From the Daemon

## Context

The browser extension counts time the user was not there. On a sway
session it produced this:

```text
producer  duration  span          title
firefox     2453s   12:45-13:26   Timeline · trackkr
firefox       40s   12:45-12:45   Timeline · trackkr
desktop      229s                 firefox-beta      <- longest desktop record
```

The user stopped at 12:48. The daemon noticed at 12:53 and closed its
segment at 12:48, backdated to the last moment of activity. The
extension kept the same tab open in one record until 13:26, then closed
it about 43 minutes after the user actually left.

Every desktop record in that database stays under the 300s idle
threshold, because an idle transition cuts them. The extension's
next-longest record is 40 seconds. The 2453-second one is not a long
tail, it is a segment that nothing stopped.

The extension's own logic is not the problem. `IDLE_SECONDS` is 300,
matching the daemon. It listens on `idle.onStateChanged`, backdates
through `idleEndsAt`, and refuses to seed a segment on window focus
alone, with a comment explaining that focus is not presence. The design
is right.

What it rests on is wrong. `browser.idle` on Linux reads the X
screensaver extension's counter, which is the same mechanism that makes
`xprintidle` useless under Wayland and the reason Phase 10 exists.
XWayland maintains that counter from the events XWayland itself
receives, so native Wayland input never touches it. The state change
fires late or not at all.

So the bug Phase 10 fixed for the daemon is still present one layer up,
from the same root cause, and it costs more: the daemon stops at 12:48
while the extension runs to 13:26, and the dashboard counts the same
wall-clock minutes once as absence and once as activity.

## Decisions

### The daemon is the source of idle, not the browser

The extension already reports to a daemon that knows the answer. That
daemon runs a platform idle detector chosen for the session it is
actually in: swayidle on Wayland, xprintidle on X11, CoreGraphics on
macOS. Every one of them sees input the browser cannot.

The extension has good presence signals of its own -- tab activation,
navigation, window focus -- and they all prove the user *is* there. None
of them prove the user left. That single missing fact is what
`browser.idle` was supplying and supplying wrongly, and the daemon has
it already.

Nothing is lost by the change. An extension with no daemon to ask has
nowhere to send records either, so a daemon is always present when this
matters.

### The daemon reports a timestamp, not a boolean

`GET /extension/idle` answers with the moment activity stopped:

```json
{"idle": true, "idle_since": "2026-08-02T04:48:02Z", "threshold_s": 300}
```

`IdleTime` returns an error as well as a duration, and `xprintidle` can
start failing long after startup. Reporting that as `idle: false` would
be the worst available answer: the extension would hold its segment
open for as long as the detector stayed broken, which is the bug this
phase exists to remove. A detector error answers 503 instead, and the
extension reads any non-2xx as no answer and falls back.

A boolean would make the recorded end time depend on how soon the
extension asked. A timestamp lets it close the segment at 12:48:02
whatever time it learns, so polling granularity affects how quickly the
record is written and never what the record says.

That property is what makes the polling interval below a free choice
rather than an accuracy tradeoff.

`idle_since` comes from the same `IdleTime` the tracker uses, so the
two producers derive their boundaries from one clock and one detector.
Today they disagree by 38 minutes.

### The daemon needs an idle detector it does not have yet

`cmd/trackkrd/main.go` builds the idle detector inside the branch that
runs only when window detection succeeded, because until now nothing
else read it. This phase gives it a second reader.

The combination that breaks is exactly the one Phase 10 introduced: an
unsupported Wayland compositor, or sway IPC failing, returns no window
detector, and the daemon keeps running on the strength of the extension
alone. It then serves `/extension/idle` with nothing behind it.

So the detector moves out of that branch. One is constructed
unconditionally, the tracker and the extension server share it, and
`closerFor` closes it once -- it owns a swayidle child, and two owners
must not mean two `Close` calls or two processes.

### Polling on an alarm, not a timer

A Chrome MV3 service worker is evicted after about 30 seconds idle, and
`setInterval` dies with it. Firefox event pages suspend too. Only
`chrome.alarms` survives, waking the worker to run the poll.

Alarms fire at most every 30 seconds and may be delayed an arbitrary
amount beyond that. Neither bound matters here, because the daemon
returns a timestamp: a poll at 12:54, or one Chrome defers to 13:26,
still closes the segment at 12:48:02. Delay costs latency in writing
the record and never accuracy in what it says.

Chrome does not promise an alarm survives a worker restart before
Chrome 150, so the worker checks for its alarm on every startup and
recreates it when missing, as the alarms documentation recommends. An
extension that silently lost its alarm would look exactly like the bug
this phase is fixing.

The poll runs only while a segment is open. A browser with nothing being
timed has nothing to close, and waking a service worker on a timer to
learn that would spend battery for no result.

### Which source wins, precisely

"Prefer the daemon" is not an implementable rule. The existing
`idle.onStateChanged` handler calls `finalize()` the moment it fires,
and a record written that way cannot be withdrawn by a later daemon
reply. Two sources both allowed to end a segment means whichever speaks
first decides, which is how the extension came to trust the one that
was wrong.

One source is authoritative at a time, held in `storage.session` so it
survives service-worker eviction:

```text
daemon   the default, and the state after any successful reply
browser  entered on a failed or unusable reply, left on the next good one
```

A reply is unusable when the request fails, the status is not 2xx, the
body does not parse, or `idle_since` is in the future.

While the source is `daemon`, a `browser.idle` transition to `idle` no
longer finalizes anything by itself. It queries the daemon and acts on
that answer: end the segment at `idle_since` when the daemon agrees,
and do nothing when it does not, because on Wayland the browser's own
notion of idle is the unreliable one. Only when that query is unusable
does the handler fall through to today's `idleEndsAt` behaviour.

`locked` stays authoritative under either source and keeps its current
semantics. A lock is an explicit act, so it ends the segment at the
moment it arrives with no backdating -- the existing comment already
explains why backdating a lock would end a young segment before it
began. The daemon has nothing better to say about a deliberate act, and
waiting to ask it would only delay a correct finalize.

Every path runs inside the existing `serialize()`, so an alarm firing
and an idle event cannot interleave and finalize the same segment twice.

### Failure keeps the old behaviour, and says so

A daemon that cannot be reached, or one too old to serve the route,
leaves the extension on `browser.idle`. That is worse on Wayland and
correct on X11, which is exactly where an unreachable daemon is most
likely to be an old one.

The options page already classifies daemon connectivity for its status
display. It gains one line for this state, so a user on Wayland can see
that browser time is being measured by the mechanism known to overcount
rather than discovering it in a 41-minute record.

### Not capping segments at the threshold

The cheap fix is to refuse any browser segment longer than
`IDLE_SECONDS` without an intervening activity event. It needs no
protocol and no daemon.

It also destroys the records that are correct. Reading a long article,
watching a video, and sitting in a call are all real activity with no
tab events at all, and the daemon's idle inhibitor handling already
treats a full-screen video as non-idle. Capping would cut every one of
them at five minutes and call it accuracy.

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

1. `cmd/trackkrd/main.go`: construct the idle detector once, outside
   the `window != nil` branch, and hand it to both the tracker and the
   extension server. Close it once through `closerFor`.

2. `extension.go`: add `GET /extension/idle` beside the existing status
   route, behind the same `authorized` check, answering 503 on a
   detector error. The server reads a small interface that answers
   "idle since when", defined in the consuming package as the repo does
   elsewhere.

3. `logic.js`: the pure part, which is where this can be tested without
   a browser. Parse the reply, reject a malformed or future
   `idle_since`, and return the moment to close at, or null to keep the
   segment open.

4. `background-core.js`: the alarm and the arbitration from the
   decisions above. Create the alarm when a segment opens, clear it
   when none is open, re-create it on worker startup when missing, and
   ignore alarms that are not ours by name. Route both the alarm and
   the `idle.onStateChanged` handler through `serialize()`, and keep
   the source in `storage.session`.

5. Manifests: add `alarms` to both. The Chrome build takes it through
   `manifest.chrome.json`, which `mise run ext-build-chrome` stages.

6. `options.js`: report which idle source is in use, so the degraded
   state is visible rather than silent.

7. `docs/plan.md`: state that browser idle comes from the daemon, with
   the browser API as fallback.

## Tests

The Go side is ordinary handler testing against `httptest`: the route
answers a GET, rejects other methods, requires the token, and returns
`idle_since` only when idle. A fake idle detector supplies the state,
as `internal/tracker` already does for `IdleDetector`. One of those
fakes returns an error, asserting 503 rather than a cheerful
`idle: false`.

`main_test.go` covers the wiring that does not exist today: a daemon
built with a failing window detector and the extension enabled still
has an idle detector for the route to read, and still closes it once.

Pure helpers in `logic.js`, run by `mise run ext-test` with no browser:

- a reply with `idle: false` keeps the segment open
- a reply with `idle: true` returns its `idle_since`
- an `idle_since` after now is rejected rather than producing a segment
  that ends before it started
- a malformed body, a 401, a 404, and a 503 all fall back rather than
  throw, with 404 meaning a daemon too old for the route

One case deserves naming, because it is the bug in miniature: given a
segment opened at 12:45 and a reply of `idle_since = 12:48`, the result
is 12:48 regardless of whether the poll happened at 12:54 or 13:26. A
boolean-shaped reply cannot express that, and a test that polls
immediately would pass either way.

Behaviour, in the existing `background.test.js` and `chrome.test.js`
harnesses, because none of the above exercises the lifecycle:

- opening a segment creates the alarm; finalizing clears it
- an alarm event with someone else's name is ignored
- a worker that starts with no alarm and an open segment re-creates it,
  which is the state Chrome leaves behind before 150
- with the source `daemon`, a `browser.idle` idle event queries the
  daemon and does not finalize when the daemon disagrees
- with the source `browser`, the same event finalizes as it does today
- an unusable reply moves the source to `browser`, and the next good
  one moves it back
- `locked` finalizes at once under either source, without backdating
- an alarm firing and an idle event arriving together produce one
  finalize, not two

## Out of Scope

The desktop daemon. Phase 10 already gives it a correct detector on
every platform, and this phase makes the extension defer to it.

Reconciling the two producers on the dashboard. Once the extension
stops overcounting, desktop and browser records for the same minutes
still both exist by design, and how the dashboard presents that is a
separate question from whether the underlying numbers are true.

Title churn. Unread counters in window titles fragment desktop records
into hundreds of two-second segments, which is real and unrelated: it
lives in `tracker.go`'s title comparison, not in idle handling.

## Manual Verification

With `mise run dev` running and both extensions loaded, open a tab and
leave the machine for longer than the threshold.

The check is where the segment *ends*, not how long it is. A record
longer than the threshold is not a failure -- reading, a video, and a
call are all real activity with no tab events, and this phase keeps
those records on purpose. Comparing a maximum duration against the
threshold would fail every one of them.

What must hold is that the browser's last record before the gap ends at
the same instant the desktop's does, and that neither reaches into the
gap:

```sh
docker exec <db> psql -U trackkr -d trackkr -tAc \
  "select producer, max(ended_at) from activity_records
   where ended_at < <the moment you returned> group by producer;"
```

Both producers should land within seconds of each other, because both
now derive their boundary from one detector. On the run that prompted
this phase they were 38 minutes apart.

Then read the gap itself: no record of any producer may overlap the
period you were away. That is the assertion the 2453-second record
would have failed, and a duration comparison would not have caught it
if the user had spent that time watching something.

Repeat on X11 to confirm the fallback still works when the daemon is
stopped, and check the options page reports which source is in use.
