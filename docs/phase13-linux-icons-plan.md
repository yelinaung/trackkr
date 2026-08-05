# Phase 13: Linux Application Icons

## Context

Every application on a Linux device renders as a monogram chip. Not
some of them, and not the ones with unusual names -- all of them, on
sway and on X11 alike.

The break is one field. Icons reach the server only when the daemon
fills `WindowInfo.AppIcon`, and `maybeEnqueueAppIcon` returns
immediately when it is nil, so nothing is hashed, validated, queued, or
posted:

```go
// internal/tracker/app_icon.go:222
func (t *Tracker) maybeEnqueueAppIcon(info WindowInfo, now time.Time) {
	if info.AppIcon == nil || t.reporter == nil {
		return
	}
```

Only `detectorCore.ActiveWindow` sets that field, and only when its
`iconFor` hook is present. The single non-test caller of
`newAppIconCache` is `window_darwin.go:78`, behind `darwin && cgo`.
Both Linux detectors bypass `detectorCore` entirely and return a bare
literal:

```go
// sway_linux.go:158
return WindowInfo{AppName: swayAppName(node), Title: strings.TrimSpace(node.Name)}, nil

// window_linux.go:95
return WindowInfo{AppName: appName, Title: title}, nil
```

So the cache, the PID-keyed dedup, the worker queue, the upload
endpoint, the storage and the dashboard delivery all work and are all
unreachable from Linux. This is not a regression. Phase 6 named it:

> - Linux application icons or freedesktop desktop-entry resolution.

listed under Non-Goals, because macOS hands over a frontmost PID and
AppKit renders a bundle icon directly, which made it the cheaper first
slice.

### What the freedesktop side actually offers

The rest of this plan rests on measurements from a live sway session
rather than on what the specifications permit.

Five open windows, compared against every desktop entry on
`XDG_DATA_HOME` plus `XDG_DATA_DIRS`:

```text
app_id                                              exact  case-insensitive
firefox-beta                                        yes    yes
Slack                                               no     yes
brave-browser                                       yes    yes
com.mitchellh.ghostty                               yes    yes
org.telegram.desktop._42ce2b9c5259...               yes    yes
```

Every one resolves, and the single miss is a capital letter. That
matters because `icon.AppKey` already lowercases and collapses
whitespace, so the key the daemon computes today -- `slack` -- is
already the right lookup key.

Across 304 desktop IDs, 265 declare an icon. Where that icon lives,
and in what format, decides how much of the specification this phase
has to implement. The rows are disjoint and sum to 304:

```text
  116  PNG in hicolor or /usr/share/pixmaps      reachable
   51  PNG, but only in a theme other than hicolor   reachable
   18  Icon= is an absolute path to a PNG        reachable
    8  Icon= is an absolute path to SVG or XPM   not this phase
   55  SVG or XPM only                           not this phase
   12  no icon file anywhere                     unreachable
    5  absolute path, file missing               unreachable
   39  no Icon= key
```

Reachable is `116 + 51 + 18 = 185` of the 265 that declare an icon, or
**70%**. Dropping the theme chain and searching `hicolor` plus
`/usr/share/pixmaps` alone gives `116 + 18 = 134`, or **51%**. Adding
an SVG and XPM decoder later would reach `185 + 8 + 55 = 248`, or
**94%**.

The absolute-path row is split because it has to be: four of those
paths end in `.svg` and four in `.xpm`, so counting all 26 existing
absolute paths as reachable -- as an earlier draft of this table did
-- overstated the figure by three points.

The 51 in the second row are the reason this phase cannot take the
shortcut of searching hicolor alone. They are not obscure: the icon
theme configured on this machine is Yaru, and Yaru holds 482 of the
PNGs that hicolor does not. Ignoring the configured theme would leave
a fifth of installed applications on monograms while their artwork sat
on disk.

