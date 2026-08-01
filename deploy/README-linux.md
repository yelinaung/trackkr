# Linux Installation

The daemon runs as a systemd user unit tied to the graphical session, so it
starts with the compositor and stops with it.

## Install

From the repository root:

```sh
mise run build-daemon
install -Dm755 trackkrd ~/.local/bin/trackkrd
install -Dm644 deploy/trackkrd.service ~/.config/systemd/user/trackkrd.service
```

Create `~/.config/trackkr/config.toml` with at least:

```toml
server_url = "https://trackkr.example.com"
api_key = "replace-with-device-api-key"
```

## Dependencies

Which binaries you need depends on the session, and nothing needs the ones
for the other kind.

| Session      | Window detection      | Idle detection |
| ------------ | --------------------- | -------------- |
| sway         | none (sway's own IPC) | `swayidle`     |
| X11, i3      | `xdotool`, `xprop`    | `xprintidle`   |
| other Wayland | unsupported          | `swayidle`     |

A missing idle binary is not fatal: the daemon logs what to install and
records activity without idle detection. A Wayland session never falls back
to `xprintidle`, which counts only the events XWayland itself receives and
would climb while you type in a native Wayland window.

## Session Environment

The unit sets no display variables. Import them from the session instead, so
the daemon gets the values that session actually has.

For sway, add to `~/.config/sway/config`:

```text
exec systemctl --user import-environment WAYLAND_DISPLAY SWAYSOCK XDG_CURRENT_DESKTOP
exec systemctl --user start sway-session.target
```

`import-environment` affects services started afterwards, which is why the
target starts on the line after it.

If your sway packaging does not already provide `sway-session.target` (the
`sway-systemd` project does), create
`~/.config/systemd/user/sway-session.target`:

```ini
[Unit]
Description=sway compositor session
Documentation=man:systemd.special(7)
BindsTo=graphical-session.target
Wants=graphical-session-pre.target
After=graphical-session-pre.target
```

Do not start `graphical-session.target` by hand. It is meant to be pulled in
by a session target that `BindsTo` it, which is what the unit above does.

On X11, import `DISPLAY` in place of the Wayland pair, from whatever starts
your session.

Then enable the daemon:

```sh
systemctl --user daemon-reload
systemctl --user enable trackkrd.service
```

It starts on your next login, or immediately with
`systemctl --user start trackkrd.service` if a session is already up.

## Recovering a Moved Socket

Sway names its IPC socket after its own PID, so a restarted compositor
usually listens somewhere new while the variables imported for the previous
session still name the old one. A daemon that outlives its compositor would
otherwise keep dialling a socket that is gone.

`PartOf=graphical-session.target` handles the ordinary case by stopping the
daemon with the session, so the replacement starts against a freshly imported
environment. Where that does not happen — a daemon restarted by
`Restart=on-failure` while the session moved underneath it — the daemon scans
`$XDG_RUNTIME_DIR` for a live socket and adopts it, at startup and while
running. The log says so:

```text
INF SWAYSOCK named a socket that is gone, adopting the live one
INF sway IPC socket moved, adopting the live one
```

It adopts only when exactly one live socket is there. Two mean a nested sway
or a second seat, and choosing wrongly would produce a complete, plausible,
entirely wrong timeline, so it stops instead. Candidates must also be owned
by you and answer sway's own IPC handshake.

Discovery needs `XDG_RUNTIME_DIR`. Without it there is nowhere safe to scan —
sway itself falls back to `/tmp`, which is world-writable — so the daemon
keeps the socket it was given and fails loudly if that one is dead.

## Idle Detection on Wayland

Wayland has no equivalent of the X screensaver extension: a client cannot ask
how long you have been idle, only to be told when a timeout it picks elapses.
The daemon supervises a `swayidle` child at its own `idle_threshold` and
turns the resulting idle and resume events back into a duration.

That child runs with `-C /dev/null`, which matters. swayidle treats
config-file events and command-line events as cumulative, so without it the
daemon's instance would inherit your own swayidle config and re-run every
command in it — locking the session a second time and suspending the machine
as a side effect of tracking.

`idle_threshold` is rounded up to whole seconds, since that is swayidle's
resolution. The log reports the effective value when it differs from what you
configured.

## Verifying

```sh
systemctl --user status trackkrd.service
journalctl --user -u trackkrd.service -f
```

Focus a native Wayland window and confirm the `app` field matches its
`app_id` in `swaymsg -t get_tree`. XWayland windows report their `WM_CLASS`
instead, which is the same name the X11 detector would give them, so an
application keeps one history across both kinds of session.

Stop typing for longer than `idle_threshold` and confirm `entered idle state`,
then `resumed from idle` on the next keypress. Check that no second screen
lock or suspend fires at that moment: that is what `-C /dev/null` prevents,
and a live session is the only place it shows.

## Sleep

A suspended machine runs no polls at all. The daemon compares the wall clock
against the time it has itself experienced, and on a gap it closes the open
segment at the last moment the machine was awake. Work done after the lid
opens starts a new segment at the time it happened.
