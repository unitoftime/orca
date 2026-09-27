# orca

Deploy your own services onto servers you own.

You describe each service in a small YAML file. `orca apply` runs them on your
server, with HTTPS, service discovery, logs, metrics, a firewall and database
backups already set up.

orca is built for one person running a handful of services on one server, with
room to grow to a few machines. It assumes you wrote everything you run: there
is no isolation between services.

## Quick start

You need:

- **A server** running Debian or Ubuntu on x86-64 that you can SSH into as
  root. orca installs everything else on it (Docker, Nomad).
- **On your own machine:** Go 1.26 or newer, `ssh` and `rsync`.
- **A domain** pointing at the server, for HTTPS. No domain? Use your server's
  IP with dashes under sslip.io, such as `203-0-113-10.sslip.io`. Every name
  under it resolves to that IP.

Install orca:

```
go install github.com/unitoftime/orca/cmd/orca@latest
```

The fastest way to try it is the [example cluster](example/): copy it, change
three values to point at your server, and run two commands.

## How a cluster is laid out

A cluster is a directory. `cluster.yaml` at the top lists your machines. Every
directory next to it is a **group** of services, named after the directory:

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
  domain: example.com     # dashboards at status.example.com
```

Set up the server, then deploy:

```
orca bootstrap            # on a fresh server: orca --ssh-copy-id bootstrap
orca apply
```

`bootstrap` updates the OS, turns on automatic security updates, and installs
Docker and Nomad. It is safe to run again; that is also how you upgrade orca on
the server.

Like git, orca finds `cluster.yaml` by looking upward from where you are, so
every command works from anywhere inside the directory. `-C <dir>` points it
somewhere else.

## Writing a service

Each `.yaml` file in a group holds one or more services, separated by `---`:

```yaml
# shop/web.yaml
name: web
image: ghcr.io/you/shop:latest
cpu: 1                    # vCPUs; default 0.5
memory: 1G                # default 512M
replicas: 2               # default 1; with 2 or more, deploys have no downtime
env:
  LOG_LEVEL: info
  DATABASE_URL: postgres://postgres:${secret.db_password}@db:5432/postgres
ports:
  8080: shop.example.com  # served at https://shop.example.com
---
name: worker
image: ghcr.io/you/shop:latest
cmd: ./worker --queue default
```

Nested directories join with dashes: `shop/prod/` is the group `shop-prod`. The
group name `orca` is reserved for orca's own services.

Unknown keys are errors. `orca validate` checks every file without touching the
server.

### Ports

The key is the port inside the container. The value says who can reach it:

| Value | Who can reach it |
|---|---|
| `internal` | other services only |
| `metrics` | other services only; also collected as metrics from `/metrics` |
| a hostname, like `shop.example.com` | the internet, over HTTPS |
| `tcp` or `udp` | the internet, directly on that port |

```yaml
ports:
  5432: internal
  2112: metrics
  8080: shop.example.com
  7777: tcp                # the same port number on the server
  7778: udp:9000           # or a different one
  7779: [tcp, udp]         # both protocols
```

- Nothing is reachable from the internet unless a port says so, and the
  firewall opens only those ports.
- A service can have one hostname port and one `metrics` port. Point the
  hostname's DNS at your server.
- Two services cannot use the same public port, and TCP ports 22, 80 and 443
  are taken by SSH and HTTPS. `orca apply` stops and tells you before
  deploying.
- A service with no `ports` is not reachable at all, which is right for most
  background workers.

### Finding other services

Services reach each other by name:

| Name | Works from | Means |
|---|---|---|
| `db` | the same group | this group's `db` service |
| `db.shop` | anywhere | the `db` service in group `shop` |

Groups are for naming, not security: any service can reach any other.

### Volumes

```yaml
volume:
  size: 20G
  mount: /data