Searching every theme instead is worse, not better. 564 of 2134 PNG
stems exist in more than one theme -- `steam`, `chromium-browser`,
`filezilla` and 561 others appear in Yaru, gnome, HighContrast and
hicolor at once -- so a flat name-to-path index picks arbitrary
artwork, and could hand a user the HighContrast variant of one
application beside the Yaru variant of the next.

## Decisions

### The identity is the key that already exists

No new identity is introduced. `icon.AppKey(appName)` produces the
lookup key, the upload key, the storage key and the dashboard query
key, exactly as it does on macOS. A resolver that agrees with
`AppKey` cannot drift from the row the dashboard later reads.

### Entries are keyed by desktop file ID, and Hidden masks

The lookup key is the desktop file ID from the naming specification,
not the basename: `applications/screensavers/swirl.desktop` has the ID
`screensavers-swirl`, with the separator turned into a dash. 22
entries here are nested, so a basename index simply cannot see them.

The two visibility keys mean different things and get different
handling:

- `NoDisplay=true` means "this application exists but does not belong
  in a menu". Its icon is perfectly valid. 106 entries here set it
  while declaring an icon, and skipping them -- as an earlier draft of
  this plan did -- would discard 40% of the installed base for no
  reason.
- `Hidden=true` means the entry is deleted. It must mask any
  lower-precedence entry with the same ID and stop the search, rather
  than falling through to the copy in `/usr/share`.

### X11 and sway both get a StartupWMClass index

`StartupWMClass` exists precisely to tie a window's `WM_CLASS` to a
desktop entry, and 67 entries here declare one. 34 of those values
reach an entry that no basename or ID match would find:
`jetbrains-pycharm` names an entry called
`jetbrains-pycharm-b3d5c3dd-9c56-4af1-9db4-569ed5551fdd`, and
`TelegramDesktop` names one suffixed with a content hash.

The resolver builds a second index over `StartupWMClass`, consulted
after the ID match fails and before giving up. It is case-insensitive
for the same reason the ID match is.

Both indexes are built from the same post-masking entry set, in one
pass. Masking at lookup time instead would leave a deleted entry
reachable through its class mapping: a user `Hidden=true` file with no
`StartupWMClass` of its own would suppress the ID while the masked
system entry kept answering to its `WM_CLASS`.

### Icon lookup follows the specification's theme chain

The search order is the one in the icon theme specification, and the
measurements above are why it is worth implementing rather than
approximating. Two things depart from it, both in size selection and
both argued under "Size selection" below: `Scale` folds into the
candidate size instead of gating eligibility, and near-miss sizes are
ranked upward rather than by absolute distance.

1. Base directories in fixed order: `$XDG_DATA_HOME/icons`, `~/.icons`
   for legacy installs, each `$XDG_DATA_DIRS/icons`, then
   `/usr/share/pixmaps` as the unthemed fallback.
2. Themes in order: the configured theme, then its `Inherits` chain
   depth-first, then `hicolor` as the guaranteed terminal. Yaru here
   declares `Inherits=Humanity,hicolor`.
3. Within a theme, only the subdirectories its `index.theme` lists,
   using each group's `Size`, `Scale`, `Type`, and for `Type=Scalable`
   the `MinSize` and `MaxSize`, or for `Type=Threshold` the
   `Threshold`.

Directory names are not parsed for size. Yaru's `[48x48/apps]` group
happens to declare `Size=48`, but nothing requires that agreement, and
`16x16@2x` is a 32-pixel icon whose group carries `Scale=2`. Reading
`index.theme` costs one small INI parse per theme and removes the
whole class of guess.

One parser serves both file types. `index.theme` and `.desktop` are
the same INI dialect, so the desktop entry parser is written once and
reused, including the `\s`, `\n`, `\t`, `\r` and `\\` unescaping that
the value-type specification requires -- without it an `Icon=` naming
a path with a space resolves to nothing.

### One theme can span several base directories

A theme is not a directory; it is a name that may have parts in
several base directories at once, which is how a user drops one
replacement icon into `~/.local/share/icons/Yaru` without copying the
theme.

