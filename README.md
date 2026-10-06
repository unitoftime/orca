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
  root. orca installs everything else on it (Docker, Nomad). Its SSH server
  must allow TCP forwarding, which it does unless someone turned it off.
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
orca bootstrap
orca apply
```

On a server that does not accept your SSH key yet, `bootstrap` copies it over
first, asking for the server's password once. It leaves the server's SSH
settings alone, so logging in with that password still works afterward;
turn that off yourself if you do not want it.

`bootstrap` updates the OS, turns on automatic security updates, and installs
Docker and Nomad. It is safe to run again; that is also how you upgrade orca on
the server. It also raises the largest socket buffer a program may ask for to
8MB, which a service taking many connections on a `udp` port needs.

An update that needs a reboot, a new kernel mostly, waits for one: orca never
restarts a server on its own. `orca top` and the status page say "reboot
required" when one is waiting, and `orca reboot` restarts the server when you
choose to. Everything on it stops for a minute or two and comes back by
itself.

Like git, orca finds `cluster.yaml` by looking upward from where you are, so
every command works from anywhere inside the directory. `-C <dir>` points it
somewhere else.

## Writing a service

Each `.yaml` (or `.yml`) file in a group holds one or more services, separated
by `---`:

```yaml
# shop/web.yaml
name: web
image: ghcr.io/you/shop:latest
cpu: 1                    # vCPUs; default 0.5
memory: 1G                # default 512M
replicas: 2               # default 1; see Limitations for what 2 or more buys
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
group name `orca`, and any name starting `orca-`, is reserved for orca's own
services. A group can't be a link to a directory elsewhere; move it in.

Unknown keys are errors. `orca validate` checks every file without touching the
server.

### Ports

The key is the port inside the container. The value says who can reach it:

| Value | Who can reach it |
|---|---|
| `internal` | other services only |
| `metrics` | other services only; also collected as metrics from `/metrics` |
| a hostname, like `shop.example.com` | the internet, over HTTPS |
| `auth:` and a hostname | the same, after logging in with the dashboards' password |
| `tcp` or `udp` | the internet, directly on that port |

```yaml
ports:
  5432: internal
  2112: metrics
  8080: shop.example.com
  9090: auth:ops.example.com   # asks for the dashboards' login first
  7777: tcp                # the same port number on the server
  7778: udp:9000           # or a different one
  7779: [tcp, udp]         # both protocols
```

- Nothing is reachable from the internet unless a port says so, and the
  firewall opens only those ports.
- A service can have one hostname port and one `metrics` port. Point the
  hostname's DNS at your server. No two services can share a hostname, and
  `status.`, `logs.` and `metrics.` under your monitoring domain are orca's.
- `auth:` is for a page with no login of its own, like an admin panel. It is
  the login the dashboards use: user `admin`, password from `orca password`,
  the same limit on guesses, and requests made from another site are
  refused. It needs HTTPS, and apply refuses it without.
- Two services cannot use the same public port, and TCP ports 22, 80 and 443
  are taken by SSH and HTTPS. `orca apply` stops and tells you before
  deploying.
- A service with no `ports` is not reachable at all, which is right for most
  background workers.

### Serving TLS yourself

A hostname port is HTTPS because ingress sits in front of it and presents its
certificate. A service that takes connections directly, on a `tcp` or `udp`
port, has no ingress in front, so it needs the certificate itself:

```yaml
name: proxy
image: ghcr.io/you/game-proxy:latest
tls: play.example.com
ports:
  7777: [tcp, udp]
```

- orca gets a certificate for the name and puts it in the container as
  `/secrets/tls/cert.pem` (the full chain) and `/secrets/tls/key.pem`.
- **The service must reload them from disk.** A certificate is renewed two
  thirds of the way through its life (every two months, for one that lasts
  three), and orca rewrites the two files in place when it is. It does not
  restart the service for it, because a renewal is nobody's deploy.
- Point the name at the ingress machine: the certificate authority checks it
  on port 80, which ingress answers. Apply gets the certificate before it
  deploys the service, and stops with the authority's reason if it cannot.
- The name can't also be a hostname port.

Every certificate, these and the ones ingress presents for hostname ports and
the dashboards, is issued and renewed by one job orca runs, and `orca status`
shows each with when it expires. A hostname port's service is deployed even
when its certificate cannot be issued yet, with a warning, so a service can be
brought up before its DNS is moved; the certificate follows within a quarter
of an hour of the name pointing at the machine.

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

The size is what you expect it to hold, not a quota: the volume shares the
server's disk and can grow past it. `orca status` shows each volume's use
against its size.

