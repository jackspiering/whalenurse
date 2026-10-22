# Changelog

All notable changes to whalenurse are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/). The design history behind each change is in
[docs/PLAN.md](docs/PLAN.md) and the `docs/m*-notes.md` files.

The release workflow publishes the section of the tag that is pushed as the release notes. Before tagging, replace
`YYYY-MM-DD` of the version about to be released with the release date (see [docs/releasing.md](docs/releasing.md)); the
workflow refuses an undated section.

## [Unreleased]

### Security

- **Event text is redacted in the log and in notification bodies**, as it already was in the API. An error that quoted a
  notification URL, the uptime ping URL, the HTTP token or a registry credential reached the container log unmasked, and
  was pushed to every notification service (one URL's token to another URL's channel). The message and the free-text
  attributes of every event now pass the redactor; a rollback command it would change is withheld from the notification.

### Changed

- **The rollback `up` line has `--no-build`**, like every other `up` whalenurse runs. If the tag line failed or was skipped,
  the `up` now stops with an error instead of building an image that is not the old one. Rollbacks already shown keep
  their form.

### Fixed

- **The "upgraded during heal" message keeps its rollback commands exact.** After the rate limit had held messages back,
  the note that says so cut the end of the message, which is where the rollback commands are; the note now goes before
  them. Whitespace inside a quoted path of the commands was collapsed to one space; the commands are now sent byte for byte.
- **A failed rung-2 recreate that still changed the image is announced.** For a `whalenurse.heal.pull=true` container,
  compose replaces the container before it starts it, so a start that failed (a port in use) left it on the moved image
  without the "upgraded during heal" message or its rollback. That message is now sent, saying that the recreate failed.