Two rules follow, and they differ:

- Metadata comes from the **first** `index.theme` found in base
  directory order. Later copies are not merged, so a stale
  `Directories=` list in `/usr/share` cannot override the user's.
- Icon files are searched across **every** base directory that
  contains that theme, in the same order. A user override in
  `~/.local/share/icons/Yaru/64x64/apps` therefore wins over
  `/usr/share/icons/Yaru/64x64/apps` for the same subdirectory.

Merging metadata instead would let a subdirectory listed in one copy
and absent from another silently drop; searching only the first base
would make user overrides invisible.

### Getting the configured theme is the weakest link

An earlier draft read `gtk-icon-theme-name` from
`$XDG_CONFIG_HOME/gtk-3.0/settings.ini` and fell back to `hicolor`.
Tried against the machine this plan was written on:

```text
~/.config/gtk-4.0/settings.ini   exists, no icon-theme key
~/.config/gtk-3.0/settings.ini   exists, no icon-theme key
~/.gtkrc-2.0                     absent
/etc/gtk-3.0/settings.ini        gtk-icon-theme-name = Yaru
gsettings                        'Yaru'
```

That draft finds nothing and selects `hicolor` -- losing the 51
entries whose only PNG is in Yaru, which is the entire reason the
theme chain exists. A silent, total failure of the phase's main
mechanism, on the developer's own machine.

The cause is that GTK does not treat `settings.ini` as the source of
truth. It reads the desktop-wide setting from the Wayland settings
portal or XSettings first, and `settings.ini` is a fallback that many
systems never write.

So the theme name is resolved in explicit priority order, and every
step is testable:

1. `icon_theme` in trackkr's own config, or `TRACKKR_ICON_THEME`.
   Deterministic, needs no desktop integration, and is the documented
   escape hatch when the guesses below are wrong.
2. `gsettings get org.gnome.desktop.interface icon-theme`, when
   `gsettings` is on `PATH`. This is where the real answer lives on
   this machine and on most GTK desktops. It is optional, it is
   already the shape of dependency the daemon has with `xdotool` and
   `xprop`, and it runs once per index build rather than per poll.

    It runs under `exec.CommandContext` with a short deadline, like
    every other exec in `internal/tracker`. `gsettings` reads over
    D-Bus, which is the dependency step 4's existence lets this phase
    avoid taking directly, and a wedged session bus must not stall an
    index build. It also needs the package's
    `//nolint:gosec // nosemgrep // gitlab-advanced-sast-exclude`
    annotation with the `LookPath` justification, or it fails
    `mise run lint` and the SAST job.
3. `gtk-icon-theme-name` from, in order: `$XDG_CONFIG_HOME/gtk-4.0`,
   `$XDG_CONFIG_HOME/gtk-3.0`, each `$XDG_CONFIG_DIRS/gtk-4.0` and
   `gtk-3.0`, then `/etc/gtk-4.0` and `/etc/gtk-3.0` -- which are GTK
   system paths, not XDG ones, and are where this machine's answer
   sits -- then `~/.gtkrc-2.0`.
4. `hicolor`.

Querying the portal over D-Bus is out of scope. It is the only fully
correct answer, and it costs a D-Bus dependency and a session bus the
daemon may not have; the override at step 1 exists so that being wrong
is recoverable rather than permanent.

### PNG this phase; SVG stays on the monogram

63 entries -- 55 by name, 8 by absolute path -- have only vector or
XPM artwork, and they stay on monograms. Covering them means a
rasterizer: `oksvg` and `rasterx` are the usual pairing, and they
bring partial SVG support, a new dependency, and a new class of
untrusted-input parsing into a daemon that currently decodes nothing
but PNG.

The trade is 70% now for no new dependency against 94% later, and it
stays a self-contained follow-up because the resolver's contract does
not change: a vector candidate is simply not eligible, and the search
continues past it.

### Size selection deviates from the specification, deliberately

