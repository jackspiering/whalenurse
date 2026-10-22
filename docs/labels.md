# Labels

whalenurse reads these labels from the containers it watches. In a compose file they go under the service:

```yaml
services:
  web:
    labels:
      whalenurse.enable: "true"
```

A boolean label accepts `1`, `t`, `T`, `TRUE`, `true`, `True` and `0`, `f`, `F`, `FALSE`, `false`, `False` (Go's
`strconv.ParseBool`), with surrounding spaces ignored. An invalid value falls back to the default and logs one warning for that container; it never opts a container in or out by
accident.

| Label | Values | Default | What it does |
|---|---|---|---|
| `whalenurse.enable` | `true`, `false` | — | `true` opts the container in (with `WHALENURSE_MODE=label`, the default, nothing is watched without it). `false` leaves the container alone entirely, **also as the sidecar of an opted-in parent** (see below). |
| `whalenurse.actions` | `restart`, `recreate`, or `restart,recreate` | `restart,recreate` | The rungs of the ladder, in order. `restart` alone never recreates the container: after a failed restart it is SUSPENDED. `pull` is not accepted here; use `whalenurse.heal.pull`. |
| `whalenurse.heal.pull` | `true`, `false` | `false` | Appends rung 3: after restart and recreate failed, pull a newer image and recreate from it. **Needs `recreate` in `whalenurse.actions`** (else one warning and the pull stays off). Never runs for a service with `pull_policy: never` or `build`. A private registry needs `WHALENURSE_REGISTRY_AUTH_DIR` ([security.md](security.md#private-registries)). It is the only thing in whalenurse that fetches an image. While it stays on, a pasted rollback lasts only until the next heal reaches the pull again: to keep an old image, pin it in the compose file or remove this label ([running.md](running.md)). |
| `whalenurse.heal-exited` | `crashed`, `always`, `never` | `crashed` | What to do with a container that has *exited*. `crashed`: heal one that crashed (a non-zero exit other than 137/143, an OOM kill, a `die` nobody asked for); leave an intended exit (`restart: "no"`/`on-failure` with exit 0) and a stop by hand alone. `always`: heal any exited container, including one a host shutdown stopped and Docker's restart policy did not restore. `never`: notify only. |
| `whalenurse.cooldown` | a positive duration | `WHALENURSE_COOLDOWN` | Overrides the base cooldown between rungs for this container (it still doubles per rung, capped at 5 minutes; a value above 5 minutes acts as 5 minutes). |
| `whalenurse.flap-limit` | a positive integer | `WHALENURSE_FLAP_LIMIT` | Overrides the flap limit for this container. |

## Sidecars

A container with `network_mode: service:<parent>` (or `container:<parent>`) breaks whenever its parent restarts or is
recreated. For an **opted-in** parent, whalenurse repairs such sidecars after it has healed the parent, even if they carry
no `whalenurse.enable` label, unless they are labelled `whalenurse.enable=false`. Only that repair needs no opt-in: a sidecar
that is unhealthy or exited *on its own* is healed only if it is opted in itself. See the README for what "repair" does.

## Labels from the image

whalenurse reads a container's labels, and Docker puts an image's own labels (its `LABEL` lines) on every container made
from it, for each key the container is not given itself. So an image can carry `whalenurse.enable=true` and opt itself in
under `WHALENURSE_MODE=label`, and it can set `whalenurse.heal.pull`, `whalenurse.heal-exited` (with `always`, a container you
stopped by hand is started again), `whalenurse.flap-limit` or the compose labels below. whalenurse cannot tell these labels
from the ones you wrote.

To see what an image brings:

```sh
docker image inspect -f '{{json .Config.Labels}}' <image>
```

A label you set on the service wins over the image's, so to override one, set it yourself, for example
`whalenurse.enable: "false"`.

## Labels whalenurse reads from compose

The recreate rung uses the labels compose put on the container, not your current compose file: `com.docker.compose.project`,
`.project.working_dir`, `.project.config_files`, `.project.environment_file`, `.service`, `.config-hash`, `.image` and
`.version`. A container without them, normally one compose did not make, only gets the restart rung. A container can carry
them without compose, though (from its image, see [Labels from the image](#labels-from-the-image), or set by hand), and then
it is treated as a compose container. A project or service label that is not a name compose would write is refused, and
that container gets the restart rung only.

Also read: `com.docker.compose.container-number` (part of the state key `host/project/service/number`, which survives a
recreate), `com.docker.compose.oneoff` (`True` for a `docker compose run` container, which is never touched),
`com.docker.compose.depends_on` (a sidecar's parent services; compose empties it when it recreates a container with
`up --no-deps`, so whalenurse remembers the parents it has seen instead of relying on it) and `com.docker.swarm.task.id` (a swarm
task, never touched).

## Per-container state, not labels

Pausing and unsuspending are done with the CLI (`docker exec whalenurse /whalenurse pause <project>[/<service>] --for 2h`,
`unsuspend`), not with labels; see the README.
