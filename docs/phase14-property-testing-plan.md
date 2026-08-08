# Phase 14: Property-Based Testing

## Why

The test suite is large, careful, and almost entirely example-based. That
suits code whose contract is a specific output -- rendered HTML, an error
string, a redirect target -- and it is why the handlers and templates are
well covered. It suits the arithmetic much less well.

Three areas of this codebase hold invariants that no finite table of
examples can pin down:

- **Deduplication** (`internal/db/activity_dedup.go`) computes the same
  quantity two ways. `timeline` materializes uncovered desktop slices;
  `totals` sums them from a prefix table without materializing anything.
  The file states the two agree. Nothing checks it.
- **Timeline geometry** (`internal/server/timeline.go`) claims a bar's
  width is exactly its duration even after the axis drops empty hours,
  and that a merged bar covers exactly the span of the records it
  replaces. Both are stated in comments and tested at two or three points.
- **Normalization** (`icon.AppKey`, `db.SiteFromURL`,
  `favicon.CanonicalSite`, `icon.Normalize`) are idempotent functions
  over full Unicode and arbitrary bytes, tested against a dozen ASCII
  examples each.

Property-based tests are the right instrument for those. They are the
wrong instrument for most of the rest of the repo, and this plan says so
explicitly rather than leaving it to be discovered per-package.

## Tool

