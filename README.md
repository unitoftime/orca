# orca

Deploy your own services onto machines you own.

You describe services in YAML files, in a directory. `orca apply` runs them on
your server with HTTPS, DNS between services, logs, metrics, a firewall and
backups set up. It is built for one person running a handful of services on
one server, with room to grow to a few machines. There is no isolation between
services: it assumes you wrote all of them.

## What you need

- **A server** running Debian or Ubuntu on x86-64, reachable over SSH **as
  root**, with cgroups v2 (any current release). orca installs Docker and
  Nomad on it itself, from pinned versions; you install nothing by hand.
- **On your own machine:** Go 1.26 or newer, `ssh` and `rsync`.
- **For HTTPS:** DNS for each hostname your services use, pointing at the
  server. **For the web UIs:** a domain with a wildcard record
  (`*.example.com`) pointing there too. No domain? Your server's IP with
  dashes, under sslip.io, works as one (`203-0-113-10.sslip.io`), since
  every name beneath it resolves to that IP. It relies on sslip.io's DNS
  servers, and the names change if the IP does.

Install:

```
go install github.com/unitoftime/orca/cmd/orca@latest
```

To try it first, [example/](example/) is a small cluster (a web server, a
background worker, and Postgres with an app using it) that you point at your
server and bring up in a few commands.

## Setting up

A cluster is a directory. `cluster.yaml` at its root lists your machines, and
every directory beside it is a **group** of services:

```
infra/
  cluster.yaml
  notifier/
    notifier.yaml
  shop/
    db.yaml
    web.yaml
```

A minimal `cluster.yaml`:

```yaml
nodes:
  - host: root@203.0.113.10
    name: box0

monitoring:
  domain: example.com             # the dashboards: status.example.com and beside it
```

The dashboards are behind a password generated for the cluster when you
bootstrap it; `orca password` shows it, and `orca password set` changes it.
The user is `admin`.

Certificates come from Let's Encrypt and renew themselves; there is nothing
to sign up for.

Then bring the server up:

```
orca bootstrap                  # or: orca --ssh-copy-id bootstrap, for a fresh box
```

Bootstrap updates the OS, turns on automatic security updates, installs Docker
and Nomad, and caps Docker's own log files. It is safe to run again, and that
is how an orca upgrade reaches the server.

orca finds `cluster.yaml` by walking up from the current directory, like git
finds `.git`, so every command works from anywhere inside the tree. Use
`-C <dir>` to point it somewhere else.

## Describing services

Each `.yaml` file in a group directory holds one or more services, separated by
`---`.

```yaml
# shop/web.yaml
name: web
image: ghcr.io/you/shop:latest
cpu: 1                    # vCPU; default 0.5
memory: 1G                # always with a unit; default 512M
replicas: 2               # default 1; 2+ deploys with no downtime
env:
  LOG_LEVEL: info
  DATABASE_URL: postgres://postgres:${secret.db_password}@db:5432/postgres
ports:
  8080: shop.example.com  # https://shop.example.com
---
name: worker
image: ghcr.io/you/shop:latest
cmd: ./worker --queue default
```

The group is the directory name. Nested directories join with dashes, so
`blog/prod/` is the group `blog-prod`. A group named `orca` is reserved.

Unknown keys are errors. `orca validate` checks everything without touching a
server.

### Ports

`ports` lists what a service listens on and who can reach it. The key is the
port inside the container.

```yaml
ports:
  5432: internal              # other services only; never reachable from outside
  2112: metrics               # like internal, and scraped at /metrics
  8080: shop.example.com      # https://shop.example.com, with its certificate
  7777: tcp                   # a raw public port, no proxy in the way
  7777: udp:7778              # public port can differ from the container's
  7777: [tcp, udp]            # both protocols
```

(Those are alternatives: a port number appears once per service.)

- `internal` and hostname ports publish nothing on the server's public
  address; a hostname is reached through ingress.
