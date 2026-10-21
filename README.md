# Anastasis

*Anastasis* (ἀνάστασις, Greek for "resurrection") is a container orchestrator for homelabs
and small servers: what dies comes back to life. It runs your services on rootless Podman and keeps
them at the size you asked for, without a root daemon and without the weight of a full cluster
platform.

## What it fixes

| Problem with plain container engines | What Anastasis does |
|---|---|
| A crashed container stays dead | Replaces dead or unhealthy replicas, with crash backoff |
| Scaling is manual and the load balancer does not follow | `anastasis scale`, plus autoscaling on in-flight requests and CPU, with a built-in HTTP load balancer that tracks every replica |
| One root daemon is a security risk and a single point of failure | Rootless Podman under your user; if Anastasis itself stops, containers keep running and are adopted on restart |
| Full cluster platforms are too heavy for one machine | One binary, one JSON project file, nothing else to install |

## Features

- **Replicas and self-healing** with crash backoff (1s doubling to 5m).
- **Zero-downtime rolling updates** (start-first by default) with pause or rollback on failure.
- **HTTP load balancer** per exposed service, with sticky sessions and connection draining.
- **Autoscaling** on concurrent requests and CPU, with a 5-minute scale-down window.
- **JSON project files** (`compose.json`, Compose-shaped); YAML Compose files load too.

## Status

Built and tested on a single host, with autoscaling: unit tests, integration tests on Docker, and
CI on rootless Podman. Anastasis uses only the Go standard library: its Podman API client, project
file loader and YAML reader are its own.

## Install

Requirements: Linux, rootless Podman 4+, and a systemd user session.

```sh
go build -o ~/.local/bin/anastasis ./cmd/anastasis
systemctl --user enable --now podman.socket
mkdir -p ~/.config/systemd/user && cp contrib/anastasis.service ~/.config/systemd/user/
systemctl --user enable --now anastasis
sudo loginctl enable-linger "$USER"   # keep running after logout; also needed for healthchecks
```

## Quick start

`compose.json`:

```json
{
  "name": "weather",
  "services": {
    "web": {
      "image": "ghcr.io/rationislabs/weather-live-dashboard:1.4.0",
      "healthcheck": {
        "test": ["CMD", "wget", "-qO-", "http://localhost:8080/health"],
        "interval": "5s"
      },
      "deploy": {
        "replicas": 3,
        "update_config": { "parallelism": 1, "delay": "5s" }
      },
      "x-anastasis": {
        "expose": "8000:8080",
        "sticky": true
      }
    }
  }
}
```

`expose` puts Anastasis's load balancer on host port 8000, forwarding to container port 8080.
`sticky` pins each browser to one replica. JSON is the default format; existing YAML Compose
files (`compose.yaml`) work too.

```sh
anastasis up                          # reads compose.json; waits until every service is ready
anastasis status
anastasis scale weather web=5         # lasts until the next `anastasis up`
anastasis rollout restart weather web # roll every replica, re-pull the image
anastasis down weather --volumes
```

## How it behaves

- **Self-healing:** exited or unhealthy replicas are replaced. A replica that
  dies within 30s of starting puts the service in crash backoff (1s doubling
  to 5m). Replicas that died while Anastasis was not running (a reboot) are
  replaced without backoff.
- **Hung replicas:** a replica whose healthcheck gives no result for
  `interval` + `timeout` stops getting traffic, and is replaced after
  `retries` such periods. Podman 4.9 never times out a hung check itself.
- **Rolling updates** start when a service's config changes. Anastasis defaults
  to `order: start-first`, unlike Compose's `stop-first`, so a single-replica
  service updates without downtime. If new replicas fail within `monitor`
  (default 30s), `failure_action` decides: `pause` (default), `rollback` or
  `continue`.
- **Load balancer:** HTTP only, round-robin over ready replicas. Replicas
  without a healthcheck count as ready as soon as they start.
- **Sticky sessions** keep a browser on one replica. Apps that keep sessions
  in memory (NiceGUI, Reflex) need this; if that replica dies, the user's
  session is lost.
- **Stateless services only scale.** Services that keep state in a local
  volume (SQLite, files) must stay at one replica.
- **Keep state in named volumes or bind mounts.** Anonymous volumes (an
  image's `VOLUME`, such as redis `/data`, or a `volumes:` entry without a
  source) belong to one replica and are deleted with it, so every
  replacement, rollout or scale-down starts them empty. Compose keeps them
  across a recreate; Anastasis does not.