[Hegel](https://hegel.dev) for Go, `hegel.dev/go/hegel`, pinned at
v0.6.24. It draws values imperatively -- `hegel.Draw(ht, gen)` anywhere
in the test body, including in loops and conditionally -- and shrinks
failures to a minimal counterexample. Tests are ordinary `go test`
functions, so `mise run test` picks them up with no runner change.

### Amend the testing policy first

`AGENTS.md:114-116` says standard library `testing` only, with no
assertion library. Every batch below violates that as written, so the
policy needs an explicit exception before any of this lands -- not a
quiet one established by the first commit.

The exception is narrow and worth stating in those terms: hegel is an
input generator and a shrinker, not an assertion library. The reason the
policy exists -- that a handful of tests should not grow a DSL for
saying `got != want` -- is untouched, because property tests still assert
with plain `if got != want { ht.Fatalf(...) }`. Amend `AGENTS.md` to
permit `hegel.dev/go/hegel` for generative tests, keep the ban on
assertion libraries, and say that a test which does not generate inputs
has no reason to import hegel.

### Setup

1. `go get hegel.dev/go/hegel@v0.6.24`
2. Nothing else. No `.gitignore` change, no new mise task -- these are
   `go test` functions in the existing suite.

### What the dependency actually costs

Verified against the module in the cache rather than taken from the
docs, which are stale on this point:

- **No runtime download, and no repository directory.** Since v0.6.18
  hegel vendors `libhegel` for all five supported platforms and embeds
  them in the Go module; the changelog is explicit that it "no longer
  downloads libhegel from GitHub at runtime, so builds are self-contained
  and work offline." At first use it materializes the matching `.so` into
  a per-version directory under `os.UserCacheDir()` -- `hegel-go/libhegel/<version>`,
  created 0700 -- and `dlopen`s it. Nothing is written to the repository,
  so there is nothing to gitignore.
- **No cgo.** The loader uses no `import "C"`, so `CGO_ENABLED=0` builds
  are unaffected.
- **About 12 MB of vendored shared objects** in the module cache, of
  which one platform's is ever used. This is the real cost, and it is
  paid at `go mod download` time.
- **Frequent patch releases,** mostly libhegel bumps -- five in the six
  weeks before v0.6.24. Renovate will raise them; pin the version and let
  it.

The CI risk this section previously described does not exist. What
remains is ordinary: the runner needs a writable `os.UserCacheDir()`,
which it needs for the build cache anyway. Confirm it incidentally on the
batch 1 branch rather than gating the plan on it.

### API notes

The published reference is out of date in two places that would otherwise
cost an implementation session:

- `WithDatabase` takes a plain path string. A non-empty path persists
  failing examples there; an empty string disables persistence. There is
  no `hegel.Database(...)` or `hegel.DatabaseDisabled()` constructor.
- There is no `SetHegelDirectory`. The default database location is
  libhegel's own, and CI environments disable it automatically, so no
  test here should need to configure it at all.

Check the package source, not the reference, when an API does not
compile.

### Other constraints

- **Coverage threshold is 70%,** enforced at `ci.yml:177`, not the 50%
  `AGENTS.md` claims. The two disagree today; `AGENTS.md` should be
  corrected as part of batch 1. Property tests count normally and should
  help -- `mergeActivityIntervals` and `visitUncoveredActivityIntervals`
  are currently reached only incidentally through their callers.
- **Runtime.** 100 cases per property is the default. The suite currently
  runs in seconds; budget for it growing. Tune with
  `hegel.WithTestCases(n)` per test, not by shrinking generators.
- **The example database is disabled in CI** and `WithDerandomize` is on
  by default there, so CI runs are reproducible and do not depend on any
  cached state surviving between jobs.

### Conventions

These extend the testing rules already in `AGENTS.md`, they do not
replace them.

- Property tests live in the existing `_test.go` file for the code under
  test. No `*_pbt_test.go` files.
- `hegel.Test(t, ...)` replaces `t.Parallel()` inside the property body;
  the outer test function keeps `t.Parallel()` where the existing rules
  allow it.
- Draw the full domain. No `min`/`max` added to dodge an edge case. A
  bound is legitimate only when the function's contract excludes the
  input, or when the *test's own* arithmetic would wrap -- and the fix
  there is a narrower draw widened into `int`, not a narrower contract.
- One property per test function, named for the property:
  `TestMergeActivityIntervalsPreservesUnion`, not `TestMergeProperties`.
  The exception is a set of facets of one contract that share a generator
  *and* a reference implementation -- splitting those duplicates the
  expensive part of the test. `mergeActivityIntervals` is the case in
  point: sortedness, disjointness, non-adjacency, and union preservation
  are four readings of one output. Keep the generator and the naive
  reference in package-level helpers, then assert the facets in one
  function with a distinct message per facet, so a failure still names
  which one broke.

## What not to test this way

Listed so the next reader does not re-litigate it.

- HTTP handlers, `internal/server/templates.go`, and rendered HTML. The
  contract is exact output.
- `Config.Validate` and config loading in both packages. The contract is
  a specific error for a specific malformed file. The existing table
  tests are the right shape.
- Platform detectors that shell out (`sway_linux.go`, `idle_sway_linux.go`,
  `window_linux.go` beyond `parseWMClass`). The interesting behavior is
  process handling, not input space.
- `monogramForeground` contrast. The domain is 360 hues -- exhaust it in
  a loop; a random sample would be strictly worse.
- Anything requiring Postgres, except the one differential test in
  batch 3 that exists precisely to compare Go against SQL.
- `favicon.validateRemoteURL`. Worth knowing, not worth a property: it
  mutates its argument, writing the canonicalized host back into
  `target.Host` (`fetcher.go:274`). That is load-bearing -- the fetch
  then goes to the normalized host -- but it is a single assignment on a
  URL the caller just built, and a property test would restate the line.
  Note it here so the next reader does not mistake the mutation for a
  bug and does not go looking for the test that covers it.

## Batch 1 -- Pure normalization and parsing

No infrastructure, fast, and the place to discover whether hegel fits the
repo before committing to the harder batches.

### `internal/icon` (`app_test.go`, `raster_test.go`)

`AppKey` idempotence -- `AppKey(AppKey(s)) == AppKey(s)` over full
Unicode text. This is a contract, not a nicety: `Validate` rejects any
key that is not its own `AppKey`, so a non-idempotent `AppKey` would
produce keys the validator refuses.

`AppKey` output shape -- no leading, trailing, or repeated whitespace; no
uppercase. Draw text with the whitespace categories included.

`Validate` accepts what `AppKey` produces -- for any name where
`AppKey(name)` is non-empty and within `MaxKeyBytes`, `Validate` with
valid PNG bytes succeeds. Note that `strings.ToLower` can *lengthen* a
string (U+0130 lowercases to two runes), which is why `db.AppIconKeys`
re-checks the length; the property should confirm that guard is the only
one needed.

`ValidatePNG` never panics on arbitrary bytes -- `hegel.Binary(0, -1)`.
The universal claim is only the absence of a panic; it sits behind an
HTTP upload, so a crash on hostile bytes is the failure that matters.
Do not also require an error: arbitrary bytes can be a valid small PNG,
and hegel will eventually produce one. Assert the error only for inputs
the property itself puts outside the contract -- oversized, wrong magic,
out-of-range dimensions -- and for everything else assert the weaker
disjunction, that the call either returns `Details` with no error or an
error wrapping `ErrInvalid`, never a panic and never a bare error.

`Normalize` postconditions -- for any decodable source within the
dimension and pixel limits, the output passes `ValidatePNG` and is
exactly `NormalizedDimension` square. Build sources with a composite
generator: draw width and height in `[1, 1024]` subject to the pixel cap,
fill with drawn RGBA, encode as PNG.

`Normalize` on arbitrary bytes -- never panics; decode failures come back
as errors. This is the closest thing to a fuzz target in the repo and the
input is attacker-supplied.

`Normalize` idempotence on its own output -- worth *checking*, not
asserting blind. A 64x64 CatmullRom resample to 64x64 may not be
byte-identical. If it is not, weaken to dimension and validity stability
and record why.

### `internal/identity` (`identity_test.go`)

`New` always produces a `Valid` id, with version 4 and the RFC 4122
variant.

`Derive` always produces a `Valid` id, and is deterministic for the same
producer and parts.

`Derive` is injective in its parts. Expect this one to fail:
`Derive(p, "a", "b")` and `Derive(p, "a\x00b")` hash the same bytes,
because `Derive` joins with `\x00` and puts no length or count in the
digest. `TestDeriveSeparatesParts` only checks `("ab","c")` vs
`("a","bc")`, which the separator does handle. Today's callers pass URLs
and RFC 3339 timestamps, so no real collision is reachable -- confirm
that before deciding whether to fix the hash input or narrow the property
with a comment saying why.

`Valid` implies canonical -- `Valid(id)` means `id == strings.ToLower(id)`
and `len(id) == 36`. The doc comment says uppercase is rejected
deliberately; make it enforceable.

### `internal/db` (`site_test.go`)

`SiteFromURL` never panics on arbitrary text.

`SiteFromURL` output shape -- when ok, the result contains no `@`, no
uppercase, and no trailing dot. Note what is *not* true: the result can
still start with `www.` (only one prefix is stripped, so
`www.www.example.com` yields `www.example.com`). Assert what the code
promises, not what the name suggests.

`SiteFromURL` round-trips a constructed URL -- for a host drawn from
`hegel.Domains()`, `SiteFromURL("https://" + host + "/")` returns the
lowercased host with any `www.` prefix removed.

### `internal/favicon` (`fetcher_test.go`)

`CanonicalSite` idempotence -- a successful result is itself a valid
input producing the same output. It is a cache key; two spellings of one
site would mean two cache entries and two fetches.

`CanonicalSite` output shape -- when it succeeds, every label passes
`validDNSLabel`, the result is lowercase ASCII, and it is not an IP
literal. Draw full Unicode so IDNA paths are exercised.

`CanonicalSite` never panics -- arbitrary text, including strings that
look like URLs and IPv6 literals.

### `internal/tracker` (`window_linux_test.go`, `idle_linux_test.go`)

`parseWMClass` never returns an empty string for any input -- it falls
back to `unknownApp`, and an empty app name would produce a record the
server would then have to reject.

`parseIdleMs` round-trips a decimal integer. It is `strconv.ParseInt`
plus a multiply (`idle_linux.go:42-48`), so a negative result is not a
bug -- `"-1"` parses to `-1ms` by construction. Two properties hold
plainly: it never panics on arbitrary text, and it errors exactly when
the trimmed input is not a base-10 `int64`.

The third one needs an oracle that is not the implementation. Asserting
`result == ms * time.Millisecond` restates the function's own expression,
including its overflow, so it passes by construction at exactly the
inputs worth asking about -- above roughly 9.2e12 ms (about 292 years)
`time.Duration(ms) * time.Millisecond` wraps, and both sides wrap
identically. Instead: draw `ms` across the full `int64` range, feed
`strconv.FormatInt(ms, 10)` as the input, and compute the expected
nanoseconds with `math/big`. Assert that the exact product fits in
`int64` and that the returned duration equals it.

Written that way the property **fails** on overflow rather than
accommodating it, which is the point -- it forces the decision instead of
documenting a wrap nobody chose. Triage when it does: either
`parseIdleMs` should reject an out-of-range magnitude (it already returns
an error, so there is somewhere to put it), or the domain is genuinely
bounded by what `xprintidle` emits, in which case narrow the draw and say
so in a comment naming the real bound. Do not silently assert the wrap.

Whether the caller should reject a negative idle time is a separate
question for `IdleTime`, not for the parser.

### `internal/server` (`templates_test.go`)

`humanDuration` on the full `int64` domain. `time.Duration(seconds) *
time.Second` overflows above roughly 9.2e9 seconds, and the function
takes an `int64` straight from a SQL sum. Expect either a wrapped
negative rendering as `"-..."` or a nonsense result at the boundary.
Investigate before constraining -- the fix may be a clamp in the
function, since the type says it accepts any `int64`.

## Batch 2 -- Deduplication

This is where the value is. `internal/db/activity_dedup.go` is 460 lines
of interval arithmetic with two independent paths to the same number.
Add to `activity_dedup_test.go`.

**`mergeActivityIntervals` preserves the union.** Draw a list of
intervals; assert the output is sorted, pairwise disjoint, non-adjacent,
and covers exactly the same set of instants as the input. Model the union
as a sorted set of boundary points computed by a naive O(n^2) reference
-- that is the oracle, and it is fine for it to be slow. Per the
convention note above, these four facets share one test function.

Non-adjacency is a deliberate contract, not a universal truth about merge
functions, and the property should assert it as such. The merge condition
is `interval.start.After(previous.end)` (`activity_dedup.go:364`), so an
interval starting exactly where the previous one ended is absorbed rather
than kept separate. That choice is what makes a browser reporting
back-to-back one-second tabs produce one coverage span instead of
thousands, and `TestDeduplicateFirefoxActivityKeepsTouchingIntervals`
already asserts it for one pair. Generate ties deliberately -- draw
timestamps from a small pool so exact `start == end` coincidences occur
often, rather than hoping a wide draw produces them.

**Build coverage canonically, not by drawing it.** Both functions below
take coverage as a precondition, not as arbitrary input, and a generator
that ignores that will report failures the production code cannot
experience. `visitUncoveredActivityIntervals` binary-searches its
coverage slice (`activity_dedup.go:313`) and so assumes it is sorted and
merged: hand it `[5,10]` before `[0,6]` and it reports a gap that is not
there. `visibleUncoveredDuration` additionally indexes
`visibleGapPrefix` (`activity_dedup.go:289`), which only
`newActivityDeduplicator` builds -- pass merged intervals with a nil
prefix and two or more of them in range, and it panics on the index
rather than answering.

Write one helper that draws raw intervals, runs them through
`mergeActivityIntervals`, and builds the matching `visibleGapPrefix` the
same way `newActivityDeduplicator` does (`activity_dedup.go:51-66`), and
have every property in this section take its coverage from that helper.
The raw-input case is already covered by the merge property above; these
two are about what happens *downstream* of it. If the prefix construction
is worth exercising independently, promote it out of
`newActivityDeduplicator` into a named function and give it its own
property -- do not test it by accident through a panic.

**`visitUncoveredActivityIntervals` partitions the subject.** When it
returns `true` (work limit not hit), the visited intervals are disjoint,
ascending, contained in the subject, and their union is exactly the
subject minus the coverage union. The cursor advance in that loop is the
subtlest code in the file.

**`visitUncoveredActivityIntervals` respects the work limit.** It never
exceeds `workLimit` increments, and returns `false` exactly when it
stopped early. This bound is a denial-of-service guard against
adversarial overlaps; it should be checked against inputs designed to be
adversarial, which is what a generator does for free.

**`visibleUncoveredDuration` agrees with materialized slices.** The
oracle test, and the highest-value property here. For a drawn subject
interval and drawn coverage, the binary-search-plus-prefix-sum result must
equal the sum of the slices `visitUncoveredActivityIntervals` yields,
keeping only those at least `minEffectiveSliceDuration` long. Two
implementations of one quantity, both already in the file, neither
currently compared.

**`boundedActivityRecords` is a top-k.** A stateful model test:
`RuleAdd` pushes a drawn record into both the heap and a plain slice.
Invariant: `sorted()` equals the first `limit` elements of the model
sorted by `compareActivityRecords`, and `truncated` is true exactly when
an add was rejected -- that is, when more records have been added than
`limit` admits.

State it that way rather than as "or `limit` is zero". A limiter built
with zero (or a negative limit, normalized to zero by `max(0, limit)` at
`activity_dedup.go:415`) starts with `truncated == false` and only sets
it inside `add` (`activity_dedup.go:421-423`). Since `RunStateful` checks
invariants against the freshly built machine before any rule runs, the
zero-limit phrasing fails on the initial check for a machine that is
behaving correctly. `addCount > limit` is right at every limit including
zero, and it is falsifiable in the direction that matters: a limiter that
silently drops a record without setting the flag.

The generator decides whether this property can fail at all. The
comparator is three-level -- `StartedAt`, then `DeviceID`, then `ID`
(`activity_dedup.go:375-383`); `AppName` is not part of it. Records with
distinct `StartedAt` values exercise only the first level and would let a
broken tiebreak pass. Draw `StartedAt` from a small pool of instants and
`DeviceID` from two or three devices, with `ID` unique, so ties at the
first and second levels are common and the second and third comparisons
actually decide the order.

**`categoryTotals` partitions exactly.** The comment claims largest
remainder makes category seconds exactly partition the rounded
application seconds. Watch the `for i := range remaining` loop -- if
`remaining` can exceed `len(remainders)` it indexes out of range, and a
generator over many destinations with fractional durations is how that
gets found.

The per-application claim is not observable from the return value.
`categoryTotals` accumulates into `secondsByDestination` across every
application (`activity_dedup.go:213-243`) and returns one row per
category, so once two applications share a category their allocations are
summed and cannot be separated again. Two ways to make the property
implementable, and the plan takes both:

- **Single-application cases.** Draw records that all canonicalize to one
  app name, varying only the category destinations and the durations.
  The returned rows then *are* that application's allocation, and the
  partition claim is directly assertable. This is the tighter test and
  the one that would localize a largest-remainder bug.
- **Aggregate cases.** With many applications, assert the global
  identity instead: the sum of all returned category seconds equals the
  sum over applications of `round(applicationDuration)`, computed by an
  independent model built from `visitEffectiveDurations`. Weaker, but it
  is the property that actually holds of the published numbers, and it
  covers the cross-application interaction the single-app case cannot.

Both share the duration bound below.

Bound the durations, and say why in the test. `remaining` is computed via
`applicationDuration.Seconds()` (`activity_dedup.go:232`), a `float64`
whose mantissa stops representing nanosecond-exact values past 2^53 ns,
about 104 days of accumulated activity for one application. Beyond that
the rounding is approximate and the partition property fails for a reason
that says nothing about the largest-remainder logic.

The bound has to be on the **aggregate**, not on each record. Every
generated record can canonicalize to one application -- in the
single-application case they all do by construction -- so a per-record
cap with an unbounded list length caps nothing that matters. Bound both:
draw the record count in `[0, 200]` and per-record durations at up to an
hour, which puts the worst-case accumulation at about 8.3 days, more than
an order of magnitude under the 104-day ceiling. Drawing a total budget
and splitting it across records is the equivalent alternative; either
way the invariant the generator must maintain is
`count * maxRecordDuration << 2^53 ns`, and it belongs in a comment
beside the draw so a later widening of one bound does not quietly void
the other.

This is the one legitimate generator bound in the batch, and it is the
"prevents my test from producing meaningless input" kind, not a claim
about the function's domain -- note that the ceiling exists and is
unreachable for a per-window aggregate, since the query windows are days
and weeks.

**`totals` agrees with `timeline`.** With a record limit high enough that
nothing truncates, summing `timeline`'s slices per canonical app name
equals `totals` for the same window, up to the sub-second slices
`timeline` drops. State that tolerance explicitly rather than fudging it.

## Batch 3 -- Timeline geometry, limiters, SQL parity

### `internal/server/timeline.go`

**`chartWindow` invariants** (`timeline_test.go`). For any records and
any day, including DST days in a real zone: `dayStart <= start < end <=
dayEnd`; the window contains every record's clamped extent; and the span
is at least `min(minChartWindow, dayEnd - dayStart)`. The three sequential
clamps at the end of that function are exactly the shape of code that
holds for the cases someone thought of.

Include the empty and the no-overlap cases in the same property rather
than excluding them -- draw the record list without a minimum size, and
draw records that fall outside the day. Both take the `recordExtent`
`ok == false` path, which returns the **whole day**, not a minimum
window (`timeline.go:114-119`). The invariants above all hold for it: a
23-to-25-hour span satisfies the `minChartWindow` floor, and the "contains
every clamped extent" clause is vacuous when nothing overlaps. That is
the point of stating them as one property -- the fallback needs no
special case, and if a future change gives it one, the property notices.

**`mergeAdjacentActivity` conserves covered time**
(`timeline_merge_test.go`). Per device and app, the union of merged
intervals equals the union of the input intervals, and no merged bar
bridges a gap. `TestMergeAdjacentActivityPreservesCoveredTime` is the
single-example version of this -- evolve it rather than adding beside it.

**`activeHourScale` preserves widths** (`timeline_scale_test.go`). The
central claim of the compressed axis: for any record inside the window,
`minutesAt(end) - minutesAt(start)` equals its duration in minutes.
Separately, `minutesAt` is monotonic non-decreasing in `t`. Draw records
that leave real gaps -- a scale is only built when some hour is empty and
some hour is not.

**`toBar` stays inside the chart.** For any record and window, a returned
bar has `X >= 0`, `Width > 0`, and `X + Width <= span`. The minimum-width
floor and the trailing clamp interact, and that interaction is unchecked.

### Limiters

**`attemptLimiter`** (`middleware_test.go`) and **`slidingWindowLimiter`**
(`app_icons_test.go`) as stateful model tests over a virtual clock.
Rules: reserve, release/refund, reset, advance time by a drawn duration.
Model: a multiset of reservation timestamps per key. Invariant: the
number of reservations inside the window never exceeds `limit`, and a
refund of a genuine reservation restores exactly one slot.

Two behaviors to pin down rather than assume:
`slidingWindowLimiter.refund` matches on timestamp equality, so two
reservations at the same instant are interchangeable and a refund of an
already-swept reservation is a no-op. Decide which is contract and which
is accident before writing the invariant.

These guard a login throttle and an upload quota, which is reason enough
to want inputs nobody hand-picked.

### `SiteFromURL` against SQL (`site_test.go`)

`TestSiteFromURLMatchesSQL` already runs both derivations over thirteen
shared examples. Generating the URLs is the natural extension, and the
one place a database-backed property test earns its cost -- the whole
point of the Go port is that it must not drift from `siteExpr`.

Constraints specific to this test: it uses `testPool(t)`, so it skips
without Postgres and must not call `t.Parallel()`. Each case is a
round trip, so drop to roughly `hegel.WithTestCases(200)` and accept the
runtime -- and note that CI sets `TRACKKR_TEST_DSN` (`ci.yml:159`), so
unlike a bare developer machine it will not skip. Those 200 round trips
run on every push; measure them on the trial branch and lower the count
if the job notices. Generate URLs compositionally -- scheme, optional userinfo, host
or bracketed IPv6 literal, optional port, path -- rather than from raw
text, which would almost never match the authority pattern and would
spend every case on the drop path. Mix in a minority of raw `Text()`
draws to cover that path deliberately.

## Sequencing

0. **Amend `AGENTS.md`.** The generative-testing exception to the
   standard-library rule, and the coverage figure corrected from 50% to
   the 70% CI enforces. A docs commit, and the only true prerequisite --
   without it batch 1 contradicts the project's own instructions.
1. **Batch 1, `internal/icon` only.** One package, five properties.
   Settles the runtime question, whether `mise run lint` has opinions
   about the new code, and -- incidentally, not as a gate -- that the
   runner materializes the vendored library without complaint.
2. **Rest of batch 1.** Normalization and parsing across the four other
   packages.
3. **Batch 2.** Deduplication, in the order listed --
   `mergeActivityIntervals` first because everything else builds on it.
4. **Batch 3.** Timeline geometry, then the limiters, then the SQL
   parity test last since it is the only one with an infrastructure
   dependency.

Each batch is its own commit, `test:` prefixed. Failures found along the
way are separate commits with their own prefix, and a property test that
fails on real behavior gets triaged three ways before anything changes:
real bug, unsound property, or an input genuinely outside the function's
domain. The third is the rarest and needs the comment.

## Expected findings

Recorded now so the batches can be judged against a prediction rather
than in hindsight:

- `identity.Derive` collides on `("a","b")` versus `("a\x00b")`.
  Unreachable from today's callers; the question is whether to fix the
  digest input anyway.
- `humanDuration` misbehaves past `math.MaxInt64 / 1e9` seconds, and
  `parseIdleMs` wraps past `math.MaxInt64 / 1e6` milliseconds. Both are
  unreachable from real data and both have signatures that accept the
  input anyway. Expect the argument about each to be about the contract,
  not the arithmetic.
- `categoryTotals` largest-remainder loop is the most likely place for a
  genuine index-out-of-range.
- `chartWindow`'s sequential clamps and `toBar`'s width floor are the
  most likely places for a genuine off-by-one against real inputs.

If batches 1 and 2 turn up nothing beyond the first two, that is still a
result: the invariants become executable and the next refactor of
`activity_dedup.go` has a net under it.
