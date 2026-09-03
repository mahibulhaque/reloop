# repeat

A cron and one-shot job scheduler.

[![ci](https://img.shields.io/github/actions/workflow/status/mahibulhaque/repeat/ci.yml?branch=main&label=ci&style=for-the-badge)](https://github.com/mahibulhaque/repeat/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fmahibulhaque%2Frepeat%2Fbadges%2Fcoverage.json&style=for-the-badge)](https://github.com/mahibulhaque/repeat/actions/workflows/ci.yml)

It does two things:

- recurring (cron-style) jobs
- one-shot jobs at a wall-clock time

Both run inside its own daemon, so behaviour is the same on macOS and Linux. The system cron
isn't involved.

State lives in a single SQLite file under the platform's data directory. Captured output for
the last 100 runs per job is retained for 100 days.

## Why

I've been using LLM agents to schedule both one-off and recurring jobs. The agents I use
keep their job ticker inside their own process and there's little visibility into it. For
some tasks that's adequate, but for others I'd like to see all the scheduled jobs in one
place.

The system cron works, but it takes some shell-fu to check state and tail the output.
One-off scheduling is also inconsistent across platforms: on macOS, the `at` daemon is
usually off by default. So I wanted a small, self-documenting CLI that an LLM can drive and
that behaves the same on macOS and Linux.

## Quickstart

### Install

macOS:

```sh
brew tap mahibulhaque/repeat https://github.com/mahibulhaque/repeat
# Newer Homebrew requires trusting third-party taps.
brew trust mahibulhaque/repeat
brew install --cask repeat
```

Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mahibulhaque/repeat/main/install.sh | sh
```

From source:

```sh
go install github.com/mahibulhaque/repeat/cmd/repeat@latest
```

On systems without launchd or systemd (Alpine, WSL, containers),
skip `repeat install` and run the daemon in a loop:

```sh
mkdir -p ~/.local/share/repeat
nohup sh -c 'until repeat daemon; do sleep 1; done' >> ~/.local/share/repeat/repeat.log 2>&1 &
```

The loop restarts the daemon after a crash. `repeat stop` stops the
daemon and ends the loop.

A restart loses nothing. The schedule lives in SQLite. A missed
one-shot fires on the next start. A run that was killed mid-run is
recorded as interrupted. It is not retried, because the command may
have finished right before the crash.

To start the loop at boot, put the same line in your system's boot
hook, for example `/etc/local.d`, a runit service, or `wsl.conf`.

### Use

Register the daemon with launchd (macOS) or systemd --user (Linux) so it restarts across
logins and crashes:

```sh
# Register the supervisor unit once.
repeat install
```

Add recurring jobs. Everything after `--` is the command repeat will run:

```sh
# Record weekday disk space.
repeat add --cron '0 9 * * 1-5' --name disk-space -- sh -c 'date; df -h "$HOME"'

# Check a website every 15 minutes.
repeat add --cron '*/15 * * * *' --name homepage-check -- curl -fsS https://example.com
```

Add one-shot jobs:

```sh
# Run after a relative delay.
repeat add --at '+30m' --name stretch -- sh -c 'printf "stand up and stretch\n"'

# Run at a wall-clock time.
repeat add --at 'tomorrow 9am' --name morning-note -- sh -c 'printf "review calendar\n"'
```

List and inspect:

```sh
# List jobs in a compact table.
repeat ls

# Emit JSON for scripts.
repeat ls --json

# Show one job's schedule and state.
repeat show disk-space

# Read captured output after a run has completed.
repeat logs disk-space --lines 50

# Stream future completed runs.
repeat logs disk-space --follow

# Check daemon state, supervisor state, and job counts.
repeat status
```

Control the lifecycle:

```sh
# Pause a job without deleting it.
repeat disable disk-space

# Re-enable it.
repeat enable disk-space

# Delete a job and its run history.
repeat rm stretch

# Ask the daemon to exit.
repeat stop

# Purge done one-shots and disabled jobs.
repeat prune

# Remove the supervisor unit.
repeat uninstall
```

## Development

Requires Go 1.27 or newer.

```sh
make build
make test
make vet
make lint
make tidy
make clean
```

`make e2e` drives the real binary end to end. The architecture is
described in [docs/arch.md](docs/arch.md) and the test layout in
[docs/testing.md](docs/testing.md).

## Releases

Tagged `v*` pushes trigger a goreleaser build via `.github/workflows/release.yml`.
