# Troubleshooting

Start with what whalenurse itself says:

```
docker exec whalenurse /whalenurse status            # containers with an incident, a pause, flap history; the budget; versions
docker logs whalenurse                               # JSON lines: "type" is IncidentOpened, ActionResult, Suspended, DriftBlocked ...
docker exec whalenurse /whalenurse healthcheck       # is whalenurse itself healthy?
```

and the dashboard, if you enabled it (see [reading the dashboard](#reading-the-dashboard)).

## A container is not being healed

Work down the list; `status` and the dashboard's container page say which one it is.

1. **Is it opted in?** `whalenurse.enable: "true"` on the service (in the compose file, then `docker compose up -d` so the container
   has the label), or `WHALENURSE_MODE=all`. A sidecar (`network_mode: service:<parent>`) of an opted-in parent is repaired
   without a label, but not healed for its own failures.
2. **Is the daemon in dry-run?** `WHALENURSE_DRY_RUN=true` logs and notifies "would restart" and changes nothing. The
   dashboard shows a banner.
3. **Is it SUSPENDED or PAUSED?** See below. Neither gets any action.
4. **Is it one whalenurse never touches?** Its own container, `docker compose run` containers (`oneoff=True`), swarm tasks, and a
   container labelled `whalenurse.enable=false`.
5. **Is the action budget spent?** `WHALENURSE_RATE_LIMIT` (default 10 actions an hour, host-wide). `status` shows the budget and
   the next free slot; a notification says when it is reached.
6. **Is it just waiting?** After a trigger it waits `WHALENURSE_GRACE` (30 s) and then a cooldown between rungs (30 s, doubling).
   A parent with an open incident also holds its sidecars back.
7. **Did a host shutdown stop it?** A container stopped by a daemon restart or reboot is judged by its restart policy:
   `restart: "no"` stays down on purpose (one notification, its `network_mode` sidecars stay down with it);
   `always`/`unless-stopped`/`on-failure` are left to Docker's own restore. `whalenurse.heal-exited=always` starts it anyway.

## Why is my container SUSPENDED?

SUSPENDED means whalenurse gave up on purpose: nothing more is tried until the container recovers or you say so. The
"giving up" notification, `status` and the dashboard's banner give the cause. The usual ones:

| Cause | What it means | What to do |
|---|---|---|
| **the ladder ran out** | A restart and a recreate (and the pull, if enabled) each ran once and the container is still bad. | Look at the container's own logs (`docker logs <container>`): whalenurse can restart and recreate, not fix an application that is broken at start. Fix the cause, then `unsuspend`. |
| **flapping** | More than `FLAP_LIMIT` incidents (3) within `FLAP_WINDOW` (24 h), even though each one recovered. | The container keeps failing and recovering. Find out why; `unsuspend` also forgets its flap history. |
| **drift** | The recreate was blocked by the drift guard. Read [the drift messages](#drift-block-messages). | Decide whether the change on disk should be deployed. |
| **recreate not possible** | `recreate skipped`: the compose files or labels are missing or unreadable from inside whalenurse (the stacks are not mounted at the identical path), the compose rungs are disabled by the version gate, or `recreate` is not in `whalenurse.actions`. | See [Recreate is skipped](#recreate-is-skipped). |
| **pull found nothing** | The opt-in pull rung pulled and the image did not change ("the pull found no newer image for <ref>"). | Nothing newer exists; the container is broken for another reason. |
| **pull does not apply** | `pull_policy: never` or `build` on the service, or the policy could not be read. | The pull rung respects those policies by design. |

To get going again: `docker exec whalenurse /whalenurse unsuspend <project>[/<service>]`. A suspended container also clears
by itself when it becomes healthy again, when someone creates it anew (`docker compose up -d` with a change), or when someone
starts it by hand after stopping it. A start by the restart policy, or by Docker restoring containers after a restart or
reboot, never clears it.

## Drift-block messages

Before it recreates a container, whalenurse asks whether the recreate would deploy something other than what is running. A
recreate builds the container from the compose files on disk and the image in the local store *now*, so an edit you have not
deployed yet, or an image tag that moved, would be deployed by a "heal". In `strict` mode (default) whalenurse refuses,
says why and SUSPENDS. The messages:

| Message | Meaning | What to do |
|---|---|---|
| `compose would recreate this container (its dry-run says: Recreate): the compose file, an env file or the image changed since it was created` | Compose's own dry run says the container no longer matches the file, an env file or the image. It cannot say which. | Check `git diff` / `docker compose config` of the stack. If the edit is meant to go out, deploy it yourself (`docker compose up -d <service>`) and `unsuspend`. If not, undo it. |
| `config --hash is X, the container was created with Y: the compose file or an env file changed` | The same, found by comparing hashes (used when the dry-run output could not be read). | Same. |
| `image <ref> now resolves to A, but this container was created from B: the tag moved` | Someone pulled a newer image under the same tag (another stack, Watchtower, `docker pull`). A recreate would silently upgrade this container. | Pull-and-recreate on purpose (`docker compose up -d <service>`), or re-tag the old image, then `unsuspend`. If the tag was moved by whalenurse's own pull rung of another container, the message names that container and when (`moved_by`). |
| `... (compose upgraded from X to Y since this container was created, so this may be a changed hash layout rather than an edit)` | Information added to a config finding: the host's compose plugin was upgraded after the container was made, and compose versions compute the hash differently. | Usually nothing is edited: recreate it once yourself with `docker compose up -d <service>` so it is made by the current compose. |
| `can't verify drift (compose X on host vs Y in whalenurse): ...` | See below. | See below. |
| `image <ref> cannot be resolved for linux/<arch>: ...` | The image the container runs is no longer in the local image store under that name (it was pruned or re-tagged away), so the recreate would have to pull. A heal never pulls. | Pull or restore the image yourself, then `unsuspend`. |

`WHALENURSE_DRIFT_GUARD=warn` recreates anyway for *verified* drift (an edit, a moved tag) and says so. It does not override
a "can't verify" finding.

### "Can't verify drift"

whalenurse uses the compose that made your containers when it can: the host's plugin mounted read-only at
`/host/docker-compose`. Without it, it falls back to the copy bundled in the image, and a container made by another compose
release than the bundled one cannot be judged: compose sees a change, and whalenurse cannot tell an edit from a changed
hash layout. It refuses rather than guess. Fix: **mount the host's plugin** (`docker info` shows its path; usually
`/usr/libexec/docker/cli-plugins/docker-compose`). The same finding family also appears when a check could not be made at all:
the container has no `com.docker.compose.config-hash` or `.image` label, or the dry run and `config --hash` both failed.
The message says which.

## Recreate is skipped

Messages like `recreate skipped: compose inputs of project X are not readable from here ...`:

- The stacks root must be mounted **at the identical host path**: `/opt/stacks:/opt/stacks:ro`, not `/opt/stacks:/stacks:ro`.
  Compose passes host paths to the daemon, so any other path fails.
- Mount whatever else compose reads from outside the project directory: `env_file` paths with `../`, `include:` and `extends:`
  files, file-based `secrets:` and `configs:`.
- The directory and all its parents must be readable (traversable) by uid 65532.
- A container that compose did not create (`docker run`) has no labels to recreate from and only gets the restart rung.
- `label com.docker.compose.service is "...", which is not a compose service name` (or the same for
  `com.docker.compose.project`): the label holds something compose would never write, such as a value starting with `-`, a
  `,` or a `:`. whalenurse does not pass it to compose, so the container gets the restart rung only. Compose writes these
  labels itself; a value like this usually comes from the image's own labels or from a container made by hand. Check with
  `docker inspect -f '{{json .Config.Labels}}' <container>` and `docker image inspect -f '{{json .Config.Labels}}' <image>`.
- Engine below 28.1 or compose below v5.2.0: rungs 2 and 3 are disabled at startup, with one warning that names the versions.
  `status` shows it.

## Notifications are not arriving

1. `docker exec whalenurse /whalenurse notify test`. It sends one message to every URL and prints `ok` or `FAILED` with a
   redacted reason per URL; it exits 1 if any failed or no URL is set. If `WHALENURSE_NOTIFY_URLS` is empty inside the
   container, the variable never reached it (check the env file and `docker compose config`).
2. `status` shows each URL as `#1 ntfy` ... with its failures and the number of messages held back; `INVALID` means Shoutrrr
   rejected the URL (check the [URL format](https://nicholas-fedor.github.io/shoutrrr/) for your service). The logs name the
   URL by position and scheme only, never by value.
3. **Once per incident.** The same event for the same container is sent once per incident, and the record survives restarts. So
   a second identical "restarted" within one incident is not news. A new incident is.
4. **Rate limit.** `WHALENURSE_NOTIFY_RATE_LIMIT` messages an hour per URL (20); routine messages may use half. The next message
   that goes out says "N earlier messages were held back".
5. **Dry-run sends**, marked `[dry-run]`; it is how you check the setup. (The uptime ping is sent in a dry run too.)
6. Which events notify: an action taken, recovered, giving up / flapping, unsuspended, drift blocked, upgraded during heal,
   a recreate that cannot run, the rate limit reached (once an hour), the version-gate warning, pause and resume. Everything
   else is only in the logs and the dashboard.
7. Egress: the container must reach the service (DNS, firewall, a proxy). A hung URL delays nothing else; a failure is
   logged, counted and retried twice.

## The pull fails on a private registry

`pull failed` with `unauthorized`, `denied`, `pull access denied`, `no basic auth credentials` or `authentication required` in the reason:

1. `whalenurse status` must list the registry under `pull credentials`. If it does not, `WHALENURSE_REGISTRY_AUTH_DIR` is not
   set, or the key in `auths` is not the registry's name as the image reference writes it (`ghcr.io`, `registry.example.lan:5000`,
   `https://index.docker.io/v1/` for Docker Hub).
2. The token must be allowed to **read** that repository, and must not have expired. Test it from the host with
   `docker login` using the same token.
3. A startup error that names a key (`credsStore`, `currentContext`, ...) means the file holds more than `auths`: remove it.
   Credential helpers do not exist in the image, so `auths` must carry the credential inline.
4. The file is checked again before every pull; if it became unreadable or invalid since startup, the failed pull says so.

Reasons are redacted: the credentials never appear in a log, `status`, the dashboard or a notification.

## Exit codes

What a container's exit code (and `docker ps`'s `Exited (N)`) tells you, and what whalenurse does with it:

| Code | Meaning |
|---|---|
| 0 | Clean exit. With `restart: "no"`/`on-failure` it was intended, and whalenurse leaves it alone. |
| 126 / 127 / 128 with a start error, status `created` | A failed **first** start: 126 permission denied, 127 command not found or not a directory, 128 anything else. The container never ran. |
| 126 / 127 / 128 with a start error, status `exited`, restart count unchanged | A failed **start at boot** (after a daemon restart or reboot): the code is mapped from the error (127 for a bind source that is gone). |
| any code with a start error, status `exited` | A failed **restart by the restart policy**. The code is the *previous* run's, so do not read it: read `State.Error` (the dashboard shows it). |
| 137 | SIGKILL: an OOM kill if `OOMKilled` is true, otherwise `docker kill` or a stop that hit its timeout. |
| 143 | SIGTERM, usually an ordinary stop. |

A non-empty start error always wins over the exit code: whalenurse classifies a failed start by that text, never by the code.
It then splits by cause: a network namespace of a parent that was recreated (`No such container`) means recreate the sidecar; a
parent that is merely stopped (`cannot join network namespace of a non running container`) means heal the parent first; a
missing bind source, a port already allocated (`port is already allocated`), a bad command: start, then recreate.
`137`/`143` without an OOM are a stop: not healed in `crashed` mode, notified only.

## Reading the dashboard

- **Overview**: the healer's state (events stream, last poll), the compose binary and version, the **version gate** (which
  rungs are available), the dry-run banner, the action budget, counts, pauses, "Needs attention" and the latest events.
- **Containers**: one row (a card on a phone) per watched container: state, health, and a **healer** badge — *ok*, *Incident*
  (the ladder is running), *PAUSED* (with when it ends), *SUSPENDED* (with why: ladder, flapping, drift) — the rung progress, the
  cooldown and the flap count. `?all=1` also lists containers that are not watched.
- **The ladder** on a container page: `restart ✓ → recreate ✗ blocked (drift)` means the recreate was stopped by the drift guard.
  *recreate skipped – not available* (never a tick) means the rung could not run: the version gate, no readable compose inputs,
  or a `pull_policy` that forbids the pull.
- **Drift status** on a container page is the guard's *last verdict* from the event log, not a live check (a page view must not
  run compose). **Last upgrade during heal** shows the old and new image and the rollback commands, one per line with a Copy
  button (shown only with JavaScript). The commands are shown exactly or not at all: when part of them matches a configured
  secret (a notification URL, the uptime ping URL, the HTTP token or a registry credential), masking them would make a
  command that is not the rollback, so the page says they are withheld, and `/api/v1` leaves out `rollback_tag` and
  `rollback_up`. Build them by hand from the old image and the reference shown. A rollback does not last while
  `whalenurse.heal.pull` stays on: the next heal pulls the newer image again. Pin the old image in the compose file or
  remove the label before you run the `up` line.
- **History** lists incidents and events, with filter chips. The noisy `EventAttributed` events (a Docker event that arrived
  during a whalenurse action) are hidden; *Debug (everything)* or `?debug=1` shows them.
- Times are UTC; an expiry reads "2m left (until 14:32)". Container names are the final names (compose's temporary
  `<12 hex>_` prefix is removed).
- **401** needs the token (browser: any user name, the token as the password). **403** is the Host check (no token set: use
  `localhost`, or add the name to `WHALENURSE_HTTP_ALLOWED_HOSTS`). **429** is the wrong-token limit; wait `Retry-After`
  seconds. Behind a reverse proxy set `WHALENURSE_HTTP_TRUSTED_PROXIES`, or one user's mistakes lock out all of them.

## Startup problems

| Symptom | Cause and fix |
|---|---|
| `permission denied while trying to connect to the docker API` | The image runs as uid 65532, which cannot read the socket. Add `group_add: ["<gid>"]` with `stat -c %g /var/run/docker.sock`. |
| `whalenurse run` exits: cannot open the store | `/data` is not writable by uid 65532. A named volume is fine (the image prepares it); a bind mount needs `chown 65532:65532`. Under systemd use `StateDirectory=`. Running the container as root (`user: "0"`) with `cap_drop: [ALL]` fails the same way: root without `CAP_DAC_OVERRIDE` cannot write a directory owned by 65532. Use `group_add` for the socket instead. |
| The container is `unhealthy` right after start | The image's health check talks to the control socket in `/run`: give the container a `tmpfs: /run:uid=65532,gid=65532,mode=0700` (it is read-only otherwise). |
| `whalenurse status` says it cannot connect | The CLI uses the unix socket (`WHALENURSE_CONTROL_SOCKET`); run it where the socket is (`docker exec`). Under systemd run it as the service user. |
| A warning that the dashboard has no token | `WHALENURSE_HTTP_ADDR` is not loopback and no token is set. Set the token, or publish the port on `127.0.0.1`. |
| `run` exits: `WHALENURSE_REGISTRY_AUTH_DIR: ...` | The credentials file is not valid: it must be a `config.json` with only `auths`, each entry with an inline credential ([deployment.md](deployment.md#private-registries-for-the-pull-rung)). The message names the problem and never a value. |
| Unknown `WHALENURSE_...` variable | A typo; the warning names it. |
