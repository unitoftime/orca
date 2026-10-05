# Managed by orca — do not edit by hand.
# Rendered from cmd/orca/templates/host/nomad.hcl; __BIND_IP__ is substituted by
# nomad.sh on the host.

data_dir   = "{{DATA_DIR}}/nomad"
datacenter = "{{DATACENTER}}"
name       = "{{NODE_NAME}}"
bind_addr  = "__BIND_IP__"

# Nomad refuses to start with a loopback bind and no explicit advertise: it
# treats "defaulting advertise to localhost" as a mistake, because on a real
# cluster it is. Here it is the intent, so it is stated outright.
advertise {
  http = "__BIND_IP__"
  rpc  = "__BIND_IP__"
  serf = "__BIND_IP__"
}

# The HTTP API always answers on loopback, whatever bind_addr is. orca reaches
# Nomad by running commands on the machine over SSH, so it wants a stable local
# address that does not change when a second machine turns bind_addr into a
# real IP.
addresses {
  http = "__HTTP_ADDRS__"
}

# On a single-node cluster __BIND_IP__ is 127.0.0.1, so the HTTP API, RPC and
# Serf are all unreachable from off the box. Above one machine it is the node's
# private_ip, and bootstrap refuses an address on the default-route interface.
#
# The API answers nothing without a token. The firewall cannot be what keeps
# a container away from it, because Nomad puts a socket to the API inside
# every task; with ACLs on, a task holds no token to ask with. orca's own is
# in {{TOKEN_PATH}}, readable by root, and what each job may do without one
# is in the policies bootstrap and apply write.
#
# There is no TLS stanza: ACLs guard the API, not the ports machines talk to
# each other on, and those the firewall leaves open to the cluster's own
# machines and nobody else on the private network.
acl {
  enabled = true
}

{{NOMAD_SERVER_STANZA}}

client {
  enabled = true

{{NOMAD_CLIENT_JOIN}}

  meta {
    node_name = "{{NODE_NAME}}"
  }

  # Where a thing binds is a property of its job spec, not something a
  # firewall has to correct afterwards. Two named networks, and the rule is
  # that nothing orca deploys for itself may use "public": public exposure is
  # opt-in, and the only opt-in is a service's port naming a protocol.
  #
  #   internal  everything that is not public: orca's own stores, and every
  #             internal or ingress-routed port above one machine. The
  #             container bridge on a single machine, the private NIC once
  #             there is a cluster. Either way, reachable by every container
  #             and by nothing outside the machines.
  #   public    ingress (80/443) and raw tcp/udp ports, deliberately
  #
  # Volumes are bind mounts under {{DATA_DIR}}/volumes rather than host
  # volumes declared here, so a service growing a volume never means
  # re-bootstrapping the machine. Node pinning is a constraint in the job.
  host_network "internal" {
    interface = "__INTERNAL_IFACE__"
  }

  host_network "public" {
    interface = "__PUBLIC_IFACE__"
  }
}

plugin "docker" {
  config {
    endpoint = "unix:///run/docker.sock"

    # Every workload here is yours. Privileged containers and host volume mounts
    # are allowed because there is no trust boundary between jobs to defend.
    allow_privileged = true

    # Nomad labels containers with alloc_id and nothing else unless asked.
    # Without these, every log line arrives at the log store knowing only a
    # container id, so "show me the API server's errors" has no field to
    # filter on. This is what makes a log line self-describing.
    extra_labels = ["job_name", "task_group_name", "task_name", "namespace"]

    volumes {
      enabled = true
    }

    # Where a pull gets registry credentials: /usr/local/bin/docker-credential-orca,
    # which reads them from Nomad's variable store (`orca registry login`). A
    # registry with none is pulled anonymously.
    auth {
      helper = "orca"
    }
  }
}

ui {
  enabled = true
}

telemetry {
  collection_interval        = "10s"
  disable_hostname           = true
  prometheus_metrics         = true
  publish_allocation_metrics = true
  publish_node_metrics       = true
}
