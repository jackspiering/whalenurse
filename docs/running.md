# How whalenurse behaves

This is the detailed behaviour reference (what each rung does, the drift guard, sidecars, shutdown handling, the dashboard's paths). For how to install and run it see [deployment.md](deployment.md); every variable and label is in [configuration.md](configuration.md) and [labels.md](labels.md); [troubleshooting.md](troubleshooting.md) answers "why is it SUSPENDED?".

whalenurse watches Docker, and for opted-in containers that go unhealthy, crash-loop, or stop unexpectedly it runs rung 1 (a Docker API `restart` or `start`), then rung 2 (recreate from the stack's own compose files, behind a drift guard), then, for a container that opts in, rung 3 (pull a newer image and recreate), and it repairs `network_mode` sidecars that depend on a container it restarted or recreated. Around that ladder: a persisted state store, cooldowns, a flap limit, SUSPENDED and PAUSED, the CLI, shutdown handling, notifications and an optional read-only dashboard, each described below.

## Recreate: two read-only mounts, and the versions it needs

Rung 2 runs `docker compose -p <project> --project-directory <working_dir> -f <files> [--env-file <f>] up -d --force-recreate --no-deps --pull never --no-build <service>`, with every input read from the container's own compose labels. It never runs `down`, never pulls, never builds. In a container it needs:

1. **the host's compose plugin, mounted read-only at `/host/docker-compose`** (usually `/usr/libexec/docker/cli-plugins/docker-compose`; `docker info` shows it). Lookup order: `WHALENURSE_COMPOSE_BIN`, then `/host/docker-compose`, then the copy bundled in the image at `/usr/libexec/whalenurse/docker-compose`, then (under systemd) the host's plugin directories. The bundled copy is a fallback: when a container was made by another compose release, the drift guard cannot tell drift from a changed hash layout and says "can't verify drift (compose X on host vs Y in whalenurse)" instead of recreating.
2. **the stacks root, mounted read-only at the identical path as on the host** (`/opt/stacks:/opt/stacks:ro`). Compose passes host paths to the daemon for relative bind mounts, so mounting it elsewhere (`/opt/stacks:/stacks:ro`) makes the recreate fail. Also mount whatever else compose reads from outside it: `env_file:` paths with `../`, `include:` and `extends:` files, file-based `secrets:` and `configs:`. The directory and its parents must be traversable by uid 65532.

If the files or labels are missing or unreadable, rung 2 is skipped for that container with a message that says so (`RungUnavailable`) and the ladder ends with SUSPENDED after the restart.

**Versions.** Rung 1 works from Engine v25. Rungs 2 and 3 and the drift guard need Engine 28.1 or later and Compose v5.2.0 or later. A pre-release of a minimum (`28.1.0-rc.1`, `5.2.0-rc.1`) counts as below it. whalenurse checks both at startup (and again after the events stream comes back; an Engine version that could not be read is tried again at every poll): below that, or when a version cannot be read, or without any compose binary, it logs **one warning**, disables every use of compose, and the ladder ends with SUSPENDED after rung 1. `whalenurse status` shows the binary, its source, the versions and the disabled rungs.

## The drift guard

A recreate deploys what is on disk and in the local image store *now*, so it runs checks first (plan §3.6). `WHALENURSE_DRIFT_GUARD=strict` (default) skips the recreate on any finding, reports the cause (`DriftBlocked`) and SUSPENDS the container; `warn` recreates anyway and says so (`DriftWarned`) when the findings are verified drift (a compose edit or a moved tag), but still blocks on a finding that says "can't verify" (a check that could not be made, or the bundled compose's version skew). If the host plugin was upgraded since the container was created, the drift message says so ("compose upgraded from X to Y since this container was created"): the change may be the hash layout rather than an edit.

1. **Config drift**: compose's own `--dry-run --progress json up ...` says whether it would recreate the container (it covers a compose edit, an env change and a moved image tag; it has no side effects). If its output cannot be read, `config --hash` is compared with the container's `com.docker.compose.config-hash` label.
2. **Image drift**: the container's `com.docker.compose.image` label against what its image reference resolves to for `linux/<arch>` now (the platform image ID; correct on both image stores). A reference that is gone locally is a failure.
3. **Version skew**: only for the bundled compose, see above.

A container whose `network_mode`, `ipc`, `pid` or `volumes_from` names another container skips check 1 (compose hashes those after resolving them, so they never match) and gets checks 2 and 3.

## The pull rung (opt-in)

`whalenurse.heal.pull=true` appends rung 3 to the container's ladder, **after** restart and recreate (it runs only if both did not help, and only above the version gate: Engine 28.1 and Compose v5.2.0, like rung 2). It needs `recreate` in `whalenurse.actions` (the default has it): without it the pull stays off, with one warning in the log. It is the only thing in whalenurse that fetches an image. In this order:

0. The service's `pull_policy` is read (`compose config --format json --no-env-resolution <service>`; only that field is kept). **`never` and `build` are respected**: the rung does not run, and the container is SUSPENDED with "pull rung does not apply (pull_policy never)" as the cause, in the one giving-up message. A policy that cannot be read does the same.
1. The drift guard's checks 1 and 3 (a compose edit, the bundled compose's version skew) run **before** the pull. A tag that moved under the container (another stack pulled it) does not block: moving the tag is what a pull is for, but only when `config --hash` still matches, so an edit cannot hide behind it.
2. `docker compose ... pull --policy always <service>` (one service, no dependencies). `--policy always` replaces `missing`, `if_not_present`, the refresh windows and no policy at all: without it, `missing` (on compose v5.2.0 for every tag, and from v5.6.0 for any tag but `latest`) silently skips an image that is already present, and the rung would report "no newer image" for an image it never looked up.
3. What the image reference resolves to (the platform image ID, as in the drift guard) is read before and after. **Unchanged:** the container is SUSPENDED at once and the one "giving up" message says "the pull found no newer image for <ref>". **Changed:** `up -d --no-deps --pull never --no-build <service>` (not forced; if compose leaves the old container in place it is recreated with `--force-recreate`), then the dependents step, which recreates the `network_mode` sidecars against the new parent. A sidecar that runs the same image reference as its parent may take the tag this pull moved, in this incident, and only that; any other moved tag is image drift for it as before.
4. `TagMoved` is recorded (event, and the store's meta, so it survives a restart). Another stack that shares the tag will see image drift; when its recreate is blocked, the message names the container that moved the tag and when, as long as the tag still points where that pull put it.
5. The notification "**upgraded during heal**" gives old → new image (12 hex digits each) and the exact rollback, two lines to paste on the host: `docker image tag <old image id> <ref>` and `docker compose -p ... up -d --no-deps --pull never <service>` (the old image stays local until you prune it; sidecars on the same image are named in the same `up` line). The same message (and the same rollback) is sent when rung 2 of a `heal.pull=true` container takes a tag that someone else moved: for such containers rung 2's drift guard lets an image-only move through (a compose edit still blocks). This message is the one exception to the 360-character body: its commands are never cut, so it may be up to 720.

A dry run never pulls: it runs the guard, logs and notifies "would pull". **A private registry needs `WHALENURSE_REGISTRY_AUTH_DIR`** ([security.md](security.md#private-registries)): a read-only directory with a `config.json` of inline `auths` only, given as `DOCKER_CONFIG` to the `compose pull` and to no other command. Without it the compose child never reads a Docker config (its `DOCKER_CONFIG` is an empty, private directory whalenurse creates at startup next to the control socket, so neither a redirect nor `$HOME/.docker` applies, and `DOCKER_AUTH_CONFIG` is removed), so a registry that needs a login fails the pull (an ordinary failed action, with compose's redacted reason). A pull can take minutes (it is bounded at 10): `/healthz` and the uptime ping tolerate an action in flight for 11 minutes from the start of each phase (unless the Docker API has been failing for longer than twice the poll interval), and `last_seen` is kept current meanwhile, so a daemon restart during a pull is still recognised as a shutdown.

Decisions and open points: [m4-notes.md](m4-notes.md).

## Dependents (`network_mode: service:<parent>` sidecars)

A sidecar breaks whenever its parent restarts or is recreated, by anything. whalenurse finds them from the parent's `start`/`create` events and from the staleness sweep in every poll, and handles each as an incident of its own: parent restarted with the same ID (the sidecar started earlier) restarts the sidecar through the API; parent recreated (the sidecar's `container:<id>` is gone) recreates it with `up -d --no-deps --pull never --no-build <dep>` **without** `--force-recreate` (and with it only if a post-check still finds it stale). A sidecar whose parent ID is gone is never restarted: that fails and turns a running sidecar into an exited one. They go one at a time, each given up to 90 s to come back (running, and healthy if it has a healthcheck) before the next; they wait while their parent has an open incident or an action in flight; and a failing sidecar never triggers an action on its parent. The dependents of an opted-in parent are repaired **without an opt-in of their own**, unless they are labelled `whalenurse.enable=false`; this is the dependents step only: a sidecar that fails on its own (unhealthy, exited) is healed only if it is opted in itself. A dependent repaired this way does not count toward its own flap limit.

A `restart: "no"` parent stopped by a host shutdown stays down, and so do its sidecars: one `NotifyOnly` event names the parent and them.

## Opt a container in

```yaml
services:
  web:
    labels:
      whalenurse.enable: "true"
```

`WHALENURSE_MODE=all` opts in everything except containers labelled `whalenurse.enable=false`. whalenurse never acts on itself, on `docker compose run` containers, or on swarm tasks.

Labels read: `whalenurse.enable`, `whalenurse.actions` (`restart`, `recreate`; default both), `whalenurse.heal-exited` (`crashed` | `always` | `never`), `whalenurse.cooldown`, `whalenurse.flap-limit` (a positive integer replacing `WHALENURSE_FLAP_LIMIT` for this container). `whalenurse.heal.pull` (`true` adds rung 3, see below).

## Dry-run first

```
docker run --rm --read-only --group-add "$(stat -c %g /var/run/docker.sock)" \
  --tmpfs /run:uid=65532,gid=65532,mode=0700 \
  -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/jackspiering/whalenurse:<tag> run --dry-run
```

Every decision is a JSON log line on stderr with a `type` such as `IncidentOpened`, `ActionResult` (with `attr.dry_run=true`), `Suspended`, `DriftBlocked`, `NeedsRecreate`, `WaitingForParent` or `NotifyOnly`. In dry-run the healer holds no-op actors for Docker and for compose, so it cannot change a container (the drift guard still runs, because it only reads: compose's own `--dry-run`, `config --hash`, an image inspect), **and it stores nothing**: incidents, SUSPENDED and pauses of a dry run live in memory only, so they are not there for the next real run to find. A dry run does not need `/data`. For the same reason a dry run keeps no boot id, `last_seen` or manual-stop markers across its own restarts, so it cannot always tell a host shutdown from a crash: after a reboot or a Docker restart it may say it would start a container that a real run leaves down (for example a `restart: "no"` app that exited non-zero, other than 137 or 143, when the host stopped it).

## Docker socket access

Mounting `docker.sock` is **root-equivalent on the host**. The image is `gcr.io/distroless/static:nonroot` (uid 65532, no shell), so it cannot open the socket by default and fails with `permission denied while trying to connect to the docker API`. Give it the socket's group: `group_add: ["<gid>"]` where `<gid>` is `stat -c %g /var/run/docker.sock`. Do not run it as root instead: with the recommended `cap_drop: [ALL]`, uid 0 has no `CAP_DAC_OVERRIDE` and can write neither `/data` nor the `/run` tmpfs (both belong to uid 65532), so `whalenurse run` exits with "cannot open the state store".

Run it with a read-only root filesystem. Writable paths: `/run` (a tmpfs for the control socket) and **`/data`, a volume for the state store**. The image creates `/data` owned by uid 65532 and declares it a volume, so a fresh named volume (`data:/data`) is writable by the nonroot user. A bind mount keeps the host directory's owner: `chown 65532:65532` it. `whalenurse run` (not dry-run) refuses to start if it cannot open the store.

Under systemd, run `whalenurse run` as a user in the `docker` group and set `WHALENURSE_CONTROL_SOCKET` to a path that user can write, and `WHALENURSE_DATA_DIR` to a directory it owns.

## The CLI

The commands talk to the running daemon over its unix socket (`WHALENURSE_CONTROL_SOCKET`, mode 0600). Run them where the socket is, so with a container, through `docker exec`:

```
docker exec whalenurse /whalenurse status [--json]
docker exec whalenurse /whalenurse pause <project>[/<service>] [--for 2h]   # default 2h; --for 0 = until resume; 30m, 1d work
docker exec whalenurse /whalenurse resume <project>[/<service>]
docker exec whalenurse /whalenurse unsuspend <project>[/<service>]
docker exec whalenurse /whalenurse notify test   # one test message to every notification URL
```

- **pause** stops all actions for the project or service. Events are still recorded. A pause expires (default 2h) so a forgotten one cannot disable healing for good; when it ends, or on `resume`, the containers are looked at again within a second. A container outside compose is addressed by its name.
- **unsuspend** lifts SUSPENDED so the ladder may run again. For a container suspended for *flapping* it also forgets its flap history; otherwise its next incident would suspend it again at once.
- **status** lists every container that has an incident, a pause, flap history, a manual-stop marker or a crash-loop count, the pauses, and the rate-limit budget.

There are no write endpoints on the TCP listener (`WHALENURSE_HTTP_ADDR`): it serves GET and HEAD only, and the commands exist only on the unix socket.

## Notifications

Set `WHALENURSE_NOTIFY_URLS` to one or more [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr) URLs (ntfy, Gotify, Discord, Telegram, Slack, Matrix, Pushover, a generic webhook and [many more](https://nicholas-fedor.github.io/shoutrrr/)), separated by whitespace or by a comma that is followed by `scheme://` (a comma inside a URL, as in `ntfy://ntfy.sh/topic?tags=warning,skull`, is part of it). So inside a URL, write a comma that is followed by `scheme://` as `%2C` and a space as `%20`, as in an ntfy button `actions=view%2COpen%2Chttps://example.com`:

```yaml
environment:
  WHALENURSE_NOTIFY_URLS: >-
    ntfy://ntfy.example.com/whalenurse-4f9k2
    discord://TOKEN@WEBHOOKID
```

**The URLs are secrets** (a webhook id and token, a bot token, a private topic). whalenurse never logs one, never shows one in `status`, `/healthz` or an error, and refers to them as `#1 ntfy`, `#2 discord`. Keep them out of compose files you commit (use an env file).

Check the setup first: `docker exec whalenurse /whalenurse notify test` sends one message to every URL and prints one line per URL, `ok` or `FAILED` with the reason (redacted); it exits 1 if any failed. A dry run (`run --dry-run`) sends notifications too, marked `[dry-run]`.

What is sent: an action taken (`restarted`, `recreated`, or the failure), recovered, **giving up (SUSPENDED) with the cause** (including "the pull found no newer image"), **upgraded during heal** (the pull rung, with the rollback), unsuspended again (with the reason), whalenurse's own action rate limit reached (at most once an hour per URL), flapping, **a recreate blocked by the drift guard** (with the reason and what to do), a container that is left down by policy (a `restart: "no"` parent stopped by a host shutdown, with its dependents), a needed recreate that is not available, the version-gate warning, and pause and resume. Each message names the host and the container key, and fits a phone push (title 72, body 360 characters). The same event for the same container is sent once per incident, and that is remembered across restarts (the state store keeps it, 24 h; a dry run keeps it in memory only). Each URL has its own limit (`WHALENURSE_NOTIFY_RATE_LIMIT`, default 20 an hour; the important messages may use all of it, the routine ones half) and its own queue: a slow or dead URL never delays healing or another URL. Failures are logged by position and scheme, counted, and shown in `status`.

**Uptime ping.** With `WHALENURSE_HEARTBEAT_URL` (a healthchecks.io or Uptime Kuma push URL) whalenurse sends a GET every `WHALENURSE_HEARTBEAT_INTERVAL` (default 5m) while it is healthy. When it is not (the events stream has been down too long, the Docker API has been failing too long, or the loop is silent), it stays quiet, so the monitor raises the alarm by the missing ping. A dry run sends the uptime ping too, unmarked: give a second instance (for example a dry run on another host) its own URL or none, or it keeps the live instance's monitor green.

Decisions and open points: [m3-notes.md](m3-notes.md).

## The dashboard and the read-only API (M5)

Off unless `WHALENURSE_HTTP_ADDR` is set. It shows what whalenurse knows and can change nothing: GET and HEAD only, no write endpoint, not even through the API. `pause`, `resume` and `unsuspend` stay CLI-only (`docker exec whalenurse /whalenurse ...`).

```yaml
services:
  whalenurse:
    ports: ["127.0.0.1:8484:8484"]      # loopback only; a reverse proxy or Tailscale in front of it
    environment:
      WHALENURSE_HTTP_ADDR: ":8484"      # inside the container; the port mapping above decides who can reach it
      WHALENURSE_HTTP_TOKEN: ${WHALENURSE_HTTP_TOKEN} # from an env file, not a committed file
      # WHALENURSE_METRICS: "true"
```

**Exposure.** The page lists container names, states, image names and the event history, so keep it off the open network. In order of preference: publish on `127.0.0.1` and put a reverse proxy in front, or run a Tailscale container on a **shared bridge network** with whalenurse and forward with `tailscale serve`. **Never `network_mode: service:<tailscale>`**: whalenurse would become a dependent of a container it may heal (plan §3.8). Whalenurse warns at startup when the listener is not loopback and no token is set, and when the token is shorter than 16 characters. Use a long random token (`openssl rand -hex 24`); wrong tokens are limited (below).

**The token.** With `WHALENURSE_HTTP_TOKEN` set, every request but `/healthz` needs it: `Authorization: Bearer <token>`, or HTTP Basic with the token as the password and any user name (a browser asks for it once; that is how the dashboard works, because a browser cannot send a bearer header on a page load or an `EventSource`). It is never read from the URL. The comparison is constant time. **Both forms send the token in clear on every request: use them only over TLS** (a reverse proxy that terminates TLS, or Tailscale Serve), or on loopback.

**Wrong tokens.** Ten wrong tokens within a minute from one client address block it: 429 with `Retry-After`, for 1 minute, then 2, 4 ... up to 15 minutes for each further block (forgotten after an hour without a failure). While blocked, even the right token gets 429. The address is the connection's peer, and `X-Forwarded-For` is ignored because a client can forge it, so **behind a reverse proxy all users share the proxy's budget unless you trust the proxy** (someone who guesses wrongly through the proxy locks everybody out for a minute): set `WHALENURSE_HTTP_TRUSTED_PROXIES` to the proxy's address or network (CIDRs, comma or space separated) and whalenurse reads `X-Forwarded-For` **only on connections from those addresses**, taking the right-most entry that is not itself a trusted proxy, so the limit is per real client; the proxy must append to `X-Forwarded-For`, and nothing else should reach the listener. Without it, keep the token long and, if you can, let the proxy rate-limit and authenticate too. Whalenurse logs each block once.

**Without a token** only the Host names `localhost`, `127.0.0.1` and `[::1]` are served (any port), and the names in `WHALENURSE_HTTP_ALLOWED_HOSTS` (comma or space separated, for example `nas.lan,192.168.1.5`); anything else gets 403. That stops a web page from reading the dashboard through DNS rebinding in a browser on the same network; it does not stop a program that sets any Host it likes, so a listener that is reachable from the network still needs a token. With a token the Host is not checked, and the variable is ignored. `/healthz` is exempt and answers only `ok` or `not ok`.

| Path | What |
|---|---|
| `/` | overview: healer status, compose binary and version, version gate, dry-run banner, action budget, counts, pauses, notifications, what needs attention |
| `/containers`, `/containers?all=1` | the watched containers (opted in, or sidecars of an opted-in one), or every container |
| `/containers/<key>` | inspect summary, ladder and incident, SUSPENDED/PAUSED with reason and expiry, the drift guard's last verdict, parent and dependents, last upgrade with the rollback commands, timeline |
| `/history` | incidents and events, filterable. The noisy `EventAttributed` events (a Docker event during a whalenurse action) are hidden; "Debug (everything)" or `?debug=1` shows them, and so does `?debug=1` on a container page |
| `/api/v1/status`, `/containers`, `/containers/<key>`, `/events?since=<id>&limit=&key=&type=`, `/incidents` | JSON. Every record has `host`, times are UTC, `events` ids are monotonic per host and survive restarts; `next` is the cursor to ask for next, `reset: true` means the cursor was ahead of this host (start over) |
| `/api/v1/stream` | server-sent events; resume with `Last-Event-ID` or `?since=` |
| `/metrics` | Prometheus text, only with `WHALENURSE_METRICS=true` |
| `/healthz` | `ok` or `not ok` (503), never a reason; no token and no Host check. The reason is in `/api/v1/status` and `whalenurse status` |

What is never shown: environment variables, commands, mounts, labels outside `com.docker.compose.*` and `whalenurse.*`, notification and uptime-ping URLs, the token. Event text and error text are passed through the same redactor as the notifications. A **dry run** serves the last 5,000 events from memory and its history is lost on a restart; with the store, events are kept for `WHALENURSE_HISTORY_RETENTION` (at most 100,000).

## Settings

Every variable, with its default, is in [configuration.md](configuration.md) (a test keeps that page in step with the code). Durations take `s`, `m`, `h` and also `d` (`30d`). An unknown `WHALENURSE_*` name logs a warning.

## What it does

- **The ladder, one pass per incident.** Rung 1 restarts (`restart` for running containers, `start` for created or exited ones), then the cooldown, then a re-inspect. If the container is still bad, rung 2 recreates it (drift guard first), then the cooldown and a re-inspect again; if it is still bad, the container is **SUSPENDED** (`Suspended`): nothing more is tried. A rung that cannot run (the version gate, no usable compose inputs, a container compose did not make) is recorded (`RungUnavailable`) and the ladder goes on. Each rung counts against the rate limit, dependents included. A failed start whose cause is a network that is gone goes straight to rung 2.
- **Recovered** means a healthcheck says `healthy`, or, with no healthcheck, the container is running and `RestartCount` has not changed for `RECOVERY_PERIOD`. SUSPENDED clears at that point. The incident itself closes (`Recovered`) only after the container has stayed recovered for `RECOVERY_PERIOD`; if it fails again before that, it is the same incident and the ladder goes on where it was, so a spent ladder means SUSPENDED again.
- **Flapping.** More than `FLAP_LIMIT` incidents within `FLAP_WINDOW` suspend the container the next time an incident opens (nothing is tried), even if every earlier one recovered. Closing an incident does not reset the count; only the window expiring does.
- **SUSPENDED also clears** on a `create` by someone else, or on a `start` that follows a manual stop (someone stopped it, someone started it). A `start` by the restart policy never clears it. Nor does a `start` inside a restore window, nor one that follows a quarantined shutdown kill (below).
- **Crash loops.** At least 3 `RestartCount` increases, the first at least `CRASHLOOP_AFTER` ago and the latest less than `CRASHLOOP_AFTER` ago (so a few quick restarts followed by a long quiet are not a loop); the count clears after `RECOVERY_PERIOD` without an increase. The poll reads `RestartCount` of every opted-in container, so a loop is found whether or not its `die` events were seen.
- **Shutdown kills.** A daemon restart or reboot sends `kill` to every container, whalenurse included. When the events stream is lost, every manual-stop marker set in the 30 s before is discarded; a changed boot id drops all markers; and whalenurse writes a `last_seen` heartbeat (every 5 s and when it stops cleanly), so that at its next start the markers set in the 30 s before the previous run's end are discarded too, and a gap longer than the poll interval opens a restore window. For `STARTUP_SETTLE` after a reconnect (or a new boot id, or such a gap) a `start` is a restore and a `kill` sets no marker. After every reconnect that follows a lost stream, everything is swept once the settle time is over. API errors while the daemon restores are "not ready", never an incident. The price: a real `docker stop` in the 30 s before whalenurse itself stops is forgotten.
- **Stopped by the host.** A container whose `FinishedAt` falls in the shutdown window (30 s before `last_seen` or the stream loss, up to the daemon's restore) was stopped by the host, whatever its exit code (an app that handles SIGTERM may exit 1). Its restart policy decides: `restart: "no"` stays down with one `NotifyOnly`; `always`, `unless-stopped` and `on-failure` are left to Docker's restore (a failed restore is a `State.Error`, handled as a failed start); `whalenurse.heal-exited=always` starts it. The window after a restart within one boot ends 5 minutes after `last_seen`.
- A container whose `network_mode: container:<id>` parent was recreated (the ID is gone) is **never** restarted: that fails and turns a running dependent into an exited one. It is recreated (see Dependents). If the compose rungs are disabled it is only advice (`NeedsRecreate`), as in M2a. If the parent is only stopped, whalenurse acts on the parent alone and starts the dependent after the parent runs.
- `/healthz` reports unhealthy when the Docker events stream has been down longer than twice the poll interval, or when the Docker API has been failing (a list or inspect that errors or times out) for longer than twice the poll interval, also while an action is running. Every container list, inspect and image inspect whalenurse makes has a 30-second timeout, so one call that hangs is an API error for that container (it is tried again at the next poll), not a stop of all healing.

Decisions and open points: [m2a-notes.md](m2a-notes.md).
