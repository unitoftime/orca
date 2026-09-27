# The orca model

> What orca is, the nouns it has, and the shape of using it. This is the
> document the implementation is built against. Where the code and this file
> disagree, one of them is a bug.

## What orca is

A tool that turns machines you own into a place you can deploy things, where the
hard parts are already solved.

The test it has to pass: **adding a new service to your infrastructure should
be eight lines of YAML and one command, and should never involve thinking about
a machine.**

## The nouns

| Noun | What it is |
|---|---|
| **Machine** | A box orca manages, listed in `cluster.yaml`. Usually one. |
| **Group** | One directory of service files. The unit of deploy, and the namespace. `blog`, `shop`, `notifier`. |
| **Service** | One container inside a group. `api`, `dev`, `web`, `worker`. |

And one thing that is *not* a noun you manage:

**`orca`** is the reserved group holding the jobs orca runs for you: ingress,
the resolver, the log and metric stores. Not hidden and not special-cased;
they show up as `orca/traefik`, `orca/dns` and so on, so what is running is one
list and not two. A directory named `orca` is refused. See
[What orca runs for you](#what-orca-runs-for-you).

## Templates

A service is normally an image you built. It can instead be a **template**: a
known-good, fully-wired piece of infrastructure that orca operates for you.

```yaml
# shop/db.yaml
name: db
template: postgres:17
memory: 2G
volume: 20G
```

A template is sugar, not a new primitive. It expands into an ordinary service
spec that the rest of the system already knows how to run. Deploys, logs,
metrics and discovery all work with no new machinery: no new manifest
primitive, no new runtime object.

The template fixes everything that has to be correct and would be tedious or
dangerous to get right by hand. You tune only size: `cpu`, `memory`, `volume`.

Three templates exist: `postgres`, `redis` and `garage`.

What `postgres` fixes:

| | |
|---|---|
| image and version | pinned to versions orca will actually run |
| port | 5432, and the health check that goes with it |
| volume | required, mounted where Postgres expects it, and **owned by the user the image runs as**; a bind mount keeps the host's ownership, so a root-owned directory fails at startup with a permission error that says nothing about volumes |
| password | generated on first apply, stored in the cluster, never shown |
| `shared_buffers`, `effective_cache_size` | a quarter and a half of the memory you asked for |

Those last two are the only settings orca touches. Postgres ships with a 128MB
`shared_buffers` and a 4GB `effective_cache_size` whatever it is running on, so
a database given 8GB would use a sixteenth of it and plan as though it had
half. Everything else depends on a workload orca knows nothing about.

### redis: a database of record

```yaml
# blog/redis.yaml
name: redis
template: redis:8.10
memory: 1G
volume: 10G
backup:
  to: storage/offsite
```

Run as the place state lives, not as a cache: every write is appended to disk
and fsynced each second, so a crash loses at most a second, and nothing is ever
evicted. What it fixes:

| | |
|---|---|
| image | the official `redis` image, which from Redis 8 carries JSON, search, time series and probabilistic types itself, so the discontinued Redis Stack is not needed |
| port | 6379, and the health check |
| volume | required, at `/data`, owned by the image's `redis` user |
| password | generated, `<service>_password`, written into a config file rendered from the variable store; never an environment variable, never an argument |
| `maxmemory` | three quarters of the memory you asked for, with `noeviction` |

The cap is the one tuning: past it a write fails with an error the application
sees, where without it the kernel kills the container and the last second of
writes goes with it. The remaining quarter is for what Redis does not count:
the copy-on-write pages of the fork that snapshots or rewrites the log.

A backup is a snapshot the server streams over the replication protocol
(`redis-cli --rdb`), so it is a consistent point in time taken while Redis keeps
serving, and nothing reads the volume while Redis is writing it.

**Restoring is the one restore with downtime.** There is no loading a snapshot
into a running server, so `orca db restore` stops the service, swaps its data,
and starts it again, keeping what it replaced under `/var/orca/pre-restore/`.
And the snapshot cannot simply be dropped in: with the append-only log on,
Redis ignores `dump.rdb`, finds no log, and starts empty — a restore that
reports success and holds nothing. So the snapshot is loaded by a throwaway
server with the log off, which then turns it on, writing the log from the
loaded data; the service starts from that as it always does. A failure once the
service is down puts the old data back and starts it again.

### garage: S3-compatible object storage

```yaml
# files/store.yaml
name: store
template: garage:2.3.0
memory: 512M
volume: 20G
```

Somewhere to put the things a filesystem is the wrong home for, without
renting a bucket. Unlike a database it is **not usable the moment its process
is up**, and the template exists mostly because of that: a fresh Garage node
holds no data at all until a storage layout is applied, and an S3 endpoint
with no access key is one nobody can use. So the template also applies the
layout, creates a bucket named after the group and service, and imports an
access key, handing one back the way postgres hands back a password.

```yaml
env:
  S3_ENDPOINT: http://store.files:3900
  S3_KEY_ID: ${secret.store_key_id}
  S3_SECRET: ${secret.store_secret_key}
```

The admin API binds loopback inside the allocation, so the setup step reaches
it while nothing else can, not even another container on the same machine.

### Generated secrets

The generated password is named after its service, so it is visible in
`orca secret list` and referenceable from whatever connects:

```yaml
# shop/app.yaml
env:
  DATABASE_URL: postgres://postgres:${secret.db_password}@db:5432/postgres
```

It is generated **once**. Regenerating it on every apply would lock the
database out of its own data. "Once" is enforced by the secret store itself
(the value is written create-only), and `orca secret set` and `orca secret rm`
refuse a generated secret unless given `--force`, since replacing it and
removing it (which makes the next apply generate a new one) lock everything
out just the same.

**You are the superuser.** orca does not model databases, roles, or schemas
inside your Postgres; `CREATE DATABASE` is your business. What orca guarantees
is narrow and is the part that is genuinely hard to build yourself: it runs, it
has fast local disk, and it is continuously backed up somewhere else with a
restore path that works.

That is what "solve the database problem once" actually means here. Not one
blessed shared instance: **you never write backup code again, for any project.**

### Targets, and where backups go

Somewhere to put backups is declared like anything else, in a group:

```yaml
# storage/offsite.yaml
name: offsite
target: s3
endpoint: https://<account>.r2.cloudflarestorage.com
bucket: orca-backups
```

A **target** is the one service document that runs no container. That is a real
cost, since every other noun here is something running, and it buys the one
thing a per-database block could not: the endpoint and the credentials are
written once, and everything that backs up refers to them by name.

```yaml
# shop/db.yaml
name: db
template: postgres:17
volume: 20G
backup:
  to: storage/offsite     # cross-group, like any other name
  schedule: "0 3 * * *"   # optional; nightly by default
  keep: 14                # optional
```

Its credentials are two secrets in its own group, named after it the way a
template's generated secrets are:

```
orca secret set storage/offsite_key_id
orca secret set storage/offsite_secret_key
```

They are required, not generated, so the ordinary secret preflight refuses to
deploy a backup job that has no way to authenticate. The failure it prevents is
the worst kind, where the job is created, runs on schedule, fails inside a
container nobody is watching, and the first anyone knows is when a restore is
needed.

Naming a target is what turns backups on. There is no separate switch to
forget, and no cluster-wide setting that silently applies to databases declared
later. The schedule and retention are per database, so one noisy database can
keep more history without everything else doing the same.

Any S3-compatible store differs only in the endpoint, so R2, Backblaze, Wasabi,
MinIO and S3 itself are one value apart.

How many Postgres instances you run is then a free choice with no tooling
consequence. Five apps each declaring their own is five processes of a couple
hundred MB, independently versioned, independently backed up, unable to take
each other down. Prefer one shared instance? Make a `db` app and have others
reach it at `db.db:5432`. Neither option needs anything orca does not already do.

## One directory is the cluster

A cluster is a directory tree. `cluster.yaml` marks its root, and each
directory beside it is a **group** of services named after the directory:

```
infra/
  cluster.yaml          the machines, and cluster-wide settings
  blog/                 group "blog"
    db.yaml
    api.yaml
  blog/prod/            group "blog-prod"
    web.yaml
  notifier/             group "notifier"
    notifier.yaml
```

**The directory is the identity.** Nothing inside a file repeats the group
name, two groups cannot collide because two directories cannot share a path,
and renaming a group is `mv`. There is no index of apps to maintain: the
directories are the index, so a group is added by creating a directory and
removed by deleting one.

**Nesting is one rule**: the group name is the path under the root with `/`
turned into `-`. Whether a directory means an app, a project or a stage is your
decision, expressed by how deep you nest it; orca does not need to know which
you meant.

orca finds the root by walking up from the working directory, the way git finds
`.git`, so every command works from anywhere inside the tree.

### `cluster.yaml`: the machines

```yaml
nodes:
  - host: root@203.0.113.10

monitoring:
  domain: example.com
  logs:
    retention: 14d
    disk: 10G
```

Nothing about *what is deployed* appears here. Nor does anything that is not a
real decision: the Nomad datacenter and the data directory are constants, and a
machine that wants its data on a bigger disk mounts that disk at `/var/orca`
rather than teaching orca a second path to think about.

### A service file

Each file in a group directory holds one or more service documents, separated
by `---`. Split one service per file or keep the whole group in one, and change
your mind later without orca caring.

```yaml
# blog/api.yaml
name: api
image: ghcr.io/you/blog:latest
cpu: 1
memory: 1G
ports:
  8080: blog.example.com
env:
  SESSION_KEY: ${secret.session_key}
---
name: dev
image: ghcr.io/you/blog:dev
memory: 512M
ports:
  8080: dev.blog.example.com
```

`cmd:` replaces the image's command, and its **entrypoint** along with it. Both
have to go, because Docker leaves an image's ENTRYPOINT in front of a replaced
command. Overriding only the command runs `<entrypoint> /bin/sh -c <cmd>`, the
flags land on the wrong binary, and the container dies saying something
unrelated.

The cost is a sharp edge worth knowing before you hit it: **`cmd:` runs through
a shell, so an image that has no shell cannot use it.** That means `scratch` and
distroless images, which is not an exotic case: it is what a small static Go
binary is usually shipped in. Such an image should run its own entrypoint and
take its configuration through `env:` instead. orca cannot check this, because
what is inside an image is not knowable from a manifest.

A service may also set `node:` to pin itself to a named machine. It is never
needed: a service with a volume is pinned automatically to the first server,
which on one machine is the only place its data can be, and is still where it
is after a second machine is added.

**The file is desired state.** A service deleted from it gets stopped on the
next apply. There is no `enabled:` field.

Unknown keys are rejected, so a typo never silently takes a default.

**Your app repositories stay out of this.** A manifest changes when the shape of
a service changes, which is rare; a new image is picked up because manifests
pin a moving tag and orca resolves it to a digest at apply time. So CI pushes an
image and never touches this repository.

### Variables

The other way to name an image is by the build it came from
(`blog-server:4f2a9c1`), so the files say exactly what runs, and going back is
reverting the change that went forward. The cost is writing the build into
every file that runs it. A variable writes it once:

```yaml
# blog/prod/vars.yaml
commit: 4f2a9c1

# blog/prod/server.yaml
image: ghcr.io/you/blog-server:${var.commit}
```

A `vars.yaml` applies to its directory and everything below it, nearest
winning, so values every group shares sit beside `cluster.yaml`. It is the one
`.yaml` in a group directory that is not a service, and a directory holding
nothing else is organisation, not a group.

Variables are kept deliberately small, because every template language began
as a way to avoid writing one value twice:

- **Values only.** They are filled in on the parsed document, not the text,
  so a key is never expanded (and a reference in one is an error), a comment
  is never touched, and a value needs no escaping for wherever it lands. An
  unquoted reference takes the type of its value; a quoted one stays a string.
- **One value each.** No lists, no blocks, no conditions. A difference between
  two stages that is structural (prod is backed up and test is not) is
  written out in each, not expressed through a variable.
- **Filled in when the files are read.** An undefined variable is an error
  naming the ones that are defined, and `orca validate` shows what each became.
  The job spec holds the result, so a changed value is a changed service.
- **No `$` in a value.** Variables are filled in before env values are read,
  so a `$` would be read twice and `${secret.x}` in a variable would become a
  secret reference nobody wrote as one.

`${var.NAME}` and `${secret.NAME}` look alike on purpose and differ in when they
are resolved: a variable in orca, from the files, before anything is deployed;
a secret on the machine, from the variable store, when the container starts.

## The verbs

```
orca bootstrap [node]        fresh machine -> ready to run things
orca validate [group...]     parse and check manifests, touching no machine
orca plan [group...]         show what apply would change, without doing it
orca apply [group...]        converge the cluster to the manifests
orca status [group...]       what is running, whether it is healthy, and at
                             what image digest
orca top [-w] [--json]       the status page in the terminal: what needs a
                             look, each machine, each service's usage
orca logs [target]           query the log store; a target is a group, a
                             service, or <group>/<service>
orca stop <group>            stop a group's services, keeping its data
orca purge <group>           delete a group AND its data (the one irreversible verb)
orca secret set|list|rm      set, list and remove secrets
orca registry login|list|logout
                             the cluster's credentials for private registries
orca db list|restore         list a database's backups, and restore one
orca nodes                   list the machines
orca version
```

Not built, and listed because they are the obvious next things rather than
because they exist: `orca exec` (a shell in a running container), `orca
forward` (a tunnel to an internal port), and `orca check` (a health sweep on
demand, which stands in for alerting; `orca top` answers the "is anything
wrong" half of it, but only when asked).

Deliberately not on that list: `orca rollback`. The files are what runs, so
going back is reverting the edit that moved forward; a command that rolled
back the cluster would leave the files saying something else. The edit
forward is kept to one line by [Variables](#variables).

`orca apply` with no argument converges everything in the tree; named groups
narrow the scope, and anything outside that scope is left completely alone.

What orca runs for you is included in every apply as a group named `orca`, so
there is no separate verb for it. `orca apply orca` targets it alone.

## What orca runs for you

Seven jobs behind four capabilities, plus a firewall that is not a job. The
knobs are capabilities rather than processes, because a store and its shipper
are useless apart:

| Capability | Jobs | What it does |
|---|---|---|
| `ingress` | traefik | The HTTP front door: routes hostnames to services and gets certificates from Let's Encrypt, to an account with no contact unless `acme_email` gives one; `https: false` serves plain HTTP instead. Only services with a hostname port need it, and those are refused when it is off. |
| `dns` | dns | The resolver on every machine that makes services findable by name. See [Names](#names). |
| `monitoring.logs` | victorialogs, vector | Vector runs on every machine, reads container logs from Docker, and ships them to VictoriaLogs, which stores and serves them. |
| `monitoring.status` | status | The status page: every machine and service at a glance. It is orca itself; see [The status page](#the-status-page). |
| `monitoring.metrics` | victoriametrics, node-exporter | VictoriaMetrics scrapes and stores; one process does both. The node exporter runs on every machine and reports the machine itself: CPU, memory, disks, network. |
| `firewall` | — | The nftables ruleset on every machine, regenerated on every apply. See [The firewall](#the-firewall). |

```
   your containers              Nomad's telemetry     node-exporter
         │ docker socket         (every machine)     (every machine)
         ▼                              │                   │
      vector  ──push──▶  victorialogs   └─────scrape────────┤
   (one per machine)          │                             ▼
                              │                      victoriametrics
                              │                             │
                              ├── vmui ─────────────────────┤  (browser UI, built in)
                              │                             │
                              └──────────▶ status ◀─────────┘ ◀── Nomad's API
                                    (the status page, orca top)

      traefik ──reads the Nomad catalog──▶ your HTTP services, and the dashboards
```

**Nothing orca runs for you depends on another of them being healthy.**
This is the rule that makes it different from the Grafana stack it replaces, and
it is worth checking against rather than asserting:

- Vector buffers to disk when the log store is down, and sends on reconnect.
  The buffer has its own ceiling, because an unbounded buffer is a slower way to
  fill the disk.
- VictoriaMetrics scraping a dead target records nothing for it and carries on.
- Traefik dying takes down HTTP routing and nothing else. Raw TCP services, the
  database, and log collection are all unaffected, and `orca logs` reads the
  store over SSH rather than through ingress, so log access does not depend on
  the front door being up.
- Each is a single binary with a local directory. There is no object store, no
  compactor, and no ring: the components that break in a Loki/Thanos stack are
  components these do not have.
- The status page reads the others and nothing reads it. When one of them
  cannot be read, the page says so (as a problem, not a blank section) and
  shows what the rest still know. Its health check asks only whether it is
  serving, so a store going down never restarts the thing reporting it.

The data stores bind the **internal** host network declared in the Nomad client
config (the container bridge on one machine, the private network above one)
and never the public interface. A well-known port on the default interface
would put your logs on the public internet for anyone to read. See
[Binding](#binding-nothing-of-ours-is-ever-public).

### The dashboards

The status page answers "is anything wrong", and vmui, the query UI built into
both Victoria binaries, is what makes logs and metrics explorable in a browser
without running Grafana. Both are published, along with the Nomad UI, on
subdomains behind HTTP basic auth:

```yaml
monitoring:
  domain: example.com
```

giving `status.example.com`, `logs.example.com`, `metrics.example.com` and
`nomad.example.com`; the status page links to the other three. The domain is
the dashboards' alone. Services name their own hostnames, on any domain, rather
than getting `<service>.<group>.<domain>` under it: a name nobody wrote is one
you have to look up.

The user is `admin`, and the password is generated for the cluster: bootstrap
makes one and keeps it in Nomad's variable store at `orca-admin/password`,
create-only, and `orca password` prints it. Apply makes one too if the cluster
has none. `orca password set` replaces it, read like a secret (prompted or
piped, never an argument), and ingress picks it up at the next apply. It is not
a setting in cluster.yaml, because that would put a credential in a file that
is otherwise safe to commit; it lives where secrets do. A password you have to
choose is one you choose badly, or reuse; one generated per cluster is neither,
and mostly you only need to read it.

Both a domain and a password are required. **Without either, nothing is
published** and the UIs stay reachable only over an SSH tunnel (`orca top`
needs neither). With the password generated, that is in practice a domain.
Failing closed matters here because the alternative is putting your logs on the
internet behind nothing.

The password is stored as a bcrypt hash carried in ingress's own job metadata,
and the deployed hash is reused whenever it still matches the configured
password. bcrypt salts randomly, so hashing afresh on every apply would change
the job spec every time and redeploy ingress forever; the reuse is what keeps
an unchanged cluster a genuine no-op. Changing the password produces exactly one
update.

### The status page

`status.<domain>` is orca itself, run on the machine as `orca serve-status`:
the same binary as the CLI, so the page and the terminal are one
implementation. Until orca publishes an image of its own, apply ships a build
of orca to the machine that runs the platform, at
`/var/orca/bin/orca-<hash>`, and the status job mounts it into a pinned Alpine
image. The path is the build's hash, so a new build is a new job spec and
nothing running ever has its binary replaced underneath it; the two previous
builds are kept for Nomad to revert to.

The build has to be static, because the image's C library is not the one on
the machine that built it. `make build` is. An orca that is not (a `go
install` on Linux links against the system's) has apply build one from the
same version in the module cache, once per version. A development build that
is not static has no version to rebuild from, and apply says so.

It runs on the host's network, because it reads Nomad's API on loopback, which
the firewall keeps every container on the bridge away from. It only reads.
Its port is dynamic on the internal network; ingress and the CLI find it
through the catalog.

What it shows is judged once, in `/api/summary`, and read by both the page and
`orca top`, so they cannot disagree about what needs a look. Service health is
`orca status`'s own: the CLI reads Nomad through jq over SSH and the page
through Nomad's Go types, and a test holds the two projections to identical
results on the same responses. Thresholds are fixed: 80% warns and 90% is
critical for memory, disks and a service against its memory limit; CPU warns
at 80% and is critical at 95%; a disk of 4G or more with under 1G free is
critical whatever the percentage. A service's CPU is shown but never judged:
it is a scheduling weight, not a cap. The stores report their own disks: the
log store at its cap is the cap working, not a problem; the metric store near
its `min_free` floor is a warning, and either one refusing data is critical.

Its logs drawer reads by service name, never by job or query: the page can ask
for the logs of a service the summary has, over a fixed set of windows, and
the query is built by the same code as `orca logs`.

### Choosing what runs

Every capability is on by default. Turn one off with `false`, or give it a
mapping of settings:

```yaml
# cluster.yaml
ingress: false              # a cluster serving only raw TCP needs no front door

monitoring:
  logs:
    retention: 14d
    disk: 10G               # hard ceiling: oldest days are dropped at the cap
  metrics:
    retention: 30d
    min_free: 2G
```

Each is named for what it is rather than for the software behind it: `ingress`
and not "traefik", `monitoring` and not "victorialogs and victoriametrics and
vector". What runs underneath is orca's business and can change; what you asked
for is yours. `monitoring` has two halves and either can be switched off alone,
because wanting logs without metrics on a small machine is an ordinary thing to
want.

You disable by saying `false`, never by omission, so deleting a settings block
cannot silently remove a running component. A capability switched off is simply
a service orca no longer declares, and the ordinary rule (apply stops what is
no longer declared) removes it, keeping its data.

### The disk ceilings are not symmetric

Worth stating precisely, because "everything is capped" would be a nicer claim
than the truth:

- **VictoriaLogs takes a hard byte ceiling.** At the cap it drops the oldest
  days. Logs can never grow into the space the database needs.
- **VictoriaMetrics has no byte ceiling.** Its size is bounded by the retention
  period, with a free-space floor (`min_free`) at which it stops accepting data
  rather than consuming the last of the disk. Bounded in practice, but by time
  and a backstop rather than by a number of gigabytes.

## Durability, and what apply is allowed to delete

The manifest is desired state, so removing a service from the file must do
something. Putting a database in that file makes the question sharp: you delete
those lines, maybe in a bad merge. What happens to your player data?

Both naive answers are wrong. If apply deletes it, a careless edit destroys data
permanently and declarative config becomes frightening. If apply silently keeps
it, the file is no longer desired state and orphaned volumes pile up unreferenced.

**So apply stops; only purge deletes.**

| Command | Services | Data |
|---|---|---|
| `orca apply`, service removed from the file | removed | kept |
| `orca stop <group>` | removed | kept |
| `orca purge <group>` | removed | **deleted, irreversibly** |

`purge` is the only destructive verb. It names what it is about to delete, and
it asks for the group's name rather than a yes; typing the name of the thing
being deleted is hard to do by reflex.

**purge refuses while the manifests still declare the group.** The only way to
reach it is to have already deleted the directory, so an accidental purge takes
two deliberate steps rather than one mistyped word. It also refuses while the
manifests do not load at all: "is this group still declared?" has no answer
then, and a guard that reads a typo in another group's file as "no" is not a
guard.

A volume lives at `/var/orca/volumes/services/<group>/<service>`, so its owner
is a fact of its path and purge deletes exactly the group it names. A flat
`<group>-<service>` directory could not be split back, because group names
contain dashes (`shop-prod-db` is shop's `prod-db` or shop-prod's `db`), and
purging `shop` would take shop-prod's data with it.

`orca stop` is the fast way off the machine: something is misbehaving and you
want it down now, without editing files and waiting for an apply. Because the
manifests are unchanged, the next apply brings it back, which it says out loud
rather than leaving you to discover it. It is named for what it does and not
for how final it sounds: nothing it touches is unrecoverable, and `purge` is
the verb that is.

A removed service is removed from the scheduler rather than left stopped. A
stopped job record is a tombstone that lingers forever and buys nothing, since
the manifests are the source of truth and bringing a service back means
declaring it again. **`orca status` lists data whose service is no longer
declared**, found by listing the one directory every volume lives under, so
"kept" never means "invisible", and nothing has to be remembered for data to be
findable later.

Backups rather than replicas: one machine is one failure domain, and HA across
one machine is theater. See [Backups](#backups) for what is actually kept and
how far back it goes.

## Networking and ingress

`ports` answers one question: **what does this service listen on, and who can
reach each one?**

```yaml
ports:
  5432: internal              # other services only, never published
  2112: metrics               # internal, and scraped for Prometheus metrics
  8080: shop.example.com      # routed through ingress, TLS included
  7777: tcp                   # raw host port 7777, no proxy in the path
  7777: udp:7778              # host port may differ from the container port
  7777: [tcp, udp]            # both protocols
  7777: [tcp:7777, udp:7778]  # both, with different host ports
```

There is no separate `port:` for the catalog and health check beside an
`expose:` for the outside world. The two would not be independent: `port:`
would default to the lowest exposed port, and be written by hand only to say
"internal", which is what the port says itself. Telling two such fields apart
is the first thing anyone asks about, which is the usual sign that one of them
should not exist.

The key is the **container** port, which makes it unique per service, so the
reach rides on the value. A list is the one thing a scalar cannot say, and both
protocols on one port need it, because YAML has no way to write the same key
twice.

**`internal` is the default reach in every sense that matters.** A templated
database gets it without asking. A hostname goes through ingress and gets a
certificate for free. Naming a protocol binds a host port directly, because a
raw TCP or UDP service does not want a proxy adding latency to every packet,
and it is the only value that publishes anything on a public interface.

Who can reach a port and how it is addressed are separate questions. The value
answers the first. The machine count answers the second, identically for every
port that is not public: on one machine a port is reached at its allocation's
own address, over the bridge, by other containers and by ingress; above one it
is published on the private network at its own number. A hostname port is
therefore not published on the public interface at all; ingress reaches it the
way a sibling would.

The port routed through ingress, when there is one, is the one registered in
the catalog and health-checked; otherwise the lowest declared that is not a
`metrics` port. A service is found and checked on the port it serves, not the
side door it reports through. Ingress routes to the registered port, so
registering any other would send web traffic to it.
The registration is what makes `db.shop` resolve. A service with no `ports:` at
all listens for nothing, registers nothing, and is checked on its task state,
which is most background workers.

Host ports are a single global namespace across every app. Two apps claiming the
same one is an error at apply time, not a placement failure later. So is
claiming a port the machine already holds: 22, which sshd listens on, and 80
and 443 while ingress runs. Nomad knows about neither, so without the check
such a service places cleanly and then fails on the box with "address already
in use".

**The firewall is derived from the manifests.** orca knows every port in
every app, so it generates the whole ruleset: default deny on the uplink, allow
SSH, allow 80/443 for Traefik, allow exactly the declared host ports, regenerate
on every apply. Nothing is reachable that is not in a file, and no rule is ever
maintained by hand.

Services reach each other by name through Nomad's service catalog, never by IP
and never by `localhost`. This is the single rule that keeps a second machine
from being a rewrite.

### Names

Every service is resolvable by DNS, so an ordinary connection string works and
an off-the-shelf image needs no orca-specific configuration:

| From | Resolves |
|---|---|
| inside group `blog` | `db` → that group's db |
| anywhere | `db.blog` |
| anywhere | `db.blog.orca` |
| anywhere | `victorialogs.orca` (orca's own jobs are a group like any other) |

A resolver runs on every machine, bound to the container bridge's gateway: the
one address every container can reach, nothing outside the machine can, and
that does not move when the resolver restarts. Tasks get it plus their own
group's search domain, which is what makes a bare `db` mean this group's db.
Anything that is not an orca name is forwarded to whatever the machine itself
uses.

The names come from the catalog: every registration orca makes carries an
`orca-dns=<service>.<group>` tag, and the resolver's hosts file is rendered
from every tagged registration. So whatever registers is resolvable, from
whichever apply deployed it, and the resolver's own job never changes when
services do.

The name is resolved when a connection is made, not when a service is
deployed, so nothing holds an address and a service that moves is simply found
at its new one. Injecting sibling addresses as environment variables instead
would need every application to read an orca-specific variable name, could not
reach another group at all, and would restart every service in a group whenever
any one of them moved.

**A group is a namespace, not a boundary.** It scopes names, job names, secret
paths and the reach of `orca apply <group>`, and nothing else. Any service can
reach any other; short names are a typing convenience, not access control. This
is the same assumption that leaves out sandboxing and network policy: you
wrote everything this runs.

**orca does not build an overlay network.** Use your provider's private network,
or Tailscale/WireGuard if your machines are in different providers. Building one
is the mistake orca exists to avoid.

## Images

Tags are resolved to a digest at apply time and the digest is what gets
submitted. That holds for every image, including the ones orca adds itself: the
platform's, a template's setup task, a backup's S3 client. This means:

- `image: foo:latest` actually redeploys when latest moves. Nomad will not
  restart a job whose spec has not changed, so without a digest in the spec a
  moved tag deploys nothing.
- Every deploy is deterministic and auditable. `orca status` shows the digest
  that is really running, not the tag you asked for.

This is what makes `orca apply` safe to run from CI on every commit: the whole
set converges, and only what actually moved gets redeployed. Two properties
follow and are requirements, not nice-to-haves:

- **A no-op apply must be quiet and fast.** Most CI runs change nothing and
  should say so in a second, not re-push every job.
- **Apply reports what it changed**, and `orca plan` shows that without doing
  it. A CI log should read `blog/api: sha256:abc -> sha256:def` and nothing else.

Apply decides what moved by stamping a content hash of the rendered job into the
job's own metadata and comparing it against what is deployed. Because the hash
covers the whole spec, the resolved digest included, anything that would
change what runs changes it, with no list of significant fields to keep in sync.

### Private images

Resolving a digest happens on your machine, with your Docker login. Pulling
happens on the cluster's, which has none. So without more, a private image
pins perfectly, deploys, and then fails on the machine as "unauthorized", after
the health timeout.

The cluster has its own login, per registry:

```
orca registry login ghcr.io        # username, then a token, never echoed
```

**It lives where every other secret does**, in Nomad's variable store, one
variable per registry at `orca-registry/<host>`. A sibling of `orca/` rather
than inside it, so `orca secret list` does not read logins as unreferenced
secrets. Nomad refuses `.` and `:` in a variable path, so the host is stored
with `_` and `~` instead. Neither can occur in a host name, which is what
lets `orca registry list` read the host back out of a path, and so list
logins without pulling a token off the machine.

**Nomad reads it at pull time, through a credential helper.** Nomad, not the
`docker` command, asks the daemon for a pull, and has to supply credentials
with the request. It has three places to get them: an `auth` block in the job,
a docker `config.json` file, or a credential helper. That is a program named
`docker-credential-<name>` that is given a registry host on stdin and answers
`{"Username", "Secret"}`, the protocol Docker defined so credentials need not
sit in a plaintext file. Bootstrap installs `docker-credential-orca` on every
machine and names it in the Nomad config. It reads the variable over the
loopback API, which answers on every machine; one running no server forwards
the read to one that does.

The other two are worse:

- **An `auth` block** would put the token in the job spec, or (filled from a
  template) in the container's environment, and so in the log store of any
  service that logs its own environment. That is what the secrets design exists
  to avoid.
- **A `config.json`** is a plaintext file on every machine, written by
  something that holds the token. apply does not hold secret values, so it
  would be a second secret store with its own way of being set, and one a new
  machine would not have.

Because nothing about a login is in any job, a job's hash does not change when
a login does: logging in, rotating a token and logging out redeploy nothing,
and the next pull uses whatever is there.

**The helper never fails.** Nomad treats a helper that exits non-zero as a
failed pull, so a failure there would break every public image too. A registry
with no login, or a store it cannot reach, is answered `{}`, which Nomad
takes as "pull anonymously": a public image still pulls, and a private one
fails as unauthorized, which is then the true reason. Every image goes through
it, including Docker Hub's, so `orca registry login docker.io` also lifts the
anonymous pull limit for the images orca runs for itself.

**apply refuses a private image the cluster cannot pull**, before it touches
anything, the way it refuses an unset secret. Knowing which images are private
comes free from resolving them: a registry is asked anonymously first, and only
when it refuses (401, 403, or the 404 some answer rather than admit a private
repository exists) with your credentials. An image that resolved only with
yours is private, and its registry needs a login in the cluster. A network
error is never retried, so a blip is not mistaken for a private image. The cost
is one extra round trip per private image; a public one costs nothing extra.

An image written as a digest is never looked up, and so is never known to be
private. That is the one case the check cannot see.

`orca registry login` checks the token against the registry before storing it,
as `docker login` does, so a typo or an expired token is caught with someone at
the keyboard rather than at a pull. It then checks that every machine has the
helper, since a machine without it pulls anonymously whatever the store holds,
and says to re-run bootstrap if not.

### Apply verifies, it does not just submit

A matching hash says a deploy happened, not that anything works. A job Nomad
accepts can still crash-loop or fail to place, so apply waits (with a bound,
because a cold image pull is slow and healthy while a crash loop never
resolves) and **fails if anything is unhealthy**.

The check covers every service in scope, not only the ones this apply changed.
An apply that changes nothing while a service is down still has to say so, or a
CI job running apply on every commit stays green while production is broken.
With nothing to change and nothing wrong, it prints one line and exits zero.

`orca status` is the same judgement on demand. Four states matter:

| State | Meaning |
|---|---|
| `running` | every replica up, rollout finished |
| `pending` | still starting or rolling out; not yet a failure |
| `failed` | crash-looping, unhealthy, or the rollout failed |
| `unplaced` | Nomad accepted it but can run it nowhere |

`unplaced` is its own state because it looks exactly like "still starting" while
being permanent, and the reason is never in the task's logs: the task never
ran. orca reads it out of the evaluation and prints it.

### Secrets

A service declares what it needs:

```yaml
secrets:
  - apiToken
  - signingKey
```

**Secrets arrive as files, at `/secrets/<name>`.** The default is a file and not
an environment variable because the environment is the worse of the two and is
only the default elsewhere out of habit: it is inherited by every child process,
printed by unlucky crash handlers, readable through `/proc`, and dumped whole by
any library that logs its own configuration. A file is read by the one process
that opens it.

That path is not orca's invention. Nomad mounts each allocation's secrets
directory there, on a private tmpfs no other allocation can see, which is why
an application that already reads secrets from files usually needs no change at
all.

The mapping form says where a secret lands when the default is wrong:

```yaml
secrets:
  - db_password                 # /secrets/db_password
  - name: session_key
    env: SESSION_KEY            # an environment variable instead
  - name: gh_key
    path: github.pem            # /secrets/github.pem
```

`env` and `path` are mutually exclusive: each names a destination, and a secret
goes to exactly one place. Two secrets landing in the same place is an error
rather than a race, since one would win, and which one would depend on map
ordering.

A secret can also be interpolated into a larger value, which is the case a file
cannot express:

```yaml
env:
  DATABASE_URL: postgres://postgres:${secret.db_password}@db:5432/postgres
```

**The manifest is the declaration**, whichever form is used. There is no
separate list of secrets to maintain, so it cannot drift from what is actually
used, and `orca secret list` derives what a cluster needs from the manifests
themselves.

**Values live in Nomad's variable store on the cluster and nowhere else.** No
key file, no encrypted file in the repository: nothing to lose and nothing to
distribute. A secret is never written into a job spec: the job carries a
template that reads the variable on the machine when the task starts, so the
value exists only in Nomad's store and in the running container.

```
orca secret set blog/session_key    # prompts, or reads all of stdin (a PEM file works)
orca secret list                    # every referenced secret, set or MISSING
orca secret rm blog/session_key
```

Three properties follow from putting the root of trust in the cluster rather
than in a file:

- **CI never holds a secret.** `apply` does not need secret *values*; only the
  machine does. So a deploy pipeline pushes images and applies manifests while
  holding nothing, which is exactly the problem that an encrypted-in-repo
  design creates rather than solves.
- **Rotation is one command.** Nomad's template watches the variable, so
  `orca secret set` restarts the services using it. There is no redeploy.
- **Apply fails before it touches anything** when a referenced secret is unset,
  naming it. Nomad blocks a task whose variable is missing, so the alternative
  is waiting out a health timeout to be told a raft path does not exist.

The trade is that a rebuilt cluster needs its secrets set again. For tokens
obtained from a vendor's dashboard (which is most of them, and which are
regenerable there) that is a few minutes, and `orca secret list` says exactly
which ones. An encrypted file layer in the repository can be added on top later
if something ever needs to survive the cluster; nothing here forecloses it.

**The best secret is one you never handle.** Anything orca creates, such as a
database password or the dashboards' password and its bcrypt hash, it
generates itself and never asks for. That is the direction to keep pushing.

One secret is one variable at `orca/<group>/<name>`, rather than one variable
per group holding many keys, because listing paths returns no values: finding
out *which* secrets are set never pulls a plaintext off the machine.

Two things this does not defend against, worth naming:

- Without Nomad ACLs, any task that can reach the API can read any variable. At
  one machine nothing can reach it; above one machine, see
  [Binding](#binding-nothing-of-ours-is-ever-public).
- A service that logs its own environment puts the secret in the log store for
  the retention period. Nothing orca can prevent, and the most likely way one
  actually leaks, which is the argument for files being the default.

## Observability

Logs and metrics land in VictoriaLogs and VictoriaMetrics, chosen over the
Grafana stack specifically because each is a single binary writing to a local
directory with no dependency on an object store, a compactor, or a ring. The
components that break are components they do not have.

- `orca logs` queries VictoriaLogs, not Docker, so logs outlive the container
- **vmui**, built into both binaries, is the browser UI for ad-hoc investigation
- `orca logs worker`, `orca logs blog`, `orca logs blog/api "connection refused"`,
  with `-f` to follow and `--since 30m` to widen the window. A mistyped target
  is an error naming what does exist, rather than an empty result that looks
  exactly like a quiet service. A backed-up database's logs include its backup
  runs
- The Nomad UI shows what is running and what has been restarting
- A port declared `metrics` is scraped at `/metrics`, labelled with the
  `group` and `service` it came from; an app's prod and test servers export
  the same names, and those labels keep them apart. Targets register in the
  catalog under one shared name that the scrape config is rendered from, so a
  service is scraped from the apply that deployed it, and the metric store is
  signalled rather than restarted. One per service
- Every machine reports itself through a node exporter, a system job reading
  the host's /proc, /sys and mounts, which is why it is platform rather than
  something a manifest could declare. It registers with a `node=<name>` tag
  that becomes its series' label, the same way a service's `group` and
  `service` do. Its port is dynamic: it runs on every machine, and a fixed
  number would be one no service could take anywhere. Container veth
  interfaces and Nomad's allocation mounts are left out, since both are
  renamed on every deploy and would add a fresh set of series each time
- The status page and `orca top` are the glance: whether anything needs a
  look, each machine's CPU, memory, disks and network, how full the stores
  are, and each service's health and usage, judged once in the status
  service. `orca top` reads it over SSH, so it works when ingress is what is
  broken. The page opens any service's logs, read by the same query code as
  `orca logs`
- Grafana is optional, stateless, and provisioned from a file in git, never a
  stack with its own database

Nothing orca runs for you may depend on another of them being healthy.
If Grafana dies, collection and querying continue.

## Hands off

What orca does without being asked:

- restarts crashed containers and reschedules failed work
- **caps every disk consumer**: Docker logs, VictoriaLogs, VictoriaMetrics
- renews TLS certificates
- applies OS security updates, never touching the pinned Docker/Nomad binaries
- backs up, off the box, every database that names a target, nightly unless it
  says otherwise

Disks filling up is the most common way a box like this dies, and it is fixed by
hard caps set on day one rather than by alerting after the fact.

## What orca deliberately does not do

| Not doing | Why |
|---|---|
| Build images | You have CI, or you run `docker build && push`. |
| Autoscale | You know how much capacity you need. |
| Isolate apps from each other | You wrote all of them. The entire design rests on this. |
| Build an overlay network | Use the provider's private network or Tailscale. |
| Multi-region | Not a thing you have. |
| A web deploy UI | CLI and git. |
| HA anything | One machine is one failure domain. Backups, not replicas. |
| Model databases, roles or schemas | You hold superuser. `CREATE DATABASE` is yours. |
| Alerting | Retention caps remove the main reason for it; `orca top` covers the rest on demand. |

## Binding: nothing of ours is ever public

**Nothing orca deploys for itself binds a public interface. Public exposure is
opt-in, and the only opt-in is a service's `ports:`.**

This is the same reasoning as loopback on a single machine, one scale up: the
way to not have to defend a listener is not to create one. It is also why there
is no Nomad ACL and no mutual TLS: there is no network path to protect.

Two host networks are declared in the Nomad client config, so *where a thing
binds* is a property of its job spec rather than something a firewall has to
correct afterwards:

| Network | Binds | Used by |
|---|---|---|
| `internal` | the container bridge, or the private IP once there is a cluster | everything orca runs for itself, and every port that is not public above one machine |
| `public` | the default-route interface | ingress on 80/443, and raw protocol ports |

| | one machine | several |
|---|---|---|
| Nomad HTTP / RPC / Serf | loopback | **private** |
| orca's own stores, the resolver | the container bridge | **private** |
| Ingress 80/443 | public | public |
| A service's `7777: tcp` | public | public |
| A service's `5432: internal` | **nothing published at all** | private, at 5432 |
| A service's `8080: shop.example.com` | **nothing published at all** | private, at 8080 |

An internal or hostname port publishes no host port on a single machine. The
service registers its allocation's own address (every allocation has one, on a
per-node bridge), which every container and ingress reach directly and nothing
outside the machine can reach. Above one machine those addresses stop being
unambiguous, because every node's bridge uses the same range, so the port is
published there on the private network. It keeps its own number, because the
resolver answers with an address and no port, and `db.shop:5432` has to mean
5432 on whichever machine db landed on. The cost is that two services
listening on the same port cannot share a machine above one; Nomad places them
apart, or reports the service unplaced and why.

Naming a protocol therefore *means* public, and is the only thing that takes a
host port on the public interface.

**A private network is required for more than one machine.** Each node declares
`private_ip`, and bootstrap refuses to continue if that address turns out to be
on the default-route interface. orca will not bind a cluster to a public
network, and failing loudly beats a cluster that works while also offering an
unauthenticated scheduler API to the internet.

The public address is never declared. It is resolved on the machine from the
default route, because orca needs to know which interface *not* to use, not
what it is called.

### The firewall

The binding rules mean nothing of orca's is listening on a public address in
the first place. The firewall is the second line: it drops everything arriving
from outside that no manifest asked for, so a service that binds a public
address by mistake is still not reachable.

What is open, and why:

| Port | Because |
|---|---|
| 22 | unconditionally: a firewall that can lock you out of the machine it protects is a worse outage than the one it prevents |
| 80, 443 | ingress is running |
| every raw tcp/udp port | a manifest asked for it |
| ICMP | path-MTU discovery and connection errors depend on it; dropping it does not buy security and breaks things subtly |
| DHCP replies | for a machine that leases its address |

Everything else arriving on the public interface is dropped, not rejected, so
a scan gets silence rather than a closed-port answer.

**The rules are derived, never written.** A port is open because something
declared it, so the ruleset is regenerated on every apply and cannot drift from
what is deployed. Deleting a service closes its port on the next apply.

Two details that make it work rather than only look like it does:

- **Filtering happens in prerouting, not input.** A published container port is
  destination-NAT'd by the CNI and then traverses the *forward* path, so an
  input-only ruleset looks correct and blocks none of them. At the prerouting
  hook the packet still carries the port it was sent to, before rewriting.
- **orca uses its own nftables table**, deleted and recreated rather than a
  global flush, so the rules Docker and the CNI maintain are left intact.

It also closes the one hole the binding rules cannot: **a container can reach
the private network**, so above one machine the scheduler API would be
reachable from inside a container — and a container that can submit jobs can
run anything on the machine as root. Traffic from the container bridge to
Nomad's ports is dropped on both paths it can take: input, for its own
machine's scheduler, and forward, for every other machine's, which is routed
out, masqueraded, and arrives looking like the machine itself. The resolver and
orca's own stores stay reachable, because services are meant to use those.

## Growing to more than one machine

Everything above is chosen so that this is a config edit:

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

Then `orca bootstrap`, then `orca apply`. The components that keep data on one
disk (ingress, and monitoring's two stores and status page) run on the first
server listed unless `node:` under `ingress:` or `monitoring:` says otherwise.
Wherever ingress runs is the machine your DNS points at.

The setting exists because above one machine the first server does two jobs
that every other machine depends on: every request enters through ingress, and
every log line and metric lands in monitoring. On one machine this cannot be
helped. On several, a flood of logs slowing the front door, or one machine
taking away both the traffic and the means to see why it stopped, can be. It
is only a placement: moving monitoring starts its stores empty on the new
machine, with the old data left on the old one's disk, and moving ingress
means a DNS change and new certificates. Apps keep working because services
resolve each other through the catalog rather than by address. Stateful
services stay where their data is. You move a workload with `node: box1` in
its manifest.

What changes at the second machine is that everything cluster-internal moves
from loopback to the private network; see [Binding](#binding-nothing-of-ours-is-ever-public).
Nothing becomes publicly reachable that was not already. Bootstrap moves the
first machine's scheduler (a lone server whose address changes is handed its
new one through Nomad's `peers.json`); apply moves the workloads, because an
allocation keeps the address it was placed with. Platform jobs carry an
`orca.network` meta key above one machine for exactly that reason: without a
change to their spec, apply would leave the stores on the first machine's
bridge address.

## Open questions

- **Volume backups.** A templated database is dumped on a schedule to a
  target it names. A plain service's own `volume:` has no backup story yet;
  probably a periodic snapshot to the same object store, but it is not
  designed.
- **Which templates exist.** `postgres`, `redis` and `garage`.
- **Reaching an internal port from your own machine.** A profiler, a database
  console: anything `internal` is reachable from the cluster and nothing else,
  which is the point, and also means looking at it by hand is an SSH tunnel to
  an allocation address you have to look up. Probably `orca forward
  <group>/<service> <port>`; not designed.
- **Stages.** Two groups that run the same services, prod and test, are
  written out twice, differing in a few values. Variables take those values
  out; the service files are still duplicated, and can drift. One directory
  of services instantiated once per stage, each with its own variables, would
  remove that, at the price of merge rules for what differs structurally.
  Not built until the duplication costs something.
- **Multi-machine, on hardware.** Everything above one machine is built to the
  rules in this file and tested against rendered specs, not yet against a
  real second machine.