### Commands

`cmd:` replaces the image's command and its entrypoint. It runs with
`/bin/sh -c`, so it can be a small script. Images without a shell (`scratch`,
distroless) can't use `cmd:`; configure them with `env:` instead.

`$$` is a literal `$` here too; any other `$` is the shell's. A secret can't be
used in `cmd:` directly: put it in `env:` and use the variable.

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
- `orca plan` says whether each change restarts the service or applies in
  place, such as a new replica count.
- Apply waits for Nomad to report the new version healthy, and exits with an
  error if it doesn't; Nomad then rolls it back.
- Only one apply runs at a time. A second one stops and says who holds the
  cluster.
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

- Values are stored on the cluster, never in your service files.
- `${secret.NAME}` in an `env:` value puts a secret inside a longer string,
  like a database URL.
- Apply refuses to deploy a service while one of its secrets is unset.
- Setting a secret restarts the services that use it. No redeploy needed.
- Secrets are files by default because environment variables leak easily (into
  logs, crash reports, child processes).

### Keeping a copy

A rebuilt cluster has no secrets, so keep a copy of them:

```
orca secret export secrets.age        # every secret, encrypted to a passphrase
orca secret import secrets.age        # put them back; shows what changes, then asks
orca secret edit                      # the cluster's secrets in $EDITOR
orca secret edit secrets.age          # or the file's, without touching the cluster
orca secret export --plain            # print them in plaintext
```

- The file holds everything needed to rebuild: your secrets, the passwords
  orca generated for databases, the registry logins, the dashboards'
  password and every certificate issued. Rebuilding is `orca bootstrap`, `orca secret import`, `orca apply`.