The image contract is Phase 6's, unchanged: a 64x64 canvas,
`icon.MaxPNGBytes` of 64 KiB, `icon.MaxDimension` of 128.

Two things depart from the specification here, and they are separate.

Directory eligibility keeps the specification's `Type` semantics, but
measures every group by `Size * Scale` rather than by `Size` at a
matching `Scale`:

```text
Fixed      Size * Scale == 64
Scalable   MinSize * Scale <= 64 <= MaxSize * Scale
Threshold  (Size - Threshold) * Scale <= 64 <= (Size + Threshold) * Scale
```

An exact match ends the search within that theme, as it should.

The specification instead rejects any group whose `Scale` differs from
the requested scale, so a toolkit asking for 64 at scale 1 never sees
`16x16@2x`. That rule is right for what it describes: a scale-2 file is
meant to fill a 16-point box on a HiDPI display, not to be drawn at 32
points. Trackkr has no display and no points. It resamples once to a
64-pixel raster and stores it, and `16x16@2x` is 32 physical pixels of
artwork on disk. Ignoring `Scale` as an eligibility gate and folding it
into the size is what "best available source" means in this pipeline.

Ranking the near misses is the second departure.
`DirectorySizeDistance` is symmetric, so given 48 and 128 for a 64
request it returns 16 and 64 and picks 48. This phase picks 128.

The reason is that trackkr is not choosing an icon to display at 64.
It is choosing a source to resample to exactly 64, once, and store
forever. Downscaling 128 to 64 keeps detail; upscaling 48 to 64
invents it, and the result is soft in a list where every neighbouring
icon is crisp. The specification's metric is right for a toolkit that
will blit the file at its native size, and wrong for a pipeline whose
next step is always `draw.CatmullRom`.

So: the smallest eligible candidate at or above 64 wins; when nothing
in the chain reaches 64, the largest below it is upscaled rather than
refused, because a soft icon still reads better than two letters in a
coloured square. The implementation notes both deviations where the
eligibility test and the comparison live, so the next reader does not
"fix" either back.

Theme order outranks size either way. A 48x48 from the configured
theme is preferred over a 256x256 from `hicolor`, because the user
chose the theme and did not choose the resolution.

### A bad file falls through to the next candidate

Resolution does not end at the first path that exists. A candidate is
only resolved once its bytes have been read within `MaxSourceBytes`
and normalized successfully; a truncated PNG, an oversized one, or a
file the daemon cannot open moves to the next candidate.

"Next" means next in the same order the search already established,
not next theme. A corrupt 64x64 in the configured theme is followed by
that theme's other eligible sizes, then by every base directory
holding that theme, and only then by the inherited themes and
`hicolor`. Skipping to the next theme on the first bad file would
discard the user's chosen artwork over one damaged size, which is the
precedence this phase went to the trouble of implementing.

Returning nil on the first bad file would let one corrupt icon in the
configured theme mask perfectly good artwork one inheritance step
away, and the failure would be invisible: a monogram looks the same
whether the search found nothing or gave up early.

### Reads are bounded before anything is decoded

`internal/favicon` bounds its source at `maxSourceBytes` of 256 KiB
*in the fetcher*, not in the normalizer:

```go
// internal/favicon/fetcher.go:263
data, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBytes+1))
```

Moving the normalizer without moving that limit would leave the daemon
reading an arbitrarily large file off disk and decoding it, with only
the dimension checks -- which run after the bytes are already in
memory -- standing in the way. A 900x900 PNG can be tens of megabytes.

So the limit travels with the code. `icon.Normalize` enforces
`MaxSourceBytes` on its input before touching `image.DecodeConfig`,
and the resolver reads candidate files through an `io.LimitReader`
rather than `os.ReadFile`.

