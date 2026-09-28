// Package images is every container image orca chooses itself: the
// platform's, the templates', and the helpers backups and templates run.
//
// Each is an exact tag and the digest it pointed at when it was pinned. The
// tag is for people; the digest is what runs. Apply submits these as they
// are and never looks them up, so an image upstream rebuilt under the same
// tag changes nothing on the machines until it is pinned here, and moving
// one is a commit with a reviewable diff.
//
// To upgrade, change a tag and run `make pin-images`, which resolves every
// tag in this file and rewrites its digest. Run on its own, it picks up
// rebuilds of the same tags, which is how base-image security fixes arrive.
package images

// Templates.
const (
	Postgres17 = "postgres:17.11-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
	Postgres16 = "postgres:16.15-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"
	Redis810   = "redis:8.10.2-alpine@sha256:3811787313eba226a2ef38658c6ccb91cd5e110edc89c37767de373120a0e5a0"
	Garage230  = "dxflrs/garage:v2.3.0@sha256:866bd13ed2038ba7e7190e840482bc27234c4afaf77be8cfa439ae088c1e4690"
)

// The platform.
const (
	CoreDNS         = "coredns/coredns:1.12.4@sha256:986f04c2e15e147d00bdd51e8c51bcef3644b13ff806be7d2ff1b261d6dfbae1"
	NodeExporter    = "prom/node-exporter:v1.12.1@sha256:1b4e4438faca4dd7e001dd445d161a4a2091b0fededa84093b3a8dfeae1f1be0"
	Traefik         = "traefik:v3.6.25@sha256:31267173a15b4944e797a76ffd9c419707c8d8b32fe5b610f80cd0cfa05f372d"
	Vector          = "timberio/vector:0.51.0-alpine@sha256:cef2a4d33b43b3e9196ba0bd9f1d8b9006a03bfe15363514c5a0f78b8cb8e9d9"
	VictoriaLogs    = "victoriametrics/victoria-logs:v1.52.0@sha256:47b820890d64c4575a2a0a46415dcd8a4fd59a0f1fcd6a377693d7aea639442e"
	VictoriaMetrics = "victoriametrics/victoria-metrics:v1.152.0@sha256:86ca5fdb6d87d56ba047b044039019ba2bd9042b36e35f6ea34e437b6c825cef"
)

// Helpers.
const (
	// Alpine runs the status page's orca binary, until orca publishes an
	// image of its own, and scripts for images that have no shell.
	Alpine = "alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

	// Rclone is the S3 client backups use. Chosen over MinIO's mc because it
	// speaks every S3 dialect, configures entirely from environment
	// variables with no config file, and is still published where it can be
	// pulled; mc's images have disappeared from both Docker Hub and quay.io.
	Rclone = "rclone/rclone:1.71.0@sha256:fd635aecd9667ee3c3bf920d14118090d4f2a83a080c1fa77e0bafbd4587ca87"
)