```

A volume is a directory on the server that survives deploys and restarts. A
service with a volume stays on the machine that holds it, and has one replica.

### Commands

`cmd:` replaces the image's command and its entrypoint. It runs with
`/bin/sh -c`, so it can be a small script. Images without a shell (`scratch`,
distroless) can't use `cmd:`; configure them with `env:` instead.

### Variables

To write a value once and use it in several files, put it in a `vars.yaml`:

```yaml
# shop/vars.yaml
commit: 4f2a9c1
```

```yaml
# shop/web.yaml
image: ghcr.io/you/shop:${var.commit}
```

Moving every service to a new build is then a one-line change, and rolling
back is undoing it.

- A `vars.yaml` applies to its directory and everything below it. The closest
  one wins. One next to `cluster.yaml` applies to every group.
- Variables fill in values only (an image, a hostname, a size), never keys.
- Using an undefined variable is an error. `orca validate` shows what each one
  became.
- Write `$$` for a literal `$`.

## Deploying

```
orca plan                 # show what would change, without changing anything
orca apply                # make the server match your files
orca apply shop           # only the shop group; nothing else is touched
```

- Only what changed is redeployed.
- Each image is pinned to its current digest. If you use `image: foo:latest`,
  pushing a new `latest` and running `orca apply` deploys it, with no file
  changes. That makes CI simple: push the image, run `orca apply`.
- Apply waits up to two minutes for services to become healthy, and exits with
  an error if any don't.
- Services you delete from your files are stopped, after asking. In CI, where
  nobody can answer, pass `--yes`. Stopping never deletes data.

Running apply from CI needs SSH access to the server. It never needs your
secret values.

### Private images

orca checks images with your own Docker login, but the server needs its own to
pull them. Give it one per registry:

```
orca registry login ghcr.io                          # prompts for username and token
echo "$TOKEN" | orca registry login ghcr.io -u you   # or pass the token on stdin
orca registry list
orca registry logout ghcr.io
```

- Use a read-only token (on GitHub, one with only `read:packages`).
- Your service files don't change: any image from that registry is pulled with
  the login.
- If an image is private and the cluster has no login for it, apply says so
  before deploying.
- `orca registry login docker.io` also lifts Docker Hub's anonymous pull limit.

## Secrets

List the secrets a service needs, then set their values:

```yaml
secrets:
  - apiToken                  # appears as the file /secrets/apiToken
  - name: session_key
    env: SESSION_KEY          # or as an environment variable
  - name: gh_key
    path: github.pem          # or as the file /secrets/github.pem
```

```
orca secret set notifier/apiToken         # prompts, without echo
orca secret set shop/gh_key < key.pem     # or reads the value from stdin
orca secret list                          # every secret, and which are MISSING
orca secret rm notifier/apiToken
```

- Values are stored only on the cluster, never in your files.
- `${secret.NAME}` in an `env:` value puts a secret inside a longer string,
  like a database URL.
- Apply refuses to deploy a service while one of its secrets is unset.
- Setting a secret restarts the services that use it. No redeploy needed.
- Secrets are files by default because environment variables leak easily (into
  logs, crash reports, child processes).

## Databases and backups

A **template** is a service orca sets up for you. For Postgres:

```yaml
# shop/db.yaml
name: db
template: postgres:17       # or postgres:16
memory: 2G
volume: 20G
```

orca picks the image, port (5432, internal only), storage and memory settings,
and generates a password. Other services in the group use it as
`${secret.db_password}`. You connect as the `postgres` user and create your own
databases and roles.

### Backups

First, add somewhere to put them: any S3-compatible storage (R2, Backblaze,
Wasabi, MinIO, S3).

```yaml
# storage/offsite.yaml
name: offsite
target: s3
endpoint: https://<account>.r2.cloudflarestorage.com
bucket: orca-backups
```

```
orca secret set storage/offsite_key_id
orca secret set storage/offsite_secret_key
```

Then point the database at it:

```yaml
name: db
template: postgres:17
volume: 20G
backup:
  to: storage/offsite
  schedule: "0 3 * * *"     # cron; nightly at 03:00 is the default
  keep: 14                  # the default
```

```
orca db list shop/db                    # the backups held, oldest first
orca db restore shop/db                 # restore the newest
orca db restore shop/db <backup-name>   # or a specific one
```

A failed backup shows as `failed` in `orca status`, with its output in
`orca logs shop/db`.

A Postgres backup covers the default `postgres` database only. Databases you
add yourself with `CREATE DATABASE` are not backed up.

### Redis

```yaml
# shop/redis.yaml
name: redis
template: redis:8.10
memory: 1G
volume: 10G
backup:
  to: storage/offsite       # optional, as with Postgres
```

Redis is set up as a database, not a cache: it writes to disk every second and
never throws data away. JSON, search, time series and bloom filters are built
in. Use the generated password like this:

```yaml
env:
  REDIS_ADDR: redis:6379
  REDIS_PASSWORD: ${secret.redis_password}
```

Data is capped at three quarters of `memory`. Past that, writes return an
error instead of the container running out of memory.

Backups work as with Postgres, but **restoring Redis stops it** while the data
is swapped. The old data is kept in `/var/orca/pre-restore/` on the server.

## Object storage

```yaml
# files/store.yaml
name: store
template: garage:2.3.0
memory: 512M
volume: 20G
```

An S3-compatible store on your own server, using Garage. orca creates a bucket
named after the group and service (`files-store`) and an access key:

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
orca top                             # every machine and service at a glance
orca top -w                          # the same, refreshing every 5 seconds
orca status                          # each service's health, replicas and uptime
orca status shop                     # one group
orca logs shop/web                   # the last hour of one service
orca logs shop                       # a whole group
orca logs web "connection refused"   # search
orca logs shop/web -f                # follow
orca logs shop/web --since 24h -n 500
```

A service is `running`, `pending` (starting or deploying), `failed`, or
`unplaced` (there is no machine it fits on; the reason is shown). `orca status`
also lists data left behind by services you removed.