- **The dashboard and the API show the rollback commands exactly, or not at all.** The redactor that masks the
  notification URLs and the token could change ordinary words of a rollback command (a topic named `docker`, a registry on
  the receiver's host), and the page offered the masked command with a Copy button. Such a rollback is now withheld, and the
  page says so.
- **A container suspended again after a drift block is notified.** When a drift-blocked container recovered and failed
  again in the same incident, the second SUSPENDED was dropped as if it repeated the drift block, so the last message said
  "unsuspended". It is now sent as a giving-up message, and the unsuspend after it is news again.

### Documentation

- **A rollback does not last while `whalenurse.heal.pull` stays on.** The next heal reaches the pull again and the registry's
  newer image comes back. The "upgraded during heal" message, the dashboard and the docs now say so and how to keep the old
  image: pin it in the compose file or remove the label before running the `up` line.

## [0.1.2] - 2026-10-22

A security fix (a compose label could put a flag on the compose command line), fixes from a code review, private
registries for the pull rung, and a hardened release pipeline.

### Security

- **A compose label could put a flag on the compose command line.** The project and service labels, which an image
  can ship, were passed to compose unchecked; a service of `--dry-run=false` overrode the drift guard's `--dry-run` and ran a
  real `up` of the project, in dry-run mode too. whalenurse now refuses a project or service that is not a name compose would
  write, and that container gets the restart rung only. 0.1.0 and 0.1.1 are affected.
- **The release is built from an empty Go cache.** The release workflow no longer restores the Go build and module cache
  that `main`'s CI saves (`actions/setup-go` cached both by default), so nothing compiled in another run can reach the
  signed binaries and image. CI checkouts no longer keep the token in `.git/config`.
- **The image's builder is pinned.** The BuildKit that builds the release image is pinned by digest
  (`moby/buildkit:v0.33.1`, the release the floating `buildx-stable-1` named), and the Dockerfiles no longer fetch the
  floating `docker/dockerfile:1` frontend; BuildKit's built-in one is used.
- **Go 1.27.3.** The binaries and the image are built with Go 1.27.3, which fixes GO-2026-6809 (`crypto/x509` trusted
  compromised intermediate certificates; whalenurse reaches it through the SMTP notifications). CI and the release now run
  `govulncheck`, and check that the Dockerfiles build with the Go of `go.mod`.
- **Every action pin is checked against its tag.** CI resolves the release tag in each pinned action's comment and fails
  if the SHA is not that tag's commit (a drifted comment, or a commit that exists only in a fork), and refuses `uses` keys
  written in flow style or quoted, which the pin check could not see.

### Added

- **Private registries for the pull rung (R5).** `WHALENURSE_REGISTRY_AUTH_DIR` names a read-only directory holding a
  `config.json` with inline `auths` and nothing else. It is given to `docker compose pull`, the pull rung's pull, as
  `DOCKER_CONFIG`, and to no other command. Any other key (`credsStore`, `credHelpers`, `currentContext`, `proxies`, ...),
  a `contexts` entry, an entry without an inline credential, or an empty `auths` stops `run` at startup; the file is checked
  again before every pull. The credentials are redacted from logs, `status`, the API, the dashboard and notifications, and
  `status`, `GET /api/v1/status` (`pull_auth.registries`) and the home page name the registries, never a user name or a
  secret. The risk (credentials in a container that holds the Docker socket) and the advice (a pull-only token, mode
  0440, a `:ro` mount) are in [docs/security.md](docs/security.md#private-registries).

### Changed

- **examples/full trusts its Tailscale proxy.** The `dashboard` network has a fixed subnet (`DASHBOARD_SUBNET`, default
  `10.84.248.0/29`) set as `WHALENURSE_HTTP_TRUSTED_PROXIES`, so the wrong-token limit counts per tailnet client instead of
  one budget shared by everyone behind Tailscale Serve. **If you run this example already**, `docker compose up -d` has to
  give the existing `dashboard` network the new subnet; if compose refuses, run `docker compose down` for this example's own
  stack (whalenurse and its Tailscale container only), then `up -d`.

### Fixed

- **`make verify-release` says why a check failed.** With `gh` installed but not logged in it reported a genuine release as
  `NOT VERIFIED` with "rejected" lines; it now says that `gh` is not logged in (`gh auth login` or `GH_TOKEN`), and every failed
  cosign or gh call shows the tool's own error. It prints the image digest it verified and checks the image's provenance
  for that digest, and the by-hand `gh attestation verify` commands in the docs pin the release workflow (`--signer-workflow`).
- **A `docker compose run` container hid its service from healing and the dashboard.** Compose writes no
  container number on a one-off container, so it shared the state key of the service's first container and, being newer,
  replaced it in the index: the service went unhealed for as long as the one-off container existed. A one-off or swarm-task
  container no longer takes a key from a container whalenurse watches, and `com.docker.compose.oneoff` is read the same
  case-insensitive way everywhere.
- **A final release tagged on the same commit as its `-rc` is released as itself.** The release workflow now tells
  GoReleaser the tag it is releasing; before, GoReleaser took the first tag on the commit, the `-rc`, and would have
  re-published the `-rc` image tag and failed to create the release.
- **Docs: backing up the store.** [docs/upgrading.md](docs/upgrading.md#backing-up-and-restoring-the-store) now says to stop
  whalenurse before copying its SQLite database (a copy of a running one may not open), and gives the backup and restore
  commands for the container and systemd setups, including removing a newer version's `-wal`/`-shm` before a restore.
- **Docs: no root alternative for the socket.** [docs/running.md](docs/running.md#docker-socket-access) offered `user: "0"`
  instead of `group_add`, which cannot start with the documented hardening (`cap_drop: [ALL]`: root cannot write `/data` or the
  `/run` tmpfs, both owned by uid 65532). `group_add` is now the only way given, and troubleshooting explains the failure.
- **Docs corrections:** the boolean spellings, the cooldown cap and the compose labels read (labels.md); what whalenurse
  writes and when it forces a recreate (security.md); `%2C` for a comma in a notification URL; what a dry run cannot know
  after a reboot (running.md).

### Documentation

- **Labels from the image.** [docs/labels.md](docs/labels.md#labels-from-the-image) and
  [docs/security.md](docs/security.md) now say that Docker copies an image's own labels onto its containers, so an image's
  publisher can set `whalenurse.*` and compose labels too (an image can opt itself in), how to check an image, and how to
  override a label. labels.md no longer says that a container compose did not make always gets only the restart rung.

## [0.1.1] - 2026-10-20

A fix to how whalenurse starts compose.

### Fixed

- **The compose child could read `$HOME/.docker`.** whalenurse removed `DOCKER_CONFIG` from the environment of the compose
  commands it runs, but kept `HOME`, and with `DOCKER_CONFIG` unset the Docker CLI reads `$HOME/.docker/config.json`. Under
  the example systemd unit that is `/var/lib/whalenurse/.docker`: a `config.json` there would have given compose registry
  credentials and a `currentContext` pointing at another daemon. Every compose command now runs with
  `DOCKER_CONFIG` set to an empty directory whalenurse creates for itself at startup (mode 0700, next to the control socket,
  else in the temp directory; removed at exit), so no Docker config is ever read. It has to be writable: compose v5.2.0 to
  v5.5.x write buildx state there during the drift guard's dry-run. If no such directory can be created, a startup warning
  says so and the drift guard of those versions uses `config --hash` instead of the dry-run.
  `DOCKER_AUTH_CONFIG` (credentials in the environment, read by recent Docker CLIs) is removed too. The container image was
  not affected in practice: nothing in it creates `/home/nonroot/.docker`.

## [0.1.0] - 2026-10-20

The first public release.

### Added

- **The healer.** For opted-in containers (`whalenurse.enable=true`, or `WHALENURSE_MODE=all`) that go unhealthy, crash-loop or
  stop unexpectedly: rung 1 restarts (Docker API), rung 2 recreates from the stack's own compose files, and the optional rung 3
  (`whalenurse.heal.pull=true`) pulls a newer image and recreates. One pass per incident, then SUSPENDED and a notification.
  Cooldown, a flap limit, a global action rate limit, pause (default 2 h) and `--dry-run`.
- **Compose-aware recreate.** `up -d --no-deps --pull never --no-build <service>` with the project, files and env file recorded
  on the container; never `compose down`. `network_mode: service:<parent>` sidecars are repaired after their parent is
  restarted or recreated, one at a time.
- **The drift guard.** A recreate is skipped, and the container SUSPENDED with the reason, when it would deploy a compose edit,
  a moved image tag, or when compose cannot be trusted to judge (version skew, a check that could not be made).
  `WHALENURSE_DRIFT_GUARD=warn` overrides verified drift only.
- **Decisions from the container's own state**, not from the order of Docker events: failed starts (`State.Error`), crash loops,
  `heal-exited`, and containers stopped by a host shutdown, which are left to their restart policy. Daemon restarts and reboots
  do not set manual-stop markers or clear SUSPENDED.
- **Persistent state** in SQLite (`/data`): incidents, SUSPENDED, pauses, flap history, the event log and the notification
  dedupe all survive a restart.
- **Notifications** through [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr) (ntfy, Gotify, Discord, Telegram, Slack and
  more): dedupe per incident, a per-URL rate limit, `whalenurse notify test`, an optional uptime ping. Notification URLs are
  secrets and are never logged or shown.
- **The pull rung** reads the service's `pull_policy` (`never` and `build` are respected), runs the drift guard in image-move
  mode, tells you what was upgraded and prints the exact rollback commands.
- **A read-only dashboard and JSON API** (`/api/v1`, a server-sent events stream, optional Prometheus `/metrics`), off unless
  `WHALENURSE_HTTP_ADDR` is set. GET and HEAD only. Optional token (bearer, or the password of HTTP Basic for browsers), a Host
  check without a token, and a limit on wrong tokens. Secrets, environment variables and foreign labels never leave the process.
- **The CLI** over a local unix socket: `status`, `pause`, `resume`, `unsuspend`, plus `notify test`, `healthcheck` and `version`.
- `WHALENURSE_HTTP_TRUSTED_PROXIES`: the CIDRs of reverse proxies whose `X-Forwarded-For` the wrong-token limit may use, so the
  limit is per real client instead of per proxy.
- **Distribution.** A multi-arch image (`linux/amd64`, `linux/arm64`) on `ghcr.io/jackspiering/whalenurse` with the compose plugin
  bundled as a fallback, and static binaries with checksums. Images and checksums are signed keyless with cosign, and
  every release carries SBOMs and build provenance ([docs/verifying-releases.md](docs/verifying-releases.md)).
- Documentation: [README](README.md), [configuration](docs/configuration.md), [labels](docs/labels.md),
  [security](docs/security.md), [troubleshooting](docs/troubleshooting.md), [deployment](docs/deployment.md) (container and
  systemd), [upgrading](docs/upgrading.md); examples in [examples/](examples/).

### Fixed

- A `network_mode: service:<parent>` sidecar without an opt-in of its own: the incident the dependents step opens for it now
  closes once it has recovered. Before, it stayed open, so the next time the parent was recreated the sidecar went straight to
  SUSPENDED and was not repaired. Found in the release-gate rehearsal on a real host.
- A sidecar recreated by whalenurse is repaired again the next time its parent is recreated. Compose writes an empty
  `depends_on` label on a service recreated with `up --no-deps`, and that label was how whalenurse found the parent of a sidecar
  whose parent was gone; from the second repair on, the sidecar was skipped without a word. whalenurse now remembers the
  services a sidecar depends on (in its store).

### Requirements

- Docker Engine 25 or later for rung 1 (restart). Rungs 2 and 3 and the drift guard need Engine 28.1 or later and Docker
  Compose v5.2.0 or later; below that they are disabled with one warning and the ladder ends after the restart.

### Known limits

- Rung 3 cannot pull from a private registry (the compose child runs without `DOCKER_CONFIG`).
- One host per whalenurse; there is no multi-host hub yet.
- No write endpoints on the network: `pause`, `resume` and `unsuspend` are CLI-only.
- The dashboard has been checked in headless Chromium, not on a real phone.

## [0.1.0-rc.1] - 2026-10-20

The first release candidate of 0.1.0, published to rehearse the release pipeline (signing, SBOMs, provenance, the image push and
`scripts/release/verify.sh`), which can only run for real on a tag. It contains what 0.1.0 will contain; see the 0.1.0 section
above for the full list. A pre-release: it never moves `latest`, `v0.1` or `v0`, and it is not meant for production.

[Unreleased]: https://github.com/jackspiering/whalenurse/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/jackspiering/whalenurse/releases/tag/v0.1.2
[0.1.1]: https://github.com/jackspiering/whalenurse/releases/tag/v0.1.1
[0.1.0]: https://github.com/jackspiering/whalenurse/releases/tag/v0.1.0
[0.1.0-rc.1]: https://github.com/jackspiering/whalenurse/releases/tag/v0.1.0-rc.1
