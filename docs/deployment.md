# Deployment

whalenurse is one static binary. Run it as a container (the Docker socket mounted) or as a systemd service. Either way, start in
**dry-run**: it logs and notifies what it *would* do and changes nothing, so you can read its decisions on your own host
first. See the [release gate](releasing.md#the-release-gate) for how long the maintainer ran it that way before v0.1.0.

The examples in [examples/](../examples/) are checked in CI (`compose config -q`, `systemd-analyze verify`):

| Example | For |
|---|---|
| [examples/minimal](../examples/minimal/compose.yaml) | restart only; the smallest working setup |
| [examples/full](../examples/full/compose.yaml) | restart and recreate, notifications, the dashboard on your tailnet |
| [examples/systemd](../examples/systemd/whalenurse.service) | a hardened unit |
| [examples/demo](../examples/demo/compose.yaml) | the two-minute demo of the README |

## As a container

The image is `ghcr.io/jackspiering/whalenurse` (`linux/amd64` and `linux/arm64`). Tags: `v0.1.0` (immutable), `v0.1`, `v0` and
`latest`. Pin the exact tag, or its digest, in production, and [verify it](verifying-releases.md).

### What the container needs

```yaml
services:
  whalenurse:
    image: ghcr.io/jackspiering/whalenurse:v0.1.0
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    group_add: ["${DOCKER_GID}"]          # stat -c %g /var/run/docker.sock
    tmpfs:
      - /run:uid=65532,gid=65532,mode=0700
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - whalenurse-data:/data
      - /usr/libexec/docker/cli-plugins/docker-compose:/host/docker-compose:ro
      - /opt/stacks:/opt/stacks:ro        # identical path on both sides
```

- **The socket** is root-equivalent on the host ([security.md](security.md)). The image runs as uid 65532 and has no shell, so
  give it the socket's group with `group_add` rather than running it as root.
- **`/run` as a tmpfs** holds the control socket that the CLI and the image's health check use (the root filesystem is read-only).
- **`/data`** is the state store (`whalenurse.db`): incidents, SUSPENDED, pauses, flap history, the event log, the notification
  dedupe. A named volume works as it is (the image prepares the directory for uid 65532); a bind mount needs
  `chown 65532:65532`. Without it a restart of whalenurse forgets everything that it has not yet recovered from. One
  whalenurse per data directory: `run` holds a lock on `whalenurse.lock` there, and a second `run` on the same directory
  refuses to start.
- **Set `WHALENURSE_HOST_ID`** if you want a stable host name in keys and notifications: the container's hostname is its ID.

### The two read-only mounts that make recreate work

Rung 2 (recreate) runs `docker compose` against the compose files recorded on the container. Inside a container that needs:

1. **The host's compose plugin, read-only, at `/host/docker-compose`.** `docker info` shows its path; usually
   `/usr/libexec/docker/cli-plugins/docker-compose` (some distributions use `/usr/lib/docker/cli-plugins/`). whalenurse uses it
   first because it is the compose that made your containers. The image also bundles a copy (a fallback); a container made by a
   different compose release than the bundled one is then reported as "can't verify drift" rather than recreated
   ([troubleshooting.md](troubleshooting.md#cant-verify-drift)). Docker Desktop has no plugin file to mount: leave the mount
   out and use the bundled one.
2. **The stacks root, read-only, at the identical path as on the host.** Compose passes host paths to the daemon for relative
   bind mounts, so `/opt/stacks:/stacks:ro` does not work; it must be `/opt/stacks:/opt/stacks:ro`. Mount anything else compose
   reads from outside it as well: `env_file:` paths with `../`, `include:`/`extends:` files, file-based `secrets:` and
   `configs:`. The directory and its parents must be traversable by uid 65532.

Without them whalenurse still restarts, says why the recreate rung is skipped, and SUSPENDS what a restart did not fix.
Variables (`${VAR}`) in your stacks' compose files are resolved from the env files that compose recorded on each
container, so keep them where they were.

### Private registries for the pull rung

Only if a container with `whalenurse.heal.pull=true` uses an image from a registry that needs a login. Read the risk first:
[security.md](security.md#private-registries).

1. Make a **pull-only** token for the registry, and write `config.json` next to your whalenurse stack, with nothing but `auths`
   (get the `auth` value with `printf 'user:token' | base64 -w0`; Docker Hub's key is `https://index.docker.io/v1/`):
   ```json
   {"auths": {"ghcr.io": {"auth": "dXNlcjp0b2tlbg=="}}}
   ```
2. Mode `0440`, owner uid 65532 (container) or the `whalenurse` user (systemd), in a directory only they can read.
3. Container: mount the **directory** read-only and set the variable (see the commented lines in
   [examples/full](../examples/full/compose.yaml)):
   ```yaml
   volumes:
     - ./registry-auth:/registry-auth:ro
   environment:
     WHALENURSE_REGISTRY_AUTH_DIR: /registry-auth
   ```
   Systemd: `Environment=WHALENURSE_REGISTRY_AUTH_DIR=/etc/whalenurse/registry-auth`, readable by the service user.
4. `whalenurse status` then shows `pull credentials: ghcr.io`. A bad file stops `run` with the reason (a key that is not
   allowed, an entry with no inline credential); no message shows a value.

### Compose project for whalenurse itself

Put whalenurse in its own stack directory inside the stacks root, with its `.env` (`DOCKER_GID`, notification URLs, token). That
`.env` holds secrets; it is read by compose, not baked into the container's labels. whalenurse never acts on itself; update it
by hand (`docker compose pull && docker compose up -d`) after reading [upgrading.md](upgrading.md).

### Opt containers in

In the compose files of the stacks you want healed:

```yaml
services:
  web:
    labels:
      whalenurse.enable: "true"
```

then `docker compose up -d` once so the container has the label. See [labels.md](labels.md). A first rollout that works well:
run with `WHALENURSE_DRY_RUN=true`, break something on purpose (the [demo](../examples/demo/compose.yaml) uses `bop` for this),
read the log, then switch dry-run off for a few test containers.

### The dashboard

Set `WHALENURSE_HTTP_ADDR=":8484"` and publish it as `127.0.0.1:8484:8484`, or put a reverse proxy or a Tailscale container on a
shared bridge network in front ([examples/full](../examples/full/compose.yaml)). Set a token for anything but loopback. Read
[security.md](security.md) first.

## Under systemd

[examples/systemd/whalenurse.service](../examples/systemd/whalenurse.service) is a hardened unit (`NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome=read-only`, no capabilities, a syscall filter, a dedicated user). The steps are in its header:

1. Get the binary from a release (`whalenurse_<version>_linux_<arch>.tar.gz`; [verify it](verifying-releases.md)) and install it as
   `/usr/local/bin/whalenurse`.
2. Create the user, in the `docker` group (**root-equivalent**, see [security.md](security.md)):
   `useradd --system --home-dir /var/lib/whalenurse --shell /usr/sbin/nologin --groups docker whalenurse`.
3. Install the unit and `/etc/whalenurse/whalenurse.env` (mode 0640, `root:whalenurse`; start from
   [whalenurse.env.example](../examples/systemd/whalenurse.env.example)). The unit sets `WHALENURSE_DATA_DIR` and
   `WHALENURSE_CONTROL_SOCKET` to the directories systemd creates (`StateDirectory=`, `RuntimeDirectory=`).
4. `systemctl enable --now whalenurse`, then `journalctl -u whalenurse -f`.

Differences from the container:

- There is no mount to get right: whalenurse sees the host's paths. The compose lookup order ends with the host's plugin
  directories, searched in this order: `/usr/libexec/docker/cli-plugins/docker-compose`, `/usr/lib/docker/cli-plugins/docker-compose`,
  `/usr/local/libexec/docker/cli-plugins/docker-compose` and `/usr/local/lib/docker/cli-plugins/docker-compose` (anything else:
  set `WHALENURSE_COMPOSE_BIN`).
  The stacks must be readable by the `whalenurse` user (`ProtectHome=read-only` lets it read under `/home`).
- The CLI needs the control socket, which the unit puts at `/run/whalenurse/whalenurse.sock` (the CLI's default is
  `/run/whalenurse.sock`), and which only the service user can open:
  `sudo -u whalenurse env WHALENURSE_CONTROL_SOCKET=/run/whalenurse/whalenurse.sock whalenurse status`.
- systemd stops whalenurse in the host's shutdown like everything else; it writes its `last_seen` heartbeat on a clean stop and
  every 5 s, which is how it recognises the containers that a shutdown stopped (and does not mistake them for crashes).
  A stop lets an action in flight finish (up to 30 s, then compose gets SIGTERM and, 10 s later, SIGKILL), so keep the
  unit's `KillMode=mixed` (systemd's default would SIGTERM compose itself) and `TimeoutStopSec=60s`. In a container the
  examples' `stop_grace_period: 60s` does the same; keep it well below 85 s, because a stop of the Docker daemon waits for
  the longest `stop_grace_period` plus 5 s, and systemd stops `docker.service` after 90 s by default.
- `Requires=docker.service` stops whalenurse when Docker is stopped (`systemctl stop docker` for maintenance), and
  `WantedBy=multi-user.target docker.service` starts it again when Docker starts. A unit from 0.1.2 or earlier has only
  `multi-user.target`: install the new file, then `sudo systemctl daemon-reload && sudo systemctl reenable whalenurse`
  (`reenable` adds the `docker.service.wants/` link).
- If a hardened setting gets in the way (an `EPERM` in the journal), loosen that one line and tell us in an issue: the filter
  fails with `EPERM`, not with a kill, so it shows.

## Checking that it works

```
docker exec whalenurse /whalenurse version
docker exec whalenurse /whalenurse status            # versions, the compose binary and where it came from, the rungs available
docker exec whalenurse /whalenurse notify test       # one message to every notification URL
docker logs whalenurse | head                        # "compose binary" with path, source, version; any warnings
```

`status` shows the **version gate**: with Engine 28.1+ and Compose v5.2.0+ all rungs are available; below that rungs 2 and 3 are
disabled and the ladder ends after the restart.