The bound is `MaxSourceBytes+1`, followed by a `len(data) >
MaxSourceBytes` comparison -- exactly the fetcher's shape, and the
`+1` is the whole mechanism. A reader capped at `MaxSourceBytes`
cannot distinguish a file that is exactly at the limit from one over
it: a 300 KiB icon would arrive silently truncated to 256 KiB, pass
the length check, and then fail inside `image.DecodeConfig` on a
half-read PNG. The failure still ends in nil, so the icon is still
missing, but it is reported as corrupt artwork rather than an
oversized file, and the "refused before decode" test would fail.

### Base directories take the specification's defaults, and only absolute paths

The lists above are described in terms of `XDG_DATA_HOME`,
`XDG_DATA_DIRS` and `XDG_CONFIG_HOME`, all three of which are
routinely unset -- `XDG_DATA_HOME` most of all, because almost nothing
sets it and almost everything assumes it. Joining an empty string
naively yields a relative path, and a daemon started from any working
directory would then search that directory instead of the user's:

```text
XDG_DATA_HOME    unset or relative  ->  $HOME/.local/share
XDG_DATA_DIRS    unset or empty     ->  /usr/local/share:/usr/share
XDG_CONFIG_HOME  unset or relative  ->  $HOME/.config
```

The base directory specification also requires that a path in any of
these variables be absolute, and that a relative one be treated as
invalid and ignored. Entries within `XDG_DATA_DIRS` are filtered
individually, so one bad element does not discard the rest of the list.

`HOME` unset leaves nothing to fall back to; the resolver then
contributes no user-level directory rather than searching `/.config`.

An existing helper takes the looser reading: `DefaultDataDir` in
`internal/tracker/config.go:246` accepts any non-empty
`XDG_DATA_HOME`. That is out of scope here, but the two should not be
mistaken for one implementation.

### The resolver takes its roots as arguments

Phase 6 set a boundary for `internal/icon`:

> The package performs no HTTP, database, filesystem, or platform-native
> work.

That sentence lives in `docs/phase6-icons-plan.md`, not in the
package, whose doc comment says only "Package icon defines application
icon identity and image validation". Since this phase widens what the
package does, it also moves the constraint into
`internal/icon/app.go`'s package comment, where the next person to add
a file to it will actually read it.

The constraint is worth keeping, so the filesystem walk lives in
`internal/tracker` behind a `linux` build tag, not in `internal/icon`.

The resolver reads no environment variable of its own. Its constructor
takes the application directories, icon base directories and configured
theme name, and one small function derives them from the environment. A
test then builds a fake XDG tree with `t.TempDir()` and never touches
the real machine, which is the difference between a test that passes
everywhere and one that passes on the author's laptop.

### The rasterizer moves to internal/icon and is shared

`internal/favicon` already has exactly the normalizer this needs:
decode any format, scale to 64x64 with `draw.CatmullRom` onto a
transparent canvas, encode PNG, validate against the shared contract.

Duplicating it in the daemon would mean two normalizers whose output
must match but is checked nowhere. It moves to
`internal/icon/raster.go`, gains the byte limit above, and
`internal/favicon` calls it. This stays inside the package's stated
boundary -- decoding and scaling bytes is pure computation, no
different from the PNG validation already there -- and adds no
dependency, since `golang.org/x/image` is already direct in `go.mod`.

### Indexes are built once; the cache owns a goroutine

The daemon polls every few seconds for the life of the session, so the
entry index, the `StartupWMClass` index and the per-theme directory
lists are built on first use and held for `resolverIndexLifetime`. A
miss does not trigger a rebuild -- a newly installed application is not
worth a filesystem walk.

```go
// resolverIndexLifetime bounds how long a newly installed application
// keeps its monogram. A miss does not rebuild, so this constant alone
// decides that wait.
const resolverIndexLifetime = 15 * time.Minute
```

Fifteen minutes is chosen against what the walk costs and what the
wait costs. The walk visits 304 desktop entries and roughly 2100 PNGs
on this machine, which is milliseconds of `os.ReadDir` on a warm page
cache and happens at most four times an hour. The wait is a quarter of
an hour of monogram after installing something -- long enough to be
noticed, short enough that nobody files a bug, and it costs nothing to
lower later once the walk has been measured on a slower disk.

