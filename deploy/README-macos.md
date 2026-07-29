# macOS Installation

The daemon runs as a background-only app so macOS can attach Accessibility
permission to a stable bundle path and signing identity.

## Install

From the repository root:

```sh
mise bundle-macos
```

This builds and installs `~/Applications/trackkr.app`, creates the config and
log directories, and writes
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

## Accessibility

App names and idle time need no permission. Window titles require
Accessibility access.

For a manual grant, open System Settings, go to Privacy & Security,
Accessibility, select the `+` button, and choose
`~/Applications/trackkr.app`. The app does not appear in this list on its own
unless it has requested access, so use the `+` button when prompting is off.

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

Ad-hoc signing changes identity when the binary changes, which can require a
new grant after every rebuild. For a stable local identity, create a
self-signed Code Signing certificate in the login keychain and install with:

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

Standard output is written to `~/Library/Logs/trackkr/daemon.log`; errors are
written to `~/Library/Logs/trackkr/daemon.err.log`.

## Detection Behavior

The daemon records the owner of the frontmost visible layer-zero window, not
necessarily the application that currently owns the menu bar. If the focused
application has no open windows, activity is attributed to the visible window
behind it. Lock-screen and screensaver overlays are also above layer zero, so
the daemon relies on the idle threshold to end activity while the screen is
locked rather than claiming a separate lock-state signal.
