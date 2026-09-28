# How orca works

This is the mental model behind orca and the rules it follows to keep your
services and data safe. For how to write service files, the commands and the
settings, see the [README](../README.md).

## The idea

orca turns a server you own into a place you can deploy things, with HTTPS,
names between services, logs, metrics, a firewall and backups already set up.
You describe what should run in YAML files in a directory, and `orca apply`
makes the server match.

The test it has to pass: adding a new service should be a few lines of YAML
and one command, and should never involve thinking about a machine.

## The pieces

| Piece | What it is |
|---|---|
| Machine | A server orca manages, listed in `cluster.yaml`. Usually one. |
| Group | One directory of service files, such as `shop`, `blog` or `notifier`. The directory name is the group name. |
| Service | One container in a group, such as `web`, `db`, `worker` or `api`. |
| Template | A service orca configures for you: `postgres`, `redis` or `garage`. |
| Target | A backup destination (an S3-compatible bucket). The one kind of service file that runs no container. |
| `orca` | The reserved group holding what orca runs for itself, shown as `orca/traefik`, `orca/dns` and so on. |

```
infra/
  cluster.yaml      the machines
  shop/             group "shop"
    web.yaml
    db.yaml
  blog/prod/        group "blog-prod"
    api.yaml
```

The directory is the identity. Nothing inside a file repeats the group name,
so adding a group is creating a directory and renaming one is `mv`. Nested
directories join with dashes. A directory named `orca` is refused.

## What `orca apply` does

The files are the desired state. `orca apply` compares them with what is
running and changes only the difference.

1. **Reads and checks every file.** Unknown keys, bad values and two services
   claiming the same public port are errors before anything touches the
   server. `orca validate` does only this step.
2. **Pins every image to a digest.** `image: ghcr.io/you/shop:latest` is
   looked up and deployed as the exact image the tag points at right now. So
   pushing a new `latest` and running apply redeploys it, and `orca status`
   shows what is really running. The images orca chooses itself (templates,
   its own services) are pinned in orca's code instead, so a database is
   never restarted because an upstream tag moved; upgrading one is upgrading
   orca.
3. **Checks what the server will need.** Every secret a service uses must be
   set, and every private image must come from a registry the cluster has a
   login for. A volume must hold data its service can start on: Postgres
   refuses another major version's data, and a database password generated
   over existing data would lock everything out. If not, apply stops and
   names what is wrong, before changing anything.
4. **Builds a job for each service and asks Nomad what submitting it would
   do**, without submitting anything. Nomad compares it with what it runs and
   says whether it changed, whether that restarts anything or applies in
   place (a replica count, say), and whether there is room to place it.
   Unchanged services are left alone, and a change with no room refuses the
   whole apply. `orca plan` stops here and prints the changes.
5. **Asks before stopping anything.** A running service that no longer appears
   in the files will be stopped, but apply asks first, because a missing file
   can also mean you ran orca from the wrong directory. With no terminal to
   ask at (in CI, say), it refuses unless you pass `--yes`.
6. **Updates the firewall** and creates any secrets a template needs.
7. **Stops removed services, then deploys new and changed ones.**
8. **Waits for every service in scope to be healthy**, including ones it did
   not change. What it deployed is judged by Nomad's verdict on the new
   version: healthy once its new copies pass their checks, failed (and rolled
   back by Nomad) if they do not within a few minutes. If anything is
   unhealthy, apply exits non-zero. A green apply in CI means everything is
   actually running, not just that something was submitted.

Only one apply runs at a time. A second one, from CI or another machine,
stops at once and says who holds the cluster; one that dies without
finishing gives way within a minute.

A service with `replicas: 2` or more, no volume and no raw port is deployed
blue/green: a full new set of copies starts beside the old one and is sent
traffic only once all of them pass their checks, then the old set drains and
stops. It briefly needs room for both. Anything else is replaced one copy at
a time.

When nothing changed and nothing is wrong, apply says so and exits quickly,
so it is safe to run on every commit.

`orca apply shop` narrows all of this to one group and leaves everything else
alone. A plain `orca apply` covers every group, including `orca`.

There is no rollback command. The files say what runs, so going back is
reverting the edit that moved forward. Putting an image tag in a `vars.yaml`
keeps that edit to one line.

## What runs on your server

`orca bootstrap` prepares a machine, and `orca apply` runs the rest as jobs in
the `orca` group.