Per-application results keep using `appIconCache` unchanged: five
minutes positive, thirty seconds negative, thirty days before a
re-upload.

`appIconCacheKey` is `{PID, Key}`, and Linux passes `PID: 0` for every
window. That is deliberate, not an omission. Sway's tree does carry a
`pid`, so a reader will reasonably wonder whether to plumb it through;
they should not. The PID earns its place on macOS because resolution
goes through the running process's bundle, so a relaunch genuinely
needs re-resolving. Linux resolution is name-based and
PID-independent, so including the PID would re-walk the filesystem to
reach an identical answer every time an application restarts.

That cache starts a worker goroutine in its constructor and stops it
only in `Close`, so ownership has to be explicit. The daemon closes a
detector through a duck-typed `interface{ Close() }`
(`cmd/trackkrd/main.go:34`), which means:

- `SwayWindowDetector.Close` must close the cache as well as the IPC
  connection, and stay idempotent.
- `XWindowDetector` has no `Close` today and gains one, or its worker
  leaks for the life of the process.

## New Files

- `internal/tracker/icon_xdg_linux.go` -- INI parser, entry and
  `StartupWMClass` indexes, theme chain, directory lists, size
  selection, bounded reads.
- `internal/tracker/icon_xdg_linux_test.go`
- `internal/icon/raster.go` -- the shared normalizer, with
  `MaxSourceBytes`.
- `internal/icon/raster_test.go`

## Changed Files

- `internal/tracker/sway_linux.go` -- hold a resolver and cache, set
  `AppIcon`, close both.
- `internal/tracker/window_linux.go` -- same for `XWindowDetector`,
  which also gains `Close`; `NewWindowDetector` builds one resolver and
  hands it to whichever detector it returns.
- `internal/favicon/fetcher.go` -- call `icon.Normalize`, drop the local
  copy and its constants.
- `internal/icon/app.go` -- move Phase 6's no-filesystem constraint into
  the package doc comment.
- `internal/tracker/config.go` -- add the optional `icon_theme` field and
  its `TRACKKR_ICON_THEME` override.
- `internal/tracker/config_test.go` -- `clearTrackkrEnv` neutralises the
  four existing `TRACKKR_*` variables by name and has to gain the fifth,
  or every config test starts depending on the shell that ran it.
- `docs/phase6-icons-plan.md` -- strike the Non-Goals line this phase
  closes, pointing at this document.

## Steps

1. Move the normalizer into `internal/icon/raster.go` and add
   `MaxSourceBytes`. No behaviour change for favicons beyond the
   limit moving inward; the existing favicon tests must pass
   untouched, which is the proof of that.
2. Write the INI parser: group-scoped, first value wins, iconstring
   unescaping. It serves `.desktop` and `index.theme` both.
3. Derive the base directories, with the specification's defaults and
   the absolute-path filter.
4. Build the entry index in one post-masking pass: desktop file IDs,
   `Hidden` masking, `StartupWMClass` reverse index.
5. Resolve the theme name through the four-step priority order, then
   build the theme chain and per-theme directory lists from
   `index.theme`, with `DirectoryMatchesSize` eligibility.
6. Assemble the resolver: key in, `*icon.App` out, each candidate
   read, bounded and normalized before it counts as resolved, and the
   next candidate tried when one fails.
7. Wire it into both Linux detectors through `appIconCache`, with
   `Close` on both.
8. Verify on a real session against the applications below.

## Tests

Database-free, and parallel wherever the resolver's injected roots
allow it -- which is everywhere except the environment derivation
below, since `t.Setenv` panics in a parallel test.

- Entry lookup honours base directory order: the same ID in
  `XDG_DATA_HOME` and in `/usr/share` resolves to the former.
- Case-insensitive match: `Slack` finds `slack.desktop`. This is the
  one real-world miss, so it gets a named test.