- `tcp` and `udp` are the only way anything becomes directly reachable from
  the internet, and the firewall opens exactly those ports. Two services
  cannot claim the same public port; apply refuses before deploying.
- A service can have one hostname port, and one `metrics` port. The hostname
  can be on any domain; point its DNS at the ingress machine.
- 22 is SSH's, and 80 and 443 are ingress's while it runs; apply refuses a
  `tcp` port on any of them.
- A service with no `ports` listens for nothing, as most background workers
  do.

### Finding other services

Services reach each other by name:

| Name | From | Means |
|---|---|---|
| `db` | inside the same group | this group's `db` |
| `db.shop` | anywhere | the `db` service in group `shop` |

Any service can reach any other; groups are for naming, not security.

### Volumes

```yaml
volume:
  size: 20G
  mount: /data
```

A volume is a directory on the server, kept across deploys and restarts. A
service with a volume is pinned to the machine that holds it, and cannot have
more than one replica.

### The command

`cmd:` replaces the image's command **and its entrypoint**, and runs through
`/bin/sh -c`, so it can be a small script. That means an image with no shell
(`scratch`, distroless) cannot use `cmd:`; configure it through `env:`
instead.

### Variables

A value repeated across a group's files can be written once, in a `vars.yaml`:

```yaml
# blog/prod/vars.yaml
commit: 4f2a9c1
```

```yaml
# blog/prod/server.yaml
image: ghcr.io/you/blog-server:${var.commit}
```

Moving every service to the next build is then a one-line edit, and moving
back is reverting it.

- A `vars.yaml` applies to its directory and every directory below it, and
  the nearest one wins. One beside `cluster.yaml` is shared by every group.
- Variables fill in values only: an image, a hostname, a size, an env value.
  Never a key, never a block.
- Each is one value, and may not contain `$`.
- Using one that is not defined is an error. `orca validate` shows what each
  one became.
- An unquoted `${var.n}` takes the type of its value (`replicas: ${var.n}` is
  a number); a quoted one stays a string.

### Literal dollar signs

`${secret.NAME}` in an env value is a secret, and `${var.NAME}` anywhere is a
variable. Any other `${…}` in an env value is an error. Write `$$` for a
literal `$`, so `$${var.x}` is the text `${var.x}`.

## Deploying

```
orca plan              # show what would change; changes nothing
orca apply             # make the server match the files
orca apply shop        # only the shop group; everything else is left alone
```

- Every image is pinned to its current digest, so `image: foo:latest`
  redeploys when `latest` moves. CI can push an image and run `orca apply`
  without changing any files.
- Only what changed is redeployed.
- Apply waits up to two minutes for every service in scope to be healthy, and
  exits non-zero if any is not.
- Services no longer in the files are stopped, after asking. Without a
  terminal to ask at, apply refuses unless you pass `--yes`. Stopping never
  deletes data.

Running apply from CI needs SSH access to the server, and no secret values.

### Private images

apply looks up digests with your own Docker login, but the server pulls
the image, and it has no login of its own. Give the cluster one per registry:

```
orca registry login ghcr.io                          # prompts for username and token
echo "$TOKEN" | orca registry login ghcr.io -u you   # or the token on stdin
orca registry list
orca registry logout ghcr.io
```

- Nothing changes in your manifests: every image from a registry the cluster
  has a login for pulls with it, on every machine.
- Use a read-only token (for ghcr.io, a token with only `read:packages`).
  `login` checks it against the registry before storing it.
- apply refuses to deploy a private image whose registry has no login, naming
  it. It knows an image is private because the registry would not serve it
  without your credentials.
- Logging in again with a new token redeploys nothing; the next pull uses it.
- `orca registry login docker.io` also lifts Docker Hub's anonymous pull limit.
- CI still needs its own read access to resolve digests (`docker login` in
  the pipeline), exactly as before.

## Secrets

Declare what a service needs, then set the values:

```yaml
secrets:
  - apiToken                  # the file /secrets/apiToken
  - name: session_key
    env: SESSION_KEY          # an environment variable instead
  - name: gh_key
    path: github.pem          # the file /secrets/github.pem
```

```
orca secret set notifier/apiToken         # prompts without echo
orca secret set shop/gh_key < key.pem     # or reads all of stdin
orca secret list                          # what every manifest needs, and what is MISSING
orca secret rm notifier/apiToken
```

- Secrets are delivered as files by default.
- `${secret.NAME}` in an `env:` value splices a secret into a larger string.
- Values are stored only on the cluster. Apply refuses to deploy while a
  secret a service needs is unset.
- Setting a secret restarts the services using it; no redeploy needed.

## Databases and backups

A **template** is a service orca configures for you. Postgres:

```yaml
# shop/db.yaml
name: db
template: postgres:17       # or postgres:16
memory: 2G
volume: 20G
```

orca sets the image, port (5432, internal), volume mount and memory tuning,
and generates a password, `shop/db_password`, which other services use as
`${secret.db_password}`. You connect as the `postgres` user and create
databases and roles yourself.

### Backups

Declare an S3-compatible store (R2, Backblaze, Wasabi, MinIO, S3):

```yaml
# storage/offsite.yaml
name: offsite
target: s3
endpoint: https://<account>.r2.cloudflarestorage.com
bucket: orca-backups
# region: auto            # the default
```

```
orca secret set storage/offsite_key_id
orca secret set storage/offsite_secret_key
```

Then point a database at it:

```yaml
name: db
template: postgres:17
volume: 20G
backup:
  to: storage/offsite
  schedule: "0 3 * * *"     # cron; nightly at 03:00 by default
  keep: 14                  # the default
```

```
orca db list shop/db                    # backups held, oldest first
orca db restore shop/db                 # restore the newest (asks you to type "db")
orca db restore shop/db <backup-name>   # or a specific one
```

A backup that fails shows up as `failed` in `orca status`, and its output is in
`orca logs shop/db`.

### Redis

```yaml
# blog/redis.yaml
name: redis
template: redis:8.10
memory: 1G
volume: 10G
backup:
  to: storage/offsite       # optional, as with postgres
```

Redis run as a database, not a cache: writes go to disk every second and
nothing is evicted. The official Redis 8 image, so JSON, search, time series
and bloom filters are built in. orca generates a password, `blog/redis_password`:

```yaml
env:
  REDIS_ADDR: redis:6379
  REDIS_PASSWORD: ${secret.redis_password}
```

The dataset is capped at three quarters of `memory`; past that, writes fail
with an error your application sees rather than the container being killed.

`orca db list` and `orca db restore` work the same way, with one difference:
**restoring Redis stops it** while its data is replaced, then starts it again.
The data it replaced is kept under `/var/orca/pre-restore/` on the machine.

## Object storage

```yaml
# files/store.yaml
name: store
template: garage:2.3.0
memory: 512M
volume: 20G
```

An S3-compatible store on your server. orca creates a bucket named
`files-store` and generates an access key:

```yaml
env:
  S3_ENDPOINT: http://store.files:3900
  S3_REGION: orca
  S3_BUCKET: files-store
  S3_KEY_ID: ${secret.store_key_id}
  S3_SECRET: ${secret.store_secret_key}
```

## Seeing what is going on

```
orca top                             # every node, store and service, and its status
orca top -w                          # the same, refreshing every 5 seconds
orca status                          # every service: health, replicas, uptime, digest
orca status shop                     # one group
orca logs shop/web                   # the last hour of one service
orca logs shop                       # a whole group
orca logs web "connection refused"   # search
orca logs shop/web -f                # follow
orca logs shop/web --since 24h -n 500
```

A port declared `metrics` is scraped every 30 seconds, and its series carry
`group` and `service` labels. Query them at `metrics.<domain>`.