| Component | What it is for |
|---|---|
| Docker | Runs the containers. orca caps its log files so they cannot fill the disk. |
| Nomad | The scheduler: starts, restarts and places containers, and stores the cluster's secrets. |
| Traefik (ingress) | Routes hostnames to services, with HTTPS certificates from Let's Encrypt that renew themselves. Also serves the web dashboards. |
| CoreDNS (the resolver) | Runs on every machine so services can find each other by name. |
| Vector | Runs on every machine, reads every container's logs and sends them to the log store. |
| VictoriaLogs | Stores logs, with a hard disk limit. `orca logs` reads from it. |
| VictoriaMetrics | Collects and stores metrics from services' `metrics` ports, Nomad and every machine. |
| Node exporter | Runs on every machine and reports its CPU, memory, disks and network. |
| Status page | orca itself, serving `status.<domain>`. `orca top` shows the same in a terminal. |
| Firewall | nftables rules generated from your files on every apply. Not a job. |
| Security updates | Installed automatically from the OS's security updates. Docker and Nomad are pinned and not touched by them. |
| Registry helper | Lets Nomad pull private images using the cluster's own registry logins. |

Each of these can be switched off in `cluster.yaml`; see the
[README](../README.md#configuring-the-cluster).

**None of them depends on another being healthy.** If the log store is down,
Vector keeps logs on disk (up to a limit) and sends them later. If Traefik
is down, only HTTP routing stops; raw ports, databases and log collection
carry on. `orca logs` and `orca top` reach the server over SSH, so they still
work when ingress is what is broken. The status page shows what it can and
names what it cannot read.

**Disks are capped from day one**, because a full disk is the most common way
a server like this dies. Docker's log files and the log store have hard size
limits. The metric store is limited by its retention time and stops storing
new data when free disk falls below a floor (`min_free`). Volumes are not
capped: a volume's size is what it is expected to hold, and `orca status`
shows each one's use against it.

## Networking

**Nothing is public unless a service's `ports:` says so.** A `tcp` or `udp`
port is published on the server's public address. A hostname port is reached
through ingress on 80 and 443. An `internal` or `metrics` port can be reached
only by other services. Nothing orca runs for itself listens on a public
address, apart from ingress.

Services find each other by name, never by IP address:

| Name | From | Means |
|---|---|---|
| `db` | inside group `shop` | shop's `db` |
| `db.shop` | anywhere | shop's `db` |
| `db.shop.orca` | anywhere | the same, fully spelled out |

A name is looked up when a connection is made, so a service that moves or
restarts is simply found at its new address. Ordinary connection strings work,
so off-the-shelf images need no orca-specific setup.

A group is a namespace, not a security boundary. Any service can reach any
other; orca assumes you wrote all of them.

**The firewall is generated from your files** on every apply, so it cannot
drift. From outside, it lets in SSH (always, so it can never lock you out),
80 and 443 while ingress runs, the `tcp` and `udp` ports your files declare,
and the ICMP and DHCP traffic a machine needs. Everything else is dropped
silently. Containers are also blocked from reaching Nomad's API, since
anything that can submit jobs can run anything on the machine.

**Above one machine**, the machines need a private network (your provider's,
or Tailscale or WireGuard; orca does not build one). Each machine lists its
`private_ip`, and bootstrap refuses one that is on the public interface.
Internal and hostname ports are then published on the private network at
their own port number, so two services using the same port cannot share a
machine. Ingress, the log and metric stores and the status page run on the
first server unless you place them elsewhere. A service with a volume stays
on the machine that holds its data.

## Your data is safe by default

Removing a service from a file has to do something, or the files would not be
the desired state. But a bad merge should never destroy a database. So
**apply stops; only purge deletes.**

| Command | Services | Data |
|---|---|---|
| `orca apply`, service removed from the files | stopped | kept |
| `orca stop <group>` | stopped | kept |
| `orca purge <group>` | stopped | **deleted, for good** |

`orca purge` is hard to run by accident:

- It refuses while the group's directory still exists. You delete the
  directory, apply, and only then purge.
- It refuses when the files do not load at all, because then it cannot tell
  what is still in use.
- It lists everything it will delete (jobs, volumes and the group's secrets)
  and asks you to type the group's name.

`orca stop` is the quick way to take a group down without editing files. The
next apply brings it back, and `orca stop` tells you so.

A volume is a directory on the server at
`/var/orca/volumes/services/<group>/<service>`. It survives deploys, restarts
and stops, and the service is pinned to the machine that holds it. When a
service is removed, `orca status` lists the data it left behind, so kept data
is never invisible.

**Backups are scheduled dumps**, not replicas: one machine is one failure
domain, so a copy on the same machine protects little. A Postgres or Redis
service that names a target is dumped to it nightly by default, and the
latest 14 are kept. The target's credentials are secrets you set, and apply
refuses to deploy a backup that has none, rather than letting it fail quietly
every night. A failed backup shows as `failed` in `orca status`.

A Postgres backup holds every database, and the roles that own them and are
granted on them, so it restores onto a server that has never seen them. The
`postgres` superuser is left out: its password is the cluster's own secret.

`orca db restore` puts a backup back. Postgres loads each database beside the
one it replaces and swaps it in only once it has loaded, so a restore that
fails changes nothing, and one that succeeds leaves each database exactly as
the backup had it. The database it replaced is kept on the server, renamed.
Redis is stopped while its data is replaced, and the data it replaced is kept
under `/var/orca/pre-restore/` on the machine.

## Secrets

A service lists the secrets it needs in its file, and you set the values with
`orca secret set`. The files are the only list, so `orca secret list` can
tell you exactly what is set and what is missing.

**Values are stored only in the cluster**, in Nomad. Not in the repository,
not in a key file, not in a job. They reach a container only when it starts.
One consequence: CI can deploy without holding any secrets. The trade: a
rebuilt cluster needs its secrets set again, and `orca secret list` shows
which.

**Secrets arrive as files by default**, at `/secrets/<name>`. Environment
variables leak easily: child processes inherit them, and libraries that log
their configuration print them. A file is read only by what opens it. A
secret can go into an environment variable, or into part of a value such as a
connection string, when an application needs that.

Setting a new value restarts the services that use it; no redeploy needed.

**Generated secrets.** Templates create their own secrets on the first apply,
named after the service: a `db` service gets `db_password`, which other
services use as `${secret.db_password}`. Nobody types or sees the value. It
is created once and never replaced, because a database initialised with a
password is locked out if it changes. `orca secret set` and `orca secret rm`
refuse a generated secret unless you pass `--force`.

The cluster's registry logins and the dashboards' password live in the
cluster in the same way.

## Templates

A template is a service orca configures for you. It becomes an ordinary
service, so deploys, logs, names and status work the same way. The template
fixes what is easy to get wrong (image version, port, health check, where
the volume mounts and who owns it, passwords). You choose only the size:
`cpu`, `memory` and `volume`.

**postgres** (17 or 16). Port 5432, internal. A generated password,
`<service>_password`. The two memory settings Postgres gets wrong by default
are set from your `memory`: `shared_buffers` to a quarter and
`effective_cache_size` to a half. Everything inside is yours: you connect as
the `postgres` superuser and create databases and roles yourself.

**redis** (8.10). Run as a database, not a cache: every write is saved to disk
each second, and nothing is ever evicted. Data is capped at three quarters of
`memory`, so a full Redis refuses writes with an error your application sees,
rather than being killed. JSON, search and time series are built in. The
generated password is read from a config file, never from the environment.

**garage** (2.3.0). S3-compatible object storage on your own server, on one
machine with no replication. A fresh Garage is not usable until it is set up,
so orca does that too: it creates a bucket named `<group>-<service>` and an
access key, stored as `<service>_key_id` and `<service>_secret_key`. Its admin
interface is reachable only from inside the service itself. Garage has no
backups; keep what matters somewhere else too.

## What orca deliberately does not do

| Not doing | Why |
|---|---|
| Build images | CI, or `docker build` and `docker push`, already does. |
| Autoscale | You know how much capacity you need. |
| Isolate services from each other | You wrote all of them. |
| Build an overlay network | Use your provider's private network, or Tailscale or WireGuard. |
| High availability | One machine is one failure domain. Backups, not replicas. |
| Manage databases, roles or schemas | You are the superuser. |
| A web UI for deploying | The CLI and your files are the interface. |
| Alerting | Disk caps remove the main reason for it; `orca top` answers "is anything wrong" when you ask. |

## Not built yet

- `orca exec`: a shell in a running container.
- `orca forward`: a tunnel from your machine to an internal port. For now,
  that means an SSH tunnel.
- On-demand health checks, and alerting.
- Backups of a plain service's `volume:`. Only Postgres and Redis are backed
  up.
- Stages: one set of service files deployed as prod and test with different
  values. Today each stage is its own directory, and variables carry the
  values that differ.
- More than one machine is built and tested against generated configuration,
  but has not yet been run on real hardware.