- Nested entries: `applications/foo/bar.desktop` is found as
  `foo-bar`, not as `bar`.
- `Hidden=true` in a higher-precedence directory masks the entry in a
  lower one, and the lookup returns nil rather than falling through.
- Masking precedes indexing: a `Hidden=true` user entry carrying no
  `StartupWMClass` still suppresses the masked system entry that does,
  so the class mapping cannot resurrect it.
- `NoDisplay=true` resolves normally.
- `StartupWMClass` finds an entry whose ID does not match the key, and
  loses to an exact ID match when both exist.
- Parser: values before `[Desktop Entry]` are ignored, a later
  `[Desktop Action foo]` group cannot supply `Icon=`, a duplicate key
  keeps the first, and `Icon=/opt/My\sApp/icon.png` unescapes to a path
  with a space that resolves.
- Theme chain: the configured theme wins over `hicolor` even at a
  smaller size; `Inherits` is followed depth-first; a cycle in
  `Inherits` terminates.
- `icon_theme` precedence, non-parallel with `t.Setenv` after
  `clearTrackkrEnv`: the file value is read, `TRACKKR_ICON_THEME`
  overrides it, and an empty variable does not blank a configured
  value -- the same shape as the four overrides already there.
- Theme name resolution, in priority order and each independently: the
  config override beats `gsettings`; `gsettings` beats every file; the
  file search finds `/etc/gtk-3.0/settings.ini` when neither
  `$XDG_CONFIG_HOME/gtk-4.0` nor `gtk-3.0` carries the key -- the case
  that would have selected `hicolor` on the author's machine -- and
  `hicolor` is the terminal fallback. `gsettings` is exercised through
  an injected lookup, not by requiring the binary.
- One theme across two base directories: `index.theme` in
  `/usr/share/icons/T` supplies the metadata while a file in
  `~/.local/share/icons/T` wins for the same subdirectory, and a
  second `index.theme` in the user directory does not merge its
  `Directories=` into the first.
- Directory metadata drives size, not directory names: a group named
  `[huge/apps]` carrying `Size=64` is selected for 64, and
  `16x16@2x` with `Scale=2` is treated as 32 rather than 16.
- Eligibility by `Type`: a `Scalable` group with `MinSize=8` and
  `MaxSize=512` is a candidate for 64; a `Threshold` group with
  `Size=48` and `Threshold=2` is not.
- The scale deviation, asserted directly: a group with `Size=32` and
  `Scale=2` is eligible for 64 and beats a `Size=48`, `Scale=1` group
  in the same theme. The specification would reject it outright, so
  this is where reverting to a `Scale`-must-match gate fails.
- Size selection: 64 wins outright when present; with only 48 and 128,
  128 wins. The second case is the documented ranking deviation from
  `DirectorySizeDistance`, which would pick 48, and it is asserted so
  that reverting it fails loudly rather than quietly changing what
  every Linux user sees.
- 48 wins when nothing in the chain reaches 64.
- `Icon=` absolute path is used directly; an absolute path to a missing
  file yields nil rather than an error.
- Bounded reads: a 300 KiB PNG whose dimensions are otherwise valid is
  refused before decode, and the refusal is not cached as a permanent
  failure any differently from other misses.
- Ordered fallback, three candidates: a corrupt 64x64 in the
  configured theme, a valid 128x128 in the same theme, and a valid
  64x64 in `hicolor`. The same-theme 128 must win. A test with only
  two candidates cannot tell correct fallback from a jump straight to
  `hicolor`.
- Only when every candidate in every theme fails does the resolver
  return nil.
- Unreadable candidates are simulated with a directory at the
  candidate path and with a dangling symlink, never with file
  permissions: a suite run as root reads a `0o000` file happily, and
  containers run as root often enough that the test would pass
  without exercising anything.