- It is an ordinary [age](https://age-encryption.org) file, so `age -d` opens
  it without orca. It is safe to commit only as far as its passphrase is
  strong: anyone with the repository can try guesses forever.
- The cluster is still what your services read. The file is a copy: it goes
  out of date when you `orca secret set`, until you export again, and
  `orca apply` never reads it.
- Import writes only what differs, since writing a secret restarts the
  services that use it. It never removes a secret the file lacks, and it
  refuses to change a generated database password without `--force`.
- `orca secret edit <file>` creates the file if it does not exist, which is a
  way to write a new cluster's secrets down before the cluster exists.
- Certificates are in an export, so a rebuilt cluster does not have to ask
  for them again, but an edit leaves them out and keeps them as they were.
- While an editor is open, the secrets are in a temporary plaintext file only
  you can read.

## Databases and backups

A **template** is a service orca sets up for you. For Postgres:

```yaml
# shop/db.yaml
name: db
template: postgres:17       # or postgres:16
memory: 2G
volume: 20G
```

orca picks the image, port (5432, internal only), storage and memory settings
(1G unless you say otherwise), and generates a password. Other services in the group use it as
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
region: auto                # the default; set it where the store needs one, as S3 does
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
`orca logs shop/db`. It goes on showing how the last run went until the next
one, which needs the status page on (it is by default): the status page is
what keeps that on record.

A Postgres backup holds every database and every role, with its password
and grants, apart from the `postgres` superuser, whose password is the
cluster's own.

A Postgres restore loads each database beside the one it replaces, and swaps
it in only once it has loaded, ending connections to the old one. A restore
that fails leaves everything as it was. What each database held before is kept
on the server as `<name>_before_restore_<time>`; drop it when you are sure.

Backups go to the top of the bucket, under `<group>/<service>/`. To share one
bucket between clusters, give each cluster's target a `path:`, a folder in the
bucket to keep everything under:

```yaml
bucket: orca-backups
path: prod                  # backups go under prod/<group>/<service>/
```

Without one, two clusters with a database of the same name would keep it in
the same place, each pruning and restoring the other's backups.

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
named after the group and service (`files-store`) and an access key. A service
in the same group uses them like this:

```yaml
env:
  S3_ENDPOINT: http://store:3900
  S3_REGION: orca
  S3_BUCKET: files-store
  S3_KEY_ID: ${secret.store_key_id}
  S3_SECRET: ${secret.store_secret_key}
```

A secret belongs to its group, so a service in another group cannot name
these two. Keep the store in the group that uses it, or copy both values into
the other group with `orca secret set` and reach the store as `store.files`.

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

A service is `running`, `pending` (starting or deploying), `failed`,
`unplaced` (there is no machine it fits on; the reason is shown) or `stopped`
(with Nomad's own tools, not by orca). A database's backup shows as `scheduled` until its first
run, and from then on as how that run went. `orca status` also lists data
left behind by services you removed.

Logs are kept for 14 days (up to 10G) and outlive the container that wrote
them. Metrics are kept for 30 days.

With `monitoring.domain` set and HTTPS on, there are web dashboards too. Log
in as `admin` with the password from `orca password`:

- `status.<domain>`: machines and services, their health and resource use,
  and each service's logs
- `logs.<domain>`: search logs
- `metrics.<domain>`: query metrics

The status page also shows what is placed on each machine, and how much of
its CPU and memory those services have claimed. Nomad places by claims, not
by use, so that is what decides whether the next service fits.

Nomad's own UI is not published: the token that opens it can run anything on
every machine. Reach it over an SSH tunnel instead:
`ssh -L 4646:127.0.0.1:4646 root@<server>`, then open http://localhost:4646
and sign in with the token in `/etc/orca/nomad.token` on the server.

`orca password set` changes the password at the next apply; it takes one of
16 to 72 characters.

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
    name: box1

ingress:                      # or `ingress: false` if nothing is served over HTTP
  acme_email: you@example.com # optional contact for Let's Encrypt
  acme_directory: https://acme-staging-v02.api.letsencrypt.org/directory
                              # where certificates come from; Let's Encrypt if
                              # unset. Staging, as here, while testing
  https: false                # plain HTTP, for names Let's Encrypt can't reach;
                              # the dashboards are then not published

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
firewall: false               # manage the public interface yourself
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
  Tailscale or WireGuard), and every node needs a `private_ip`. Anything else
  on that network is treated like the internet: it reaches SSH and the ports
  your files publish, and nothing more.
- The first node listed is a server and the rest are clients, unless a node
  says `role:`. Servers must be an odd number: 1, 3 or 5.
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
  On one machine, a service with `replicas: 2` or more that has a port, and no
  raw `tcp` or `udp` one, deploys with no downtime: a new set of copies starts
  beside the old one and takes the traffic once healthy. Above one machine its
  copies are replaced one at a time instead, because each holds its port on
  the machine it runs on.
- **Backups are scheduled dumps**, not continuous: if the server dies, you lose
  what changed since the last one. Only Postgres and Redis are backed up; a
  plain service's `volume:` is not.
- **Garage runs on one machine**, with no replication.
- **HTTPS needs names Let's Encrypt can reach** over port 80. For names only in
  your own `/etc/hosts`, set `https: false`.
- **Secrets live only on the cluster** unless you export them. If you rebuild
  it without an export, set them again (`orca secret list` shows which).
  `orca secret set` refuses to change a generated database password (unless
  you pass `--force`), because the database was created with it.
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
orca logs [target] [words...] [-f] [--since 1h] [-n 200]
                                    read or search a service's logs
orca stop <group>                   stop a group, keeping its data
orca purge <group> [--yes]          delete a removed group and its data
orca secret set <group>/<name> [--force]
                                    set a secret, read from stdin or prompted
orca secret list                    the secrets referenced, and which are missing
orca secret rm <group>/<name> [--force]
                                    remove a secret
orca secret export [file] [--plain]
                                    write the cluster's secrets to a file
orca secret import <file> [--yes] [--force]
                                    put a file's secrets on the cluster
orca secret edit [file] [--plain] [--force]
                                    edit the cluster's or a file's secrets
orca registry login <host> [-u <user>]
                                    log the cluster in to a private registry
orca registry list                  the registries the cluster has a login for
orca registry logout <host>         remove a registry's login
orca db list <group>/<service>      a database's backups, oldest first
orca db restore <group>/<service> [backup] [--yes]
                                    restore a backup, the newest by default
orca password                       the dashboards' password
orca password set                   change the dashboards' password
orca nodes                          list the machines
orca reboot [node] [--yes]          restart a machine and wait for it
orca version                        orca's version and what it installs

Flags that go anywhere on the line:
  -C <dir>          look for cluster.yaml starting here
  -h                what a command does and takes
```

## Learn more

[How orca works](docs/how-it-works.md) explains what runs on your server, how
networking and data safety work, and what orca deliberately leaves out.

## License

orca is released under the [MIT License](LICENSE).

What it installs on your server is other people's software, under their own
licenses, and not all of them are as permissive. Nomad is under the Business
Source License 1.1, Garage under the AGPL 3.0, and Redis 8 under your choice
of the RSALv2, the SSPLv1 or the AGPLv3. orca fetches each from its publisher
onto your server and redistributes none of them.

To report a security problem, see [SECURITY.md](SECURITY.md).
