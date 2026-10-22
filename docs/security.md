# Security

To report a vulnerability, see [SECURITY.md](../SECURITY.md). This page is what you should know before you run whalenurse.

## The Docker socket is root on your host

whalenurse needs the Docker API to restart, start and recreate containers. **Anything that can talk to `docker.sock` can start
a privileged container that mounts `/`, so access to the socket is root-equivalent on the host.** That is true of whalenurse
as it is of Watchtower, Portainer, Traefik's Docker provider and every other tool that mounts it, and it is the first thing
to weigh when you decide to run it. Under systemd the same holds for a user in the `docker` group.

What whalenurse does with that power, so you can judge it:

- It acts only on containers that are opted in (`whalenurse.enable=true`, or every container with `WHALENURSE_MODE=all`
  minus those labelled `false`), and never on itself, on `docker compose run` containers or on swarm tasks.
- Its own Docker API calls are reads (`ping`, `list`, `inspect`, `events`, `version`, `image inspect`) and two actions, `restart` and `start`. Recreates and pulls go through the `docker compose`
  binary with a fixed set of commands: `up -d --no-deps --pull never --no-build <service>` (always with `--force-recreate`
  for rung 2; for the pull rung and a sidecar's repair only when a post-check finds the old container still there),
  `pull --policy always <service>` for the opt-in pull rung, `config` and dry-runs for the drift guard. The `compose`
  package cannot express `down`, `rm`, `build` or an `up` that could pull.
- The compose files it runs are the ones **recorded on the container** (`com.docker.compose.project.config_files`), read from
  the stacks you mount. Those labels are not only written by whoever creates the container: Docker also copies an image's
  own labels onto its containers, so an image can ship compose labels too. The project and service names go onto the compose
  command line, so whalenurse refuses a container whose `com.docker.compose.project` is not a name compose would write
  (lowercase letters, digits, `_` and `-`, starting with a letter or digit) or whose `com.docker.compose.service` is not
  (letters, digits, `.`, `_` and `-`, not starting with `-`): it gets the restart rung only, and the recreate and pull rungs
  are reported as unavailable. In 0.1.0 and 0.1.1 a service label such as `--dry-run=false` was passed to compose as a flag
  and could turn the drift guard's dry-run into a real `up` of the project.

- **The image's publisher is a second source of labels.** Docker puts an image's own labels on every container made from
  it, for each key the container is not given itself, so the publisher of an image you run can set `whalenurse.*` labels as
  well as you can: `whalenurse.enable=true` opts the container in under the default `WHALENURSE_MODE=label`,
  `whalenurse.heal.pull=true` lets whalenurse pull a newer image for it, `whalenurse.heal-exited=always` starts it again after
  a `docker stop`, and a high `whalenurse.flap-limit` lets it use more of the host-wide rate limit. whalenurse cannot tell
  these from labels you wrote. The publisher already runs code in that container, so this adds little power, but it does mean
  an image can opt itself in. Check what an image brings with `docker image inspect -f '{{json .Config.Labels}}' <image>`;
  a label you set on the service wins (see [Labels from the image](labels.md#labels-from-the-image)).

How to reduce the exposure:

- Run the container with `read_only: true`, `cap_drop: [ALL]` and `security_opt: [no-new-privileges:true]` (the examples do).
  The image is `distroless/static:nonroot` (uid 65532, no shell); give it the socket's group with `group_add` instead of
  running it as root.
- Mount the stacks and the compose plugin **read-only** (`:ro`): recreate works with read-only mounts.
- Keep `/data` as its own volume: it holds the store. Besides the store, whalenurse writes its control socket and an empty
  private directory it gives compose as `DOCKER_CONFIG` (compose v5.2.0 to v5.5.x write buildx state there during the drift
  guard's dry-run); in the container both are on the `/run` tmpfs.
- A socket proxy that allows only what whalenurse needs is on the roadmap but adds little today: compose itself needs broad
  API access to recreate.

## The dashboard and the API

The dashboard is **off** unless you set `WHALENURSE_HTTP_ADDR`. It is **read-only**: only GET and HEAD are served (everything
else is 405, before any token check), there is no endpoint that changes anything, and the commands that do (`pause`,
`resume`, `unsuspend`) exist only on a unix socket inside the container. It still shows your container names, states, image
names and the event history, so keep it off the open network.

**Where to expose it, in order of preference:**

1. **`127.0.0.1` only.** `ports: ["127.0.0.1:8484:8484"]` with `WHALENURSE_HTTP_ADDR=:8484`. No token is needed on loopback.
   Reach it with an SSH tunnel (`ssh -L 8484:127.0.0.1:8484 host`).
2. **A reverse proxy that terminates TLS** (Caddy, Traefik, nginx) on a Docker network shared with whalenurse, with a token
   set and `WHALENURSE_HTTP_TRUSTED_PROXIES` set to the proxy's network.
3. **A Tailscale container on a shared bridge network** that forwards with `tailscale serve` (see
   [examples/full](../examples/full/compose.yaml)). whalenurse publishes no port at all and is reachable over HTTPS on your
   tailnet only. Serve is a reverse proxy too: set `WHALENURSE_HTTP_TRUSTED_PROXIES` to the shared network (the example
   gives it a fixed subnet for this). **Never `network_mode: service:tailscale`**: whalenurse would become a dependent of a container it may heal,
   and the sidecar machinery would then restart or recreate it with its parent.

Never publish the port on `0.0.0.0` of a machine on an untrusted network. whalenurse warns at startup when the listener is
not loopback and no token is set.

**The token** (`WHALENURSE_HTTP_TOKEN`):

- Sent as `Authorization: Bearer <token>`, or as the password of HTTP Basic with any user name (a browser cannot send a bearer
  header on a page load, so it asks for a password once). It is **never read from the URL**, where it would end up in logs
  and in `Referer`.
- Both forms are **clear text on the wire**: use them only over TLS (a proxy or Tailscale Serve) or on loopback.
- Compared in constant time, after hashing both sides.
- Use a long random token (`openssl rand -hex 24`). Under 16 characters logs a warning.
- `/healthz` needs no token and says only `ok` or `not ok`.
- `/metrics` is off unless `WHALENURSE_METRICS=true`, and then behind the token.

**Without a token** only the Host names `localhost`, `127.0.0.1` and `[::1]` (and `WHALENURSE_HTTP_ALLOWED_HOSTS`) are
served; others get 403. That stops a web page from reading the dashboard through DNS rebinding in a browser on your network.
It does not stop a program that sets any Host it likes, so a listener that other machines can reach needs a token.

### The wrong-token limit and reverse proxies

Ten wrong tokens within a minute from one client address block that address: 429 with `Retry-After`, for one minute, then
2, 4 ... up to 15 minutes for each further block (the count is forgotten after an hour without a failure). While blocked,
even the right token gets 429. A request with no credentials at all (a browser before its password prompt) is not counted.

The client address is the **peer address of the connection**. `X-Forwarded-For` is ignored, because a client can forge it.
Behind a reverse proxy that peer is the proxy, so all users share one budget and someone guessing wrongly through it locks
everybody out for a minute. Fix that with **`WHALENURSE_HTTP_TRUSTED_PROXIES`**: list the proxy's address or network
(`172.18.0.0/16`) and whalenurse reads `X-Forwarded-For` **only on connections from those addresses**, taking the right-most
entry that is not itself a trusted proxy, which is the address your proxy appended and not anything the client wrote.
Requirements: the proxy must set or append `X-Forwarded-For` (nginx `proxy_add_x_forwarded_for`; Caddy and Traefik do by
default), and nothing but the proxy may reach the listener, or a client that connects directly is simply keyed on its own
address (it cannot borrow another's). Do not trust a network that clients themselves sit in. A list that would trust every
address (`0.0.0.0/0`) is refused.

### What is never shown

One place decides what leaves whalenurse, for every page, endpoint and the event stream: environment variables, commands,
mounts and full inspect output are not even read into the objects the API uses; labels shown are only `com.docker.compose.*`
and `whalenurse.*`; event attributes are an allow-list; and every message, attribute and start error goes through the same
redactor as the notifications, which also knows the HTTP token. A test plants ten secrets and greps every page, endpoint,
the stream and `/metrics` for them. Host paths in the compose labels and in the rollback commands are shown on purpose.

Every response carries a strict `Content-Security-Policy` (no inline script or style, no form, no frame, no third-party
origin), `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`.

## Notification URLs and the uptime ping are secrets

A Discord webhook, a Telegram bot token or a private ntfy topic **is** a credential. whalenurse treats
`WHALENURSE_NOTIFY_URLS` and `WHALENURSE_HEARTBEAT_URL` as secrets: they are never logged, never in `status`, `/healthz` or an
error (a URL is shown as `#2 discord`), and every error text passes a redactor that masks them in all their forms. That
includes the text of events (an error that Docker or compose quoted) in the log and in every notification body, so one
URL's token is never pushed to another URL's service. A rollback command that the redactor would change is not sent at
all: the message says it was withheld. Tests grep the logs, the bodies sent, the status and the test output for them.

Your part: keep them in an env file that stays out of git (`.env` is in the repository's `.gitignore`), with mode 0600; use an
unguessable ntfy topic; and remember that `docker inspect whalenurse` shows the environment of the container to anyone who
can use Docker (`docker compose config` shows it too).

## Private registries

By default the pull rung cannot log in to a private registry: the compose child process never reads a Docker config (its
`DOCKER_CONFIG` is an empty directory whalenurse creates for itself, so `$HOME/.docker` is not read either, and
`DOCKER_AUTH_CONFIG` is removed) and the image carries no credentials.

`WHALENURSE_REGISTRY_AUTH_DIR` turns it on, deliberately. **The risk:** the credentials then sit in a container that holds the
Docker socket. The socket is root on the host, so whoever controls whalenurse already controls the host; what is added is that
the registry credentials can be read inside the container, and by anyone who can read the directory on the host.

- Use a **read-only, pull-only token** scoped to the repositories you need (a registry access token, a GitHub token with
  `read:packages`), not a personal password and not your own `~/.docker/config.json`.
- The file holds only `"auths"` with inline credentials (`auth`, `username` with `password`, or `identitytoken`). Credential
  helpers do not exist in the image, and a `currentContext` or `proxies` key would apply to the pull too, so any other key stops
  whalenurse at startup.
- Mode `0400`/`0440` on the file, owned by uid 65532 (container) or by the `whalenurse` user (systemd); mount the directory
  `:ro`. whalenurse warns at startup when it can write to the directory or the file.
- The pull is the only command that gets it. The credentials are redacted from logs, `status`, the API, the dashboard and
  notifications like the notification URLs are; `status`, the API and the home page name the registries (hosts), never a user
  name or a secret.
- Only the pull rung uses it, and only for a container with `whalenurse.heal.pull=true`. Nothing is pulled otherwise.

## Supply chain

Every release is signed and carries provenance, so you can check that what you run was built by this repository's release
workflow: [verifying-releases.md](verifying-releases.md). The image bundles a copy of the Docker Compose plugin, downloaded in
the build and checked against a pinned sha256; the base images and the BuildKit image that builds the release are pinned by
digest, and the Dockerfiles name no external frontend (`# syntax=`). Dependabot proposes updates weekly; all GitHub Actions
are pinned to commit SHAs.

## A checklist

- [ ] The socket mount is the only privileged thing, and the container is `read_only` with `cap_drop: [ALL]`.
- [ ] Stacks and the compose plugin are mounted `:ro`.
- [ ] The dashboard is on `127.0.0.1`, behind a TLS proxy with a token, or on Tailscale on a shared bridge network.
- [ ] `WHALENURSE_HTTP_TOKEN` is at least 16 random characters and lives in an env file.
- [ ] Notification URLs live in an env file; `notify test` works.
- [ ] You started in dry-run and read the decisions before letting it act.