- Environment derivation, each in its own non-parallel test using
  `t.Setenv`: unset `XDG_DATA_HOME` falls back to `$HOME/.local/share`;
  unset `XDG_DATA_DIRS` falls back to `/usr/local/share:/usr/share`;
  unset `XDG_CONFIG_HOME` falls back to `$HOME/.config`; a relative
  value in any of them is ignored rather than joined; one relative
  element inside `XDG_DATA_DIRS` does not discard the absolute ones;
  and with `HOME` unset no user-level directory is contributed.
- Source bounds still reject a 2000x2000 PNG and a declared-small,
  actually-large one.
- Normalizer parity: `TestNormalizeImage`,
  `TestNormalizeImageAcceptsICO` and
  `TestNormalizeImageRejectsOversizedDimensions` pass against the
  moved function with no edit beyond the call site. There is no
  `internal/favicon/testdata` to compare against -- those tests build
  their images inline with `png.Encode` -- so they are the parity
  check rather than a fixture diff.
- Bounded reads distinguish at the boundary: a file of exactly
  `MaxSourceBytes` normalizes, and one a single byte larger is
  refused. Capping the reader at `MaxSourceBytes` instead of
  `MaxSourceBytes+1` passes the first and fails the second.
- Indexes are built once and then rebuilt: a resolver over a counting
  fake root reports one walk across many lookups, and a second walk
  once an injected clock passes `resolverIndexLifetime`. Without the
  second assertion a permanently stale index passes the suite.
- Lifecycle: `SwayWindowDetector.Close` and `XWindowDetector.Close`
  each stop the cache worker, are safe to call twice, and leave no
  goroutine behind under `-race`.
- Both detectors are wired, asserted separately: `SwayWindowDetector`
  and `XWindowDetector` each set `AppIcon` from a stub resolver, and
  each leave it nil when the resolver is nil. Testing only sway would
  let a missed assignment in `window_linux.go` ship green, which is
  the exact shape of the bug this phase exists to fix.

## Out of Scope

- SVG and XPM rasterization, and with it the 63 entries whose only
  artwork is vector.
- Icon theme scale preference beyond `Size * Scale`. The daemon
  produces one 64x64 raster; HiDPI variants of the same icon are a
  server-side and dashboard concern if they ever matter.
- `Context` filtering. Restricting to `Context=Applications` would be
  defensible, but the entry's `Icon=` name is already application
  specific, and no sampled case resolved to a mimetype icon.
- Per-window icons. Sway's tree carries none, and a title-bar icon is
  not what the dashboard shows.
- Windows, and any change to macOS acquisition.
- Any change to the upload endpoint, storage, pruning, or dashboard
  rendering. This phase fills one nil field; everything downstream is
  already built and already tested.

## Manual Verification

```bash
# Resolve what the running session would ask for.
swaymsg -t get_tree | rg -o '"app_id": "[^"]+"' | sort -u

# The configured theme the chain should start from, asked in the same
# priority order the resolver uses. The first non-empty answer wins;
# on the machine this plan was written on that is gsettings, and the
# only file carrying the key is the one under /etc.
echo "${TRACKKR_ICON_THEME:-unset}"
gsettings get org.gnome.desktop.interface icon-theme 2>/dev/null
rg -n gtk-icon-theme-name \
  ~/.config/gtk-4.0/settings.ini ~/.config/gtk-3.0/settings.ini \
  /etc/gtk-4.0/settings.ini /etc/gtk-3.0/settings.ini \
  ~/.gtkrc-2.0 2>/dev/null

# Checking only ~/.config/gtk-3.0/settings.ini is the mistake this
# plan's theme-name section exists to prevent: it is empty here, and a
# correct resolver would look like a broken one.

# After running the daemon for one poll interval:
rtk psql -c "select app_key, width, height, octet_length(png) \
  from app_icons order by updated_at desc limit 10"
```

The dashboard should show artwork for `firefox-beta`,
`com.mitchellh.ghostty`, `brave-browser` and `slack`, and keep the
monogram for `Alacritty` -- whose icon is SVG-only, and whose continued
monogram is this phase working as designed rather than failing.
