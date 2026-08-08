# Phase 14: Property-Based Testing

## Why

The test suite is large, careful, and almost entirely example-based. That
shape suits code whose contract is a specific output -- rendered HTML, an
error string, a redirect target -- which is why the handlers and the
templates are well covered. It suits the arithmetic much less well.

Three areas hold invariants that no finite table of examples can pin
down:

- **Deduplication** (`internal/db/activity_dedup.go`) computes one
  quantity two ways. `timeline` materializes uncovered desktop slices;
  `totals` sums them from a prefix table without materializing anything.
  The file says the two agree. Nothing checks it.
- **Timeline geometry** (`internal/server/timeline.go`) claims a bar's
  width is exactly its duration even after the axis drops empty hours,
  and that a merged bar covers exactly the span of the records it
  replaces. Comments state both; two or three examples test them.
- **Normalization** (`icon.AppKey`, `db.SiteFromURL`,
  `favicon.CanonicalSite`, `icon.Normalize`) runs over full Unicode and
  arbitrary bytes, checked today against a dozen ASCII examples each.

Property-based tests fit those three. Most of the rest of the repo is
better served by the tests it already has, and the sections below name
which parts.

## Tool

[Hegel](https://hegel.dev) for Go, `hegel.dev/go/hegel`, pinned at
v0.6.25. It draws values imperatively -- `hegel.Draw(ht, gen)` anywhere
in the test body, in loops and under conditions -- and shrinks a failure
to a minimal counterexample. Tests are ordinary `go test` functions, so
`mise run test` picks them up with no runner change.

### Amend the testing policy first

`AGENTS.md:114-116` permits the standard library `testing` package only,
with no assertion library. Every batch below breaks that rule as written,
so the rule needs an explicit exception before the first test lands.

Hegel generates inputs and shrinks counterexamples. It does not assert.
The policy exists to stop a handful of tests growing a DSL for saying
`got != want`, and property tests still spell that out longhand inside
`if got != want { ht.Fatalf(...) }`. Amend `AGENTS.md` to permit
`hegel.dev/go/hegel` in generative tests, keep the ban on assertion
libraries, and record that a test generating no inputs has no reason to
import hegel.

### Setup

1. `go get hegel.dev/go/hegel@v0.6.25`
2. Add `.hegel/` to `.gitignore`.
3. Nothing else. No new mise task -- these are `go test` functions in the
   existing suite.

### What the dependency costs

Verified against the module in the module cache. The published reference
is stale on the first point and wrong on two APIs.

- **No runtime download.** Since v0.6.18 hegel vendors `libhegel` for all
  five supported platforms inside the Go module; the changelog says it
  "no longer downloads libhegel from GitHub at runtime, so builds are
  self-contained and work offline." First use materializes the matching
  `.so` into a per-version directory under `os.UserCacheDir()` --
  `hegel-go/libhegel/<version>`, created 0700 -- and `dlopen`s it.
- **A `.hegel/` directory per tested package.** The failing-example
  database goes to the test's working directory, which under `go test` is
  the package directory, so `internal/icon/.hegel/` appeared the first
  time a property ran there. Grepping the Go wrapper for the path finds
  nothing, because libhegel chooses it on the Rust side. Ignore `.hegel/`
  and keep the database: replaying a counterexample is the point of it.
- **No cgo.** The loader contains no `import "C"`, so `CGO_ENABLED=0`
  builds are unaffected.
- **About 12 MB of vendored shared objects** in the module cache, one
  platform's worth of which ever loads. That is the real price, and
  `go mod download` pays it.
- **Frequent patch releases,** almost all of them libhegel bumps: six in
  the six weeks up to v0.6.25, two of those on consecutive days. Pin the
  version and let Renovate raise them.

The runner needs a writable `os.UserCacheDir()`, which the build cache
already requires. Confirm it on the batch 1 branch; it gates nothing.

### API notes

Two APIs in the published reference do not exist in v0.6.25, and
discovering that at compile time costs an afternoon.

- `WithDatabase` takes a plain path string. A non-empty path persists
  failing examples there, an empty string disables persistence. Neither
  `hegel.Database(...)` nor `hegel.DatabaseDisabled()` exists.
- `SetHegelDirectory` does not exist. The default database location is
  libhegel's own and CI disables it automatically, so no test here needs
  to configure it.

Read the package source when an API refuses to compile.

### Constraints

- **Coverage must clear 70%,** enforced at `ci.yml:177`, not the 50%
  `AGENTS.md` claims. Correct `AGENTS.md` as part of batch 1. Property
  tests count normally and should help: `mergeActivityIntervals` and
  `visitUncoveredActivityIntervals` are reached today only incidentally,
  through their callers.
- **Runtime grows.** The default is 100 cases per property, against a
  suite that currently finishes in seconds. Tune with
  `hegel.WithTestCases(n)` per test, never by narrowing a generator.
- **CI disables the example database** and turns `WithDerandomize` on, so
  CI runs reproduce without depending on cached state surviving between
  jobs.

### Conventions

These extend the testing rules in `AGENTS.md`; they replace none of them.

- Property tests live in the existing `_test.go` file for the code under
  test. No `*_pbt_test.go` files.
- `hegel.Test(t, ...)` replaces `t.Parallel()` inside the property body.
  The outer test function keeps `t.Parallel()` wherever the existing
  rules already allow it.
- Draw the full domain. No `min` or `max` added to dodge an edge case. A
  bound is legitimate only when the function's contract excludes the
  input, or when the *test's own* arithmetic would wrap -- and the cure
  there is a narrower draw widened into `int`, not a narrower contract.
- One property per test function, named for the property:
  `TestMergeActivityIntervalsPreservesUnion`, not `TestMergeProperties`.
  Facets of a single contract that share both a generator and a reference
  implementation are the exception, because splitting them duplicates the
  expensive half of the test. `mergeActivityIntervals` is the case in
  point: sortedness, disjointness, non-adjacency, and union preservation
  are four readings of one output. Keep the generator and the naive
  reference in package-level helpers, then assert the four facets in one
  function under distinct messages, so a failure still names which one
  broke.

## What not to test this way

- HTTP handlers, `internal/server/templates.go`, and rendered HTML. The
  contract is exact output.
- `Config.Validate` and config loading in both packages. The contract is
  a specific error for a specific malformed file, and the existing table
  tests are the right shape.
- Platform detectors that shell out (`sway_linux.go`,
  `idle_sway_linux.go`, and `window_linux.go` beyond `parseWMClass`).
  The interesting behavior is process handling, not input space.
- `monogramForeground` contrast. The domain is 360 hues, so exhaust it in
  a loop; sampling it randomly would be strictly worse.
- Anything needing Postgres, except the one differential test in batch 3
  that exists to compare Go against SQL.
- `favicon.validateRemoteURL`. It mutates its argument, writing the
  canonicalized host back into `target.Host` (`fetcher.go:274`), and the
  mutation is load-bearing -- the fetch then goes to the normalized host.
  A property would restate a single assignment on a URL the caller just
  built. The mutation is deliberate, and no test covers it.

## Batch 1 -- Pure normalization and parsing

Batch 1 needs no infrastructure and runs fast. It is where hegel proves
it fits the repo, before the harder batches lean on it.

### `internal/icon` (`app_test.go`, `raster_test.go`)

`AppKey` idempotence -- `AppKey(AppKey(s)) == AppKey(s)` over full
Unicode text. Idempotence is a contract here, not a nicety: `Validate`
rejects any key that is not its own `AppKey`, so a non-idempotent
`AppKey` would mint keys the validator refuses.

`AppKey` output shape -- no leading, trailing, or repeated whitespace,
and no uppercase. Draw text with the whitespace categories included.

`Validate` accepts what `AppKey` produces -- for any name whose
`AppKey` is non-empty and within `MaxKeyBytes`, `Validate` succeeds
against valid PNG bytes. `strings.ToLower` can *lengthen* a string, since
U+0130 lowercases to two runes, which is why `db.AppIconKeys` re-checks
the length. The property should confirm that guard is the only one
needed.

`ValidatePNG` never panics on arbitrary bytes -- `hegel.Binary(0, -1)`.
Absence of a panic is the only universal claim. The function sits behind
an HTTP upload, so a crash on hostile bytes is the failure that matters.
Do not also demand an error: arbitrary bytes can encode a small valid
PNG, and hegel will eventually produce one. Demand the error only for
inputs the property itself puts outside the contract -- oversized, wrong
magic, out-of-range dimensions. Everywhere else assert the disjunction,
that the call returns either `Details` with no error or an error wrapping
`ErrInvalid`, never a panic and never a bare error.

`Normalize` postconditions -- for any decodable source inside the
dimension and pixel limits, the output passes `ValidatePNG` and measures
exactly `NormalizedDimension` square. Build sources with a composite
generator: draw width and height in `[1, 1024]` under the pixel cap, fill
with drawn RGBA, encode as PNG.

`Normalize` on arbitrary bytes -- no panic, and decode failures come back
as errors. This is the closest thing to a fuzz target in the repo, and
the input is attacker-supplied.

`Normalize` idempotence on its own output -- worth *checking*, not
asserting blind. A 64x64 CatmullRom resample to 64x64 may not be
byte-identical. If it is not, weaken the claim to dimension and validity
stability, and record why.

### `internal/identity` (`identity_test.go`)

`New` always produces a `Valid` id carrying version 4 and the RFC 4122
variant.

`Derive` always produces a `Valid` id, and repeats it for the same
producer and parts.

`Derive` is injective in its parts. Expect this one to fail.
`Derive(p, "a", "b")` and `Derive(p, "a\x00b")` hash identical bytes,
because `Derive` joins with `\x00` and commits neither a length nor a
count to the digest. `TestDeriveSeparatesParts` checks only `("ab","c")`
against `("a","bc")`, which the separator does handle. Today's callers
pass URLs and RFC 3339 timestamps, so no real collision is reachable --
confirm that before choosing between fixing the digest input and
narrowing the property under a comment that says why.

`Valid` implies canonical -- `Valid(id)` means `id ==
strings.ToLower(id)` and `len(id) == 36`. The doc comment says uppercase
is rejected deliberately; make the claim enforceable.

### `internal/db` (`site_test.go`)

`SiteFromURL` never panics on arbitrary text.

`SiteFromURL` output shape -- on success the result carries no `@`, no
uppercase, and no trailing dot. One thing that looks true is not: the
result can still start with `www.`, because only one prefix is stripped,
so `www.www.example.com` yields `www.example.com`. Assert what the code
promises, not what the name suggests.

`SiteFromURL` round-trips a constructed URL -- for a host drawn from
`hegel.Domains()`, `SiteFromURL("https://" + host + "/")` returns that
host lowercased, minus any `www.` prefix.

### `internal/favicon` (`fetcher_test.go`)

`CanonicalSite` idempotence -- a successful result is itself a valid
input producing the same output. The result is a cache key, and two
spellings of one site would mean two cache entries and two fetches.

`CanonicalSite` output shape -- on success every label passes
`validDNSLabel`, the result is lowercase ASCII, and it is not an IP
literal. Draw full Unicode so the IDNA paths run.

`CanonicalSite` never panics -- on arbitrary text, including strings
shaped like URLs and IPv6 literals.

### `internal/tracker` (`window_linux_test.go`, `idle_linux_test.go`)

`parseWMClass` never returns an empty string. It falls back to
`unknownApp`, and an empty app name would produce a record the server has
to reject.

`parseIdleMs` round-trips a decimal integer. The function is
`strconv.ParseInt` plus a multiply (`idle_linux.go:42-48`), so a negative
result is correct by construction -- `"-1"` parses to `-1ms`. Two
properties hold plainly: it never panics on arbitrary text, and it errors
exactly when the trimmed input is not a base-10 `int64`.

The third needs an oracle that is not the implementation. Asserting
`result == ms * time.Millisecond` restates the function's own expression,
overflow included, so it passes by construction at exactly the inputs
worth asking about: above roughly 9.2e12 ms, about 292 years,
`time.Duration(ms) * time.Millisecond` wraps, and both sides wrap
together. Draw `ms` across the full `int64` range, feed
`strconv.FormatInt(ms, 10)` as the input, and compute the expected
nanoseconds with `math/big`. Assert that the exact product fits in
`int64` and that the returned duration equals it.

Written that way the property **fails** on overflow instead of
accommodating it, which is the point: it forces a decision nobody has
made. Triage when it fails. Either `parseIdleMs` should reject an
out-of-range magnitude, and it already returns an error so there is
somewhere to put one, or `xprintidle` genuinely bounds the domain, in
which case narrow the draw and name the real bound in a comment. Do not
silently assert the wrap.

Whether the caller should reject a negative idle time belongs to
`IdleTime`, not to the parser.

### `internal/server` (`templates_test.go`)

`humanDuration` over the full `int64` domain. `time.Duration(seconds) *
time.Second` overflows above roughly 9.2e9 seconds, and the function
takes an `int64` straight from a SQL sum. Expect a wrapped negative
rendering as `"-..."`, or nonsense at the boundary. Investigate before
constraining: the cure may be a clamp inside the function, since the type
says it accepts any `int64`.

## Batch 2 -- Deduplication

`internal/db/activity_dedup.go` is 460 lines of interval arithmetic with
two independent paths to one number, and it is where these tests pay for
themselves. Add them to `activity_dedup_test.go`.

**`mergeActivityIntervals` preserves the union.** Draw a list of
intervals, then assert the output is sorted, pairwise disjoint,
non-adjacent, and covers exactly the instants the input covered. Model
the union as a sorted set of boundary points computed by a naive O(n^2)
reference; slowness costs nothing here. The four facets share one test
function.

Non-adjacency is a deliberate contract, not a universal truth about merge
functions, and the property should assert it as one. The merge condition
is `interval.start.After(previous.end)` (`activity_dedup.go:364`), so an
interval starting exactly where the previous one ended gets absorbed. That
choice is what turns a browser reporting back-to-back one-second tabs
into a single coverage span instead of thousands, and
`TestDeduplicateFirefoxActivityKeepsTouchingIntervals` already asserts it
for one pair. Generate the ties deliberately: draw timestamps from a
small pool so exact `start == end` coincidences occur often, instead of
hoping a wide draw stumbles into them.

**Build coverage canonically; do not draw it.** Both functions below take
coverage as a precondition, not as arbitrary input, and a generator that
ignores the precondition reports failures production can never
experience. `visitUncoveredActivityIntervals` binary-searches its
coverage slice (`activity_dedup.go:313`), so it assumes sorted, merged
input: hand it `[5,10]` before `[0,6]` and it reports a gap that is not
there. `visibleUncoveredDuration` also indexes `visibleGapPrefix`
(`activity_dedup.go:289`), which only `newActivityDeduplicator` builds --
pass merged intervals with a nil prefix and two of them in range, and it
panics on the index instead of answering.

Write one helper that draws raw intervals, runs them through
`mergeActivityIntervals`, and builds the matching `visibleGapPrefix` the
way `newActivityDeduplicator` does (`activity_dedup.go:51-66`). Every
property in this section takes its coverage from that helper. The merge
property above already covers raw input; these two are about what happens
downstream of it. If the prefix construction deserves exercise of its
own, promote it out of `newActivityDeduplicator` into a named function
and give it a property -- do not test it by accident through a panic.

**`visitUncoveredActivityIntervals` partitions the subject.** When it
returns `true`, having stayed inside the work limit, the visited
intervals are disjoint, ascending, contained in the subject, and their
union is exactly the subject minus the coverage union. The cursor advance
in that loop is the subtlest code in the file.

**`visitUncoveredActivityIntervals` respects the work limit.** It never
exceeds `workLimit` increments, and returns `false` exactly when it
stopped early. The bound guards against adversarial overlaps, so it wants
adversarial inputs, which a generator supplies for free.

**`visibleUncoveredDuration` agrees with materialized slices.** For a
drawn subject interval and canonical coverage, the
binary-search-plus-prefix-sum result must equal the sum of the slices
`visitUncoveredActivityIntervals` yields, keeping only those at least
`minEffectiveSliceDuration` long. Two implementations of one quantity sit
in the file, and nothing has ever compared them. This is the
highest-value property in the batch.

**`boundedActivityRecords` is a top-k.** A stateful model test: `RuleAdd`
pushes a drawn record into both the heap and a plain slice. Invariant:
`sorted()` equals the first `limit` elements of the model sorted by
`compareActivityRecords`, and `truncated` is true exactly when an add was
rejected, which is to say when more records arrived than `limit` admits.

Phrasing the second half as "or `limit` is zero" breaks it. A limiter
built with zero -- or with a negative limit, normalized to zero by
`max(0, limit)` at `activity_dedup.go:415` -- starts with
`truncated == false` and only sets the flag inside `add`
(`activity_dedup.go:421-423`). `RunStateful` checks invariants against
the freshly built machine before any rule runs, so the zero-limit
phrasing fails that first check against a machine behaving correctly.
`addCount > limit` holds at every limit including zero, and it stays
falsifiable in the direction that matters: a limiter that drops a record
silently without setting the flag.

The generator decides whether this property can fail at all. The
comparator has three levels -- `StartedAt`, then `DeviceID`, then `ID`
(`activity_dedup.go:375-383`) -- and `AppName` is not among them. Records
with distinct `StartedAt` values exercise only the first level and would
let a broken tiebreak through. Draw `StartedAt` from a small pool of
instants and `DeviceID` from two or three devices, keeping `ID` unique,
so ties at the first and second levels are common and the later
comparisons decide the order.

**`categoryTotals` partitions exactly.** The comment claims largest
remainder makes category seconds partition the rounded application
seconds exactly. Watch the `for i := range remaining` loop: if
`remaining` can exceed `len(remainders)` it indexes out of range, and a
generator over many destinations with fractional durations is how that
surfaces.

The per-application claim is not observable from the return value.
`categoryTotals` accumulates into `secondsByDestination` across every
application (`activity_dedup.go:213-243`) and returns one row per
category, so once two applications share a category their allocations sum
and cannot be separated again. Two case shapes make the property
implementable, and the plan takes both.

- **Single-application cases.** Draw records that all canonicalize to one
  app name, varying only the category destinations and the durations. The
  returned rows then *are* that application's allocation, and the
  partition claim is directly assertable. This is the tighter test, and
  the one that would localize a largest-remainder bug.
- **Aggregate cases.** With many applications, assert the global identity
  instead: the sum of all returned category seconds equals the sum over
  applications of `round(applicationDuration)`, computed by an
  independent model built from `visitEffectiveDurations`. Weaker, but it
  is the property that holds of the published numbers, and it covers the
  cross-application interaction the single-app case cannot.

Both shapes need the same duration bound, and the test should say why.
`remaining` comes from `applicationDuration.Seconds()`
(`activity_dedup.go:232`), a `float64` whose mantissa stops representing
nanosecond-exact values past 2^53 ns, about 104 days of accumulated
activity for one application. Past that the rounding turns approximate
and the partition property fails for a reason that says nothing about
largest remainder.

Bound the **aggregate**, not each record. Every generated record can
canonicalize to one application, and in the single-application case they
all do by construction, so a per-record cap under an unbounded list
length caps nothing. Draw the record count in `[0, 200]` and per-record
durations up to an hour, which puts worst-case accumulation near 8.3
days, more than an order of magnitude under the ceiling. Drawing a total
budget and splitting it across records works equally well. Either way the
generator must maintain `count * maxRecordDuration << 2^53 ns`, and that
invariant belongs in a comment beside the draw, so widening one bound
later cannot quietly void the other.

This is the batch's one legitimate generator bound, and it is the kind
that keeps the test from producing meaningless input, not a claim about
the function's domain. Query windows run to days and weeks, so the
ceiling is unreachable for a real aggregate.

**`totals` agrees with `timeline`.** Under a record limit high enough
that nothing truncates, summing `timeline`'s slices per canonical app
name equals `totals` over the same window, up to the sub-second slices
`timeline` drops. State that tolerance explicitly instead of fudging it.

## Batch 3 -- Timeline geometry, limiters, SQL parity

### `internal/server/timeline.go`

**`chartWindow` invariants** (`timeline_test.go`). For any records and
any day, DST days in a real zone included: `dayStart <= start < end <=
dayEnd`, the window contains every record's clamped extent, and the span
is at least `min(minChartWindow, dayEnd - dayStart)`. The three
sequential clamps closing that function have exactly the shape of code
that holds for the cases someone thought of.

Fold the empty and no-overlap cases into the same property instead of
excluding them: draw the record list without a minimum size, and draw
records falling outside the day. Both take the `recordExtent`
`ok == false` path, which returns the **whole day**, not a minimum window
(`timeline.go:114-119`). All three invariants hold for it -- a 23-to-25
hour span clears the `minChartWindow` floor, and the clamped-extent
clause is vacuous when nothing overlaps. One property covers the fallback
without a special case, and notices if a later change gives it one.

**`mergeAdjacentActivity` conserves covered time**
(`timeline_merge_test.go`). Per device and app, the union of merged
intervals equals the union of the input intervals, and no merged bar
bridges a gap. `TestMergeAdjacentActivityPreservesCoveredTime` is the
single-example version; evolve it instead of adding beside it.

**`activeHourScale` preserves widths** (`timeline_scale_test.go`). The
central claim of the compressed axis: for any record inside the window,
`minutesAt(end) - minutesAt(start)` equals its duration in minutes.
Separately, `minutesAt` is monotonic non-decreasing in `t`. Draw records
that leave real gaps, since a scale is built only when some hour is empty
and some hour is not.

**`toBar` stays inside the chart.** For any record and window, a returned
bar has `X >= 0`, `Width > 0`, and `X + Width <= span`. The
minimum-width floor and the trailing clamp interact, and nothing checks
the interaction.

### Limiters

**`attemptLimiter`** (`middleware_test.go`) and **`slidingWindowLimiter`**
(`app_icons_test.go`) as stateful model tests over a virtual clock.
Rules: reserve, release or refund, reset, and advance time by a drawn
duration. Model: a multiset of reservation timestamps per key.
Invariant: the count of reservations inside the window never exceeds
`limit`, and refunding a genuine reservation restores exactly one slot.

Pin down two behaviors before writing that invariant.
`slidingWindowLimiter.refund` matches on timestamp equality, so two
reservations at one instant are interchangeable, and refunding an
already-swept reservation does nothing. Decide which of the two is
contract and which is accident.

A login throttle and an upload quota sit behind these, which is reason
enough to want inputs nobody hand-picked.

### `SiteFromURL` against SQL (`site_test.go`)

`TestSiteFromURLMatchesSQL` already runs both derivations over thirteen
shared examples. Generating the URLs extends it naturally, and this is
the one place a database-backed property test earns its cost: the Go port
exists precisely so it must not drift from `siteExpr`.

The test uses `testPool(t)`, so it skips without Postgres and must not
call `t.Parallel()`. Each case is a round trip, so drop to roughly
`hegel.WithTestCases(200)` and accept the runtime. CI sets
`TRACKKR_TEST_DSN` (`ci.yml:159`), so unlike a bare developer machine it
will not skip, and those 200 round trips run on every push -- measure
them on the trial branch and lower the count if the job notices.

Generate URLs compositionally: scheme, optional userinfo, host or
bracketed IPv6 literal, optional port, path. Raw text would almost never
match the authority pattern and would spend every case on the drop path.
Mix in a minority of raw `Text()` draws to cover that path deliberately.

## Sequencing

1. **Amend `AGENTS.md`.** The generative-testing exception to the
   standard-library rule, and the coverage figure corrected from 50% to
   the 70% CI enforces. A docs commit, and the one true prerequisite:
   without it, batch 1 contradicts the project's own instructions.
2. **Batch 1, `internal/icon` only.** One package, five properties.
   Settles the runtime question, shows whether `mise run lint` has
   opinions about the new code, and confirms in passing that the runner
   materializes the vendored library without complaint.
3. **Rest of batch 1.** Normalization and parsing across the four other
   packages.
4. **Batch 2.** Deduplication, in the order listed above, with
   `mergeActivityIntervals` first because everything else builds on it.
5. **Batch 3.** Timeline geometry, then the limiters, then the SQL parity
   test last, since it alone carries an infrastructure dependency.

Each batch is one commit, `test:` prefixed. Failures found along the way
get their own commits under their own prefix. A property test that fails
on real behavior gets triaged three ways before anything changes: real
bug, unsound property, or an input genuinely outside the function's
domain. The third is rarest and needs the comment.

## Findings, against the predictions

Four predictions were recorded before the batches ran, so they could be
judged rather than reconstructed. Three landed and one was impossible.

**`identity.Derive` collided, and worse than predicted.** The forecast
was `("a","b")` against `("a\x00b")`, dismissed as unreachable. The
generator found `Derive(p)` against `Derive(p, "")` in seconds, and
reachability turned out to be the wrong dismissal: nothing sanitizes
window titles, so an application name and a title differing only in
where a NUL falls derived one identity and the second record was dropped
as a replay. Each field is now length-prefixed with the part count
committed.

**Both overflow predictions landed, and the reachability argument was
wrong both times.** `humanDuration(9223372037)` rendered
`"-9223372036s"`, and a total sums up to `ActivitySourceLimit` records
clipped to the query window -- 25000 across a week clears the ceiling, so
the dashboard could show negative time. `parseIdleMs` wrapped past
`maxIdleMs` and also accepted negatives, either of which reads as an
active session and keeps the tracker recording. Both are fixed and both
properties now run the full `int64` domain.

**The `categoryTotals` loop cannot over-index.** `remaining` is
`round(Σ fractional parts)`, every fraction is under a second, so the sum
is strictly below the destination count and its rounding can equal that
count but never exceed it. The property that replaced the prediction
checks the partition identity instead, which is what a reader sees.

**The timeline geometry holds.** `chartWindow`'s clamps and `toBar`'s
width floor were the last prediction and neither breaks, including on
23- and 25-hour DST days. The compressed axis really does preserve bar
widths, which is the one claim a timeline makes that a list does not.

### What the exercise taught

The properties that found defects were the ones with an oracle
independent of the implementation: `math/big` against a wrapping
multiply, a rendered string parsed back by separate arithmetic, a
quadratic union against a sort-and-sweep, PostgreSQL against its Go port.
Properties that restate the code agree with it.

Three rounds of review caught properties passing for the wrong reasons,
and the failure modes are worth naming because they recur. A generator
bounded just short of a defect documents the defect as intended
behaviour. A property that draws arbitrary text and returns on error is
satisfied by an implementation that rejects everything. A conservation
check is blind to misallocation. Each looked green.

Two mutation tests are the habit that came out of it: break the code,
confirm the property names the break. That is how the sliding-window
limiter and the SQL parity test earned their green, and it costs a
minute.
