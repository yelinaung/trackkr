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

A boolean would make the recorded end time depend on how soon the
extension asked. A timestamp lets it close the segment at 12:48:02
whatever time it learns, so polling granularity affects how quickly the
record is written and never what the record says.

That property is what makes the polling interval below a free choice
rather than an accuracy tradeoff.

`idle_since` comes from the same `IdleTime` the tracker uses, so the
two producers derive their boundaries from one clock and one detector.
Today they disagree by 38 minutes.

### Polling on an alarm, not a timer

A Chrome MV3 service worker is evicted after about 30 seconds idle, and
`setInterval` dies with it. Firefox event pages suspend too. Only
`chrome.alarms` survives, waking the worker to run the poll.

Alarms are limited to one minute. That is coarse against a 300s
threshold and does not matter, because the daemon returns the timestamp:
a poll at 12:54 still closes the segment at 12:48:02. The cost of the
coarse interval is up to a minute of delay before the record is
written, never a minute of wrong duration.

The poll runs only while a segment is open. A browser with nothing being
timed has nothing to close, and waking a service worker every minute to
learn that would spend battery for no result.

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
internal/tracker/extension.go        # GET /extension/idle
internal/tracker/extension_test.go   # route coverage
extension/background-core.js         # alarm, poll, finalize from it
extension/logic.js                   # pure: parse the reply, pick the end
extension/manifest.json              # alarms permission
extension/manifest.chrome.json       # alarms permission
extension/options.js                 # surface the fallback state
docs/plan.md                         # how browser idle is decided
```

## Steps

1. `extension.go`: add `GET /extension/idle` beside the existing
   status route, behind the same `authorized` check. It reports from
   the tracker's idle detector, so the server needs a reference to it
   or to a small interface that answers "idle since when". Keep that
   interface in the consuming package, as the repo does elsewhere.

2. `logic.js`: the pure part, which is where this can be tested without
   a browser. Parse the reply, reject a malformed or future
   `idle_since`, and return the moment to close at, or null to keep the
   segment open.

3. `background-core.js`: create the alarm when a segment opens, clear
   it when none is open, and on each firing poll the daemon and
   `finalize()` at the returned instant. Leave the existing
   `idle.onStateChanged` listener in place as the fallback, and prefer
   the daemon's answer whenever one arrives.

4. Manifests: add `alarms` to both. The Chrome build takes it through
   `manifest.chrome.json`, which `mise run ext-build-chrome` stages.

5. `options.js`: report which idle source is in use, so the degraded
   state is visible rather than silent.

6. `docs/plan.md`: state that browser idle comes from the daemon, with
   the browser API as fallback.

## Tests

The Go side is ordinary handler testing against `httptest`: the route
answers a GET, rejects other methods, requires the token, and returns
`idle_since` only when idle. A fake idle detector supplies the state,
as `internal/tracker` already does for `IdleDetector`.

The JavaScript side keeps its logic in `logic.js` so `mise run ext-test`
covers it with no browser:

- a reply with `idle: false` keeps the segment open
- a reply with `idle: true` returns its `idle_since`
- an `idle_since` after now is rejected rather than producing a segment
  that ends before it started
- a malformed body, a 401, and a 404 all fall back rather than throw,
  with 404 meaning a daemon too old for the route

One case deserves naming, because it is the bug in miniature: given
a segment opened at 12:45 and a reply of `idle_since = 12:48`, the
result is 12:48 regardless of whether the poll happened at 12:54 or
13:26. A boolean-shaped reply cannot express that, and a test that
polls immediately would pass either way.

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

The check is the comparison that failed here. After returning:

```sh
docker exec <db> psql -U trackkr -d trackkr -tAc \
  "select producer, max(duration_s) from activity_records group by producer;"
```

No producer should exceed the threshold by more than a poll interval,
and the browser's longest record should now sit in the same range as
the desktop's. Before this phase the desktop maximum was 229s and the
browser maximum was 2453s on the same machine over the same afternoon.

Then confirm the end times agree: the last browser record and the last
desktop record before the gap should close within seconds of each
other, because both now derive from the same detector.

Repeat on X11 to confirm the fallback still works when the daemon is
stopped, and check the options page reports which source is in use.