Logs are kept for 14 days (up to 10G) and outlive the container that wrote
them. Metrics are kept for 30 days.

With `monitoring.domain` set, there are web dashboards too. Log in as `admin`
with the password from `orca password`:

- `status.<domain>`: machines and services, their health and resource use,
  and each service's logs
- `logs.<domain>`: search logs
- `metrics.<domain>`: query metrics
- `nomad.<domain>`: the Nomad UI

`orca password set` changes the password at the next apply.

## Removing things

| To | Do this | Data |
|---|---|---|
| remove a service | delete it from its file, `orca apply` | kept |
| take a group down now | `orca stop shop` | kept; the next apply starts it again |
| delete a group and its data | delete its directory, `orca apply`, then `orca purge shop` | **deleted**, with its secrets |

`purge` is the only command that deletes data. It only works once the group's
directory is gone, and asks you to type the group's name.

## Configuring the cluster

Everything is on by default. Turn a piece off with `false`, or adjust it:

```yaml
nodes:
  - host: root@203.0.113.10

ingress:                      # or `ingress: false` if nothing is served over HTTP
  acme_email: you@example.com # optional contact for Let's Encrypt
  https: false                # plain HTTP, for names Let's Encrypt can't reach

monitoring:                   # or `monitoring: false`
  domain: example.com         # dashboards at status.example.com
  node: box1                  # which machine runs it; the first by default
  logs:                       # or `logs: false`
    retention: 14d
    disk: 10G                 # hard limit; the oldest logs go first
  metrics:                    # or `metrics: false`
    retention: 30d
    min_free: 2G              # stop storing metrics below this much free disk
  status: false               # no status page

dns: false                    # services can't find each other by name
firewall: false               # manage the firewall yourself
```

Leaving a block out keeps its defaults. Only `false` turns something off.

HTTPS certificates come from Let's Encrypt and renew themselves. There is
nothing to sign up for.

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

- The machines need a private network between them (your provider's, or
  Tailscale or WireGuard), and every node needs a `private_ip`.
- Servers (`role: server`, the default) must be an odd number: 1, 3 or 5.
- HTTPS (ingress) and monitoring run on the first server. Use `node:` under
  `ingress` or `monitoring` to move them. Your DNS points at the ingress
  machine.
- A service with a volume stays where its data is. `node: box1` in the service
  puts it on a specific machine.

To grow from one machine, make the edit above, then:

```
orca bootstrap     # the new machine joins
orca apply         # services move onto the private network
```

## Limitations

- **More than one machine has not been tested on real hardware yet.**
- **A service with one replica restarts on deploy**, so it is briefly down.
  With `replicas: 2` or more, copies are replaced one at a time with no
  downtime.
- **Backups are scheduled dumps**, not continuous: if the server dies, you lose
  what changed since the last one. Only Postgres (its `postgres` database) and
  Redis are backed up; a plain service's `volume:` is not.
- **Garage runs on one machine**, with no replication.
- **HTTPS needs names Let's Encrypt can reach** over port 80. For names only in
  your own `/etc/hosts`, set `https: false`.
- **Secrets live only on the cluster.** If you rebuild it, set them again
  (`orca secret list` shows which). `orca secret set` refuses to change a
  generated database password (unless you pass `--force`), because the
  database was created with it.
- **Any container can reach any other.** Don't run code you don't trust.
- **Every apply contacts your image registry** to pin digests, so a registry
  outage blocks deploys.
- **Above one machine, two services on the same port can't share a machine.**
  orca places them apart, or reports the second as `unplaced`.
- **Only Debian and Ubuntu on x86-64** are supported.
- **Not built yet:** `orca exec` (a shell in a container), `orca forward`
  (reaching an internal port from your machine), and alerting.

## Command reference

```
orca bootstrap [node]               set up every machine, or one
orca validate [group...]            check the files without touching the server
orca plan [group...]                show what apply would change
orca apply [group...] [--yes]       make the server match the files
orca status [group...]              what is running, and whether it is healthy
orca top [-w] [--json]              machines and services at a glance
orca logs [target] [words] [-f] [--since 30m] [-n 200]
orca stop <group>                   stop a group, keeping its data
orca purge <group> [--yes]          delete a removed group and its data
orca secret set <group>/<name> [--force]
orca secret list
orca secret rm <group>/<name> [--force]
orca registry login <host> [-u <user>]
orca registry list
orca registry logout <host>
orca db list <group>/<service>
orca db restore <group>/<service> [backup] [--yes]
orca password                       the dashboards' password
orca password set                   change it
orca nodes                          list the machines
orca version                        orca's version and what it installs

Flags that go before the command:
  -C <dir>          look for cluster.yaml starting here
  --ssh-copy-id     copy your SSH key to the server first
```

## Learn more

[How orca works](docs/how-it-works.md) explains what runs on your server, how
networking and data safety work, and what orca deliberately leaves out.