Every machine also reports its own CPU, memory, disks and network, as the
standard node exporter series (`node_cpu_seconds_total`,
`node_filesystem_avail_bytes`, …) labelled with the machine's `node` name.

A service is `running`, `pending` (starting or rolling out), `failed`, or
`unplaced` (accepted but with nowhere to run, with the reason). `status` also
lists data left behind by services you have removed.

Logs are kept for 14 days (10G at most) by default, and outlive the container
that wrote them.

With `monitoring.domain` set, there are also web UIs, behind a password: user
`admin`, and the password `orca password` prints, generated for the cluster at
bootstrap. `orca password set` changes it, from the next apply. `status.<domain>` links to the rest:

- `status.<domain>`: every node, store and service and its status (each
  node's CPU, memory, disks, network and last hour; how full the log and
  metric stores are; each service's health and memory against its limit).
  Click a service for its logs, with search and follow. `orca top` shows
  the same in a terminal, but for the logs
- `logs.<domain>`: search logs
- `metrics.<domain>`: query metrics
- `nomad.<domain>`: the Nomad UI, with what is running and what has restarted

## Removing things

| You want to… | Do this | Data |
|---|---|---|
| remove a service | delete it from its file, `orca apply` | kept |
| take a group down right now | `orca stop shop` | kept; the next apply brings it back |
| delete a group and its data for good | delete the directory, `orca apply`, then `orca purge shop` | **deleted** |

`purge` is the only command that deletes data. It only works on a group whose
directory is already gone, and asks you to type the group's name.

## Configuring the cluster

Everything is on by default. Turn a piece off with `false`, or adjust it:

```yaml
nodes:
  - host: root@203.0.113.10

ingress:                      # or `ingress: false` for a cluster with no web services
  acme_email: you@example.com # optional: a contact on the Let's Encrypt account
  https: false                # plain HTTP, for hostnames Let's Encrypt cannot reach

monitoring:                   # or `monitoring: false`
  domain: example.com         # the dashboards, at status.example.com and beside it
  node: box1                  # which machine; the first server if unset
  logs:                       # or `logs: false`
    retention: 14d
    disk: 10G                 # hard ceiling; the oldest logs are dropped first
  metrics:                    # or `metrics: false`
    retention: 30d
    min_free: 2G              # stop storing metrics below this much free disk
  status: false               # no status page

dns: false                    # services can no longer find each other by name
firewall: false               # leave the server's firewall to you
```

Removing a settings block keeps the default; only `false` turns something off.

## More than one machine

```yaml
nodes:
  - host: root@203.0.113.10
    name: box0
    private_ip: 10.0.0.10
  - host: root@203.0.113.11
    name: box1
    private_ip: 10.0.0.11
    role: client
```

- The machines must share a private network (your provider's, or
  Tailscale/WireGuard), and each needs `private_ip`, including the one that
  was running alone.
- Servers (`role: server`, the default for the first node) must be an odd
  number: 1, 3 or 5.
- Ingress and monitoring run on the first server unless `node:` under each
  puts them elsewhere. Every request goes through ingress, and every log line
  and metric through monitoring, so on a busy cluster they are worth putting
  on different machines. Your DNS points at ingress's machine.

  ```yaml
  ingress:
    node: box0
  monitoring:
    node: box1
  ```

  Choose before there is history to keep. Moving monitoring starts the stores
  empty on the new machine; the old data stays on the old machine's disk.
  Moving ingress means a DNS change, and new certificates, which it fetches
  itself.
- A service with a volume stays on the first server, where its data already
  is. `node: box1` puts it somewhere else.

Growing from one machine is the edit above, then:

```sh
orca bootstrap     # every machine: the new one joins, the first moves off loopback
orca apply         # re-places what has to move onto the private network
```

Between the two, services on the new machine cannot yet reach the old one's.

## Rough edges

- **More than one machine has not been run on real hardware yet.** It is built
  and tested against generated configuration, not against a second box.
- **Above one machine, a port number is taken per machine.** Two services that
  both listen on, say, 8080 cannot run on the same machine; orca's scheduler
  places them apart, or reports the second as `unplaced`.
- **A deploy restarts a one-replica service.** The old copy stops before the
  new one starts, so it is down for the image pull and startup. With
  `replicas: 2` or more, copies are replaced one at a time, and each leaves
  DNS and ingress 15 seconds before it is stopped. A service with a volume,
  and ingress itself, are always one copy.
- **Backups are dumps on a schedule**, not continuous. If the server dies, you
  lose everything since the last backup. Only Postgres and Redis are backed
  up; a plain service's `volume:` is not.
- **Garage is single-node.** No replication; back up what matters elsewhere.
- **HTTPS needs hostnames Let's Encrypt can reach.** It checks each one over
  port 80, so names that exist only in your own `/etc/hosts` cannot get
  certificates. `https: false` serves plain HTTP instead, the dashboard
  password included.
- **An older cluster with no `acme_email` switches to HTTPS on its next
  apply.** That setting used to be what turned HTTPS on; now HTTPS is on
  unless `https: false` says otherwise.
- **Secrets live only on the cluster.** Rebuild the cluster and you set them
  again; `orca secret list` shows which. The dashboard password is generated
  anew with it. A generated database password is the
  exception you cannot simply re-set: the database was initialised with it.
  `orca secret set` and `rm` refuse generated secrets unless you pass
  `--force`.
- **Any container can reach any other.** Don't run code you don't trust.
- **A service that logs its own environment** puts any environment-variable
  secret into the log store for 14 days. Prefer file secrets.
- **Metrics have no hard size limit**, only a retention time and a free-space
  floor. Logs and Docker's own log files do have hard limits.
- **Every apply talks to your image registry**, to pin digests. A registry
  outage blocks deploys.
- **An image written as a digest (`@sha256:…`) is never looked up**, so apply
  cannot tell whether it is private. If it is and the cluster has no login for
  its registry, the pull fails on the server instead.
- **A server set up by an older orca pulls without registry logins** until you
  run `orca bootstrap` again. `orca registry login` tells you which ones.
- **Only Debian/Ubuntu on x86-64** servers are supported.
- **The status page runs your orca binary on the server**, which apply ships
  there, so it has to be statically linked. `make build` is. A `go install`
  on Linux is not, so the first apply of each version builds a static copy
  from the module cache, which takes a little longer.
- **Internal ports can't be reached from your machine**, such as a profiler
  or a database console. For now that means an SSH tunnel to the allocation's
  address, which `orca status` does not show.
- **Upgrading a pinned image is an edit.** With the tag in a `vars.yaml` it
  is one line; nothing writes it for you. There is no rollback command:
  rolling back is reverting that edit.
- **Not built yet:** `orca exec` (a shell in a container), `orca forward`
  (tunnel to an internal port), and on-demand health checks. There is no alerting.

## Command reference

```
orca bootstrap [node]               set up every machine, or one
orca validate [group...]            check the files; touches no server
orca plan [group...]                show what apply would change
orca apply [group...] [--yes]       make the server match the files
orca status [group...]              what is running and whether it is healthy
orca top [-w] [--json]              machines and services at a glance
orca logs [target] [words] [-f] [--since 30m] [-n 200]
orca stop <group>                   stop a group, keep its data
orca purge <group> [--yes]          delete a removed group and its data
orca secret set <group>/<name> [--force]
orca secret list
orca secret rm <group>/<name> [--force]
orca registry login <host> [-u <user>]
orca registry list
orca registry logout <host>
orca db list <group>/<service>
orca db restore <group>/<service> [backup] [--yes]
orca nodes                          list the machines
orca version                        orca's version and what it installs

Global flags go before the command:
  -C <dir>          start the search for cluster.yaml here
  --ssh-copy-id     authorize your SSH key first (bootstrap on a fresh box)
```

## Further reading

- [spec/model.md](spec/model.md): how orca works and why each decision was
  made.
