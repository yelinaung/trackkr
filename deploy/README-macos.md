# macOS Installation

The daemon runs as a background-only app so macOS can attach Accessibility
permission to a stable bundle path and signing identity.

## Install

From the repository root:

```sh
mise bundle-macos
```

The command builds and installs `~/Applications/trackkr.app`, creates the
config and log directories, and writes
`~/Library/LaunchAgents/com.trackkr.daemon.plist`. It does not create or
overwrite the config file.

Create `~/Library/Application Support/trackkr/config.toml` with at least:

```toml
server_url = "https://trackkr.example.com"
api_key = "replace-with-device-api-key"

macos_read_titles = true
macos_prompt_for_accessibility = false
```

Set `macos_read_titles = false` to record application names without touching
the Accessibility APIs. Set `macos_prompt_for_accessibility = true` to ask for
permission once on the first denied check; the default avoids an unexpected
system dialog from a background agent.

## Application Icons

The daemon derives the installed application icon from the same process ID as
the frontmost layer-zero window. AppKit access needs neither Accessibility nor
Screen Recording permission, so icons continue working when title reads are
disabled or denied.

The daemon renders icons locally as bounded 64×64 PNGs and uploads them through
the existing reporter loop, after the activity records. They are presentation
metadata: the daemon keeps pending icons in memory instead of writing a second
durable queue. If the daemon exits before an upload, it derives the icon again
the next time it sees the application. The dashboard uses a colour-matched monogram until
an icon arrives, selecting black or white text from the generated background
colour so the fallback remains legible.

The daemon never fetches an application icon from the network. Site favicons
are a separate server feature: when the dashboard first displays a public site,
the server may fetch and cache its favicon for one year. The Firefox extension
receives no additional host permissions.

## Accessibility

App names and idle time need no permission. Window titles require
Accessibility access.

For a manual grant, open System Settings, go to Privacy & Security,
Accessibility, select the `+` button, and choose
`~/Applications/trackkr.app`. The app appears in that list only after it has
requested access, so use the `+` button when prompting is off.

Then restart the daemon:

```sh
launchctl kickstart -k gui/$UID/com.trackkr.daemon
```

The restart is required. macOS answers the trust question from the identity a
process launched with and never revises it, so a daemon that was already
running when you granted the permission keeps recording empty titles
indefinitely. It is not a delay you can wait out. The log says which state the
daemon came up in:

```text
INF Accessibility permission granted; window titles enabled
WRN Accessibility permission not granted; recording application names without titles
```

Revoking works differently and needs no restart: the Accessibility calls start
failing within a poll or two, and records keep their application names with
empty titles.

Do not check the permission by running the binary from a terminal. macOS
attributes a permission to the responsible process, so a binary launched from
a shell inherits the terminal's Accessibility grant and reports success no
matter what trackkr itself was given. Read `~/Library/Logs/trackkr/daemon.log`
instead.

Ad-hoc signing loses the grant on every rebuild. `mise bundle-macos` computes
an ad-hoc identity from the binary, so any code change produces a new one, and
a daemon that reported
`Accessibility permission granted` before the rebuild reports
`not granted` after it. macOS may still show trackkr as ticked, because the
list shows the path while the check matches the signature. Untick and re-tick
it, or remove it with `-` and add it again with `+`.

If you rebuild often, create a self-signed Code Signing certificate in the
login keychain once and install with a stable identity instead:

```sh
TRACKKR_SIGN_IDENTITY="trackkr local signing" mise bundle-macos
```

The value must match the certificate identity shown by
`security find-identity -v -p codesigning`.

## Launch

Load the per-user agent in the logged-in GUI session:

```sh
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.trackkr.daemon.plist
```

Unload it before reinstalling or changing its launch configuration:

```sh
launchctl bootout gui/$UID/com.trackkr.daemon
```

The agent writes standard output to `~/Library/Logs/trackkr/daemon.log` and
errors to `~/Library/Logs/trackkr/daemon.err.log`.

## Detection Behavior

The daemon records the owner of the frontmost visible layer-zero window, not
necessarily the application that currently owns the menu bar. If the focused
application has no open windows, the daemon attributes the activity to the
visible window behind it. Lock-screen and screensaver overlays sit above layer
zero too, so the daemon ends activity by the idle threshold while the screen is
locked instead of claiming a separate lock-state signal.

Sleep is a separate case, because a suspended machine runs no polls at all. The
daemon compares the wall clock against the time it has itself experienced, and
on a gap it closes the open segment at the last moment the machine was awake.
Work done after the lid opens starts a new segment at the time it happened.
