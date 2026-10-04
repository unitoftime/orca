# Example cluster

A small cluster you can point at your own server and bring up. It runs:

- `hello/web`: a web server at `https://hello.<domain>`, two replicas
- `hello/worker`: a background job that logs a line every 20 seconds
- `shop/db`: Postgres, with a password orca generates
- `shop/app`: an app that connects to `shop/db` by name and logs whether it got
  through

A fresh Debian or Ubuntu server with 2 GB of memory is plenty.

## Run it

Copy the directory somewhere of your own:

```
cp -r example ~/infra
cd ~/infra
```

Point it at your server. Say its IP is `198.51.100.7`:

- in `cluster.yaml`, set `host: root@198.51.100.7`
- in `cluster.yaml`, set `monitoring.domain: 198-51-100-7.sslip.io`
- in `vars.yaml`, set `domain: 198-51-100-7.sslip.io`

sslip.io turns any name ending in your IP with dashes into that IP, so HTTPS
works without buying a domain. With a domain of your own, use it in both places
instead, and add a DNS record for `hello.<domain>` and a wildcard
`*.<domain>` pointing at the server.

Check the files, then bring the server up and deploy:

```
orca validate                   # touches no server
orca bootstrap                  # installs Docker and Nomad; asks for the
                                # server's password if your key is not on it yet
orca apply
```

## Look around

```
curl https://hello.198-51-100-7.sslip.io   # answered by one of the two replicas
orca status                                # every service and its health
orca top                                   # machines and services at a glance
orca logs hello/worker -f                  # follow the worker
orca logs shop/app                         # "connected as postgres to postgres"
orca password                              # for the dashboards, user admin
```

Then open `https://status.198-51-100-7.sslip.io` for the status page, with
logs and metrics beside it.

Try a change: set `replicas: 3` in `hello/web.yaml`, run `orca plan` to see
what would change, then `orca apply`.

## Adding backups

Declare an S3-compatible store (R2, Backblaze, Wasabi, MinIO, S3) as a new
group:

```yaml
# storage/offsite.yaml
name: offsite
target: s3
endpoint: https://<account>.r2.cloudflarestorage.com
bucket: orca-backups
```

Set its credentials, uncomment the `backup:` block in `shop/db.yaml`, and
apply:

```
orca secret set storage/offsite_key_id
orca secret set storage/offsite_secret_key
orca apply
orca db list shop/db                       # after the first nightly run
```

## Taking it down

```
orca stop hello                            # stop a group, keep its data
```

To remove a group for good, delete its directory, run `orca apply`, then
`orca purge <group>` to delete its data.