- `ports:` still works for single-replica services, which then update with
  `stop-first` (short outage). Use `x-anastasis.expose` instead to scale and to
  update without downtime.

## Autoscaling

```json
{
  "services": {
    "web": {
      "image": "ghcr.io/rationislabs/weather-live-dashboard:1.4.0",
      "deploy": {
        "replicas": 2,
        "resources": { "limits": { "cpus": "0.5" } }
      },
      "x-anastasis": {
        "expose": "8000:8080",
        "autoscale": { "min": 1, "max": 6, "inflight": 10, "cpu": 70 }
      }
    }
  }
}
```

`replicas` is the starting count (and the floor when `min` is omitted), `inflight` the target
concurrent requests per replica, `cpu` the target percent of the CPU limit per replica.

- Anastasis computes a replica count from each signal (`ceil(ready × observed / target)`) and uses
  the higher one, within `[min, max]`.
- Scaling up starts within about 15 seconds, plus the time new replicas take to start and become
  ready. Scaling down waits until the load has been lower
  for 5 minutes, then removes one replica per minute.
- Missing data never scales down. `cpu` needs `deploy.resources.limits.cpus`; on rootless Podman
  it also needs cgroup v2 (on cgroup v1 the CPU signal is unavailable and `anastasis status` says so).
- `inflight` needs `x-anastasis.expose`: only Anastasis's load balancer counts requests.
- A WebSocket or server-sent-events connection counts as in flight for as long as it stays open,
  so a live dashboard with open tabs never scales down on `inflight`. Scale such apps on `cpu`.
- `anastasis scale` refuses autoscaled services; change `min`/`max` in the file and run `anastasis up`.
- `anastasis status` shows the decision in the `AUTO` column, for example
  `2→4 (inflight 14.0/10, cpu 55/70)`.

## Not supported

`build:`, `network_mode:`, custom networks, `restart: "no"` and
`restart: on-failure` (Anastasis keeps every service running; run one-shot jobs
with `podman run`), bind `propagation`, volume `nocopy`/`subpath`, tmpfs
options, Swarm-only `deploy` keys
(`placement`, `mode: global`, `endpoint_mode`, `rollback_config`,
`max_failure_ratio`), and any service key not listed in
`serviceKeys` in `internal/config/service.go`. Anastasis rejects these with an error instead of
ignoring them.

`container_name:` and `depends_on:` are accepted and ignored: Anastasis names
replicas itself and retries until dependencies are up. `depends_on:` must still
name services of the project.

## Gotchas

- **One project name, one directory.** A project name registered from one
  directory cannot be `up`-ed from another (two `docker/compose.json` stacks
  would both be named `docker`). Give them different top-level `name` keys, or
  run `anastasis down` before moving a project. Another file in the same
  directory is the same project, so `compose.yaml` can become `compose.json`
  with a plain `anastasis up`.
- **`down --volumes`** removes the project's named volumes, never `external:` ones. A volume
  belongs to one project: `up` refuses a project that names another project's volume. To share
  one, mark it `external: true` in both projects.
- **A project file is trusted like a script.** Bind mounts of any host path and `cap_add` are
  passed to Podman as written; rootless Podman limits them to what your user can do, but a bind
  of your home directory can still read your files. `privileged`, `devices` and `pid` are
  rejected.

- **Healthchecks need a systemd user session** on rootless Podman. Without
  `loginctl enable-linger`, replicas stay `starting` and never get traffic;
  `anastasis status` warns about this.
- **Ports below 1024** need `sysctl net.ipv4.ip_unprivileged_port_start=80`
  (or lower). Anastasis does not change it for you.
- **Use fully qualified image names** (`docker.io/library/nginx`), because
  Podman may refuse to guess the registry for short names.

## Development

```sh
go test ./...
# integration tests against Docker (or rootless Podman: leave the variables unset)
ANASTASIS_TEST_RUNTIME=/var/run/docker.sock ANASTASIS_TEST_CLI=docker \
  go test -tags integration -count=1 -timeout 15m ./integration/
```

## Credits

Anastasis is written from scratch and uses only the Go standard library. It runs containers
through [Podman](https://podman.io) (Apache-2.0), a separate program you install, over Podman's
Docker-compatible API. Project files follow the [Compose specification](https://compose-spec.io)
shape; the loader that reads them is Anastasis's own.

## License

Apache License 2.0; see [LICENSE](LICENSE).
