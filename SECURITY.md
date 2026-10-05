# Security

## Reporting a vulnerability

Please report it privately, through GitHub: on this repository's **Security**
tab, choose **Report a vulnerability**. Please do not open a public issue for
it.

## What orca defends, and what it does not

orca is built for one person running services they wrote on servers they own.
Its defenses face outward. Inside the cluster there are deliberately very few.

**What it defends**

- **From the internet, a server answers only SSH and the ports your files
  publish.** The firewall is generated from your files on every apply. On a
  private network, anything that is not one of the cluster's machines is
  treated the same way.
- **Nomad answers nothing without a token.** Whatever can ask Nomad to run a
  job can run anything on every machine. The token is made by `orca
  bootstrap` and kept on each machine in a file only root can read. A
  container holds none.
- **The dashboards are published only over HTTPS**, behind a password
  generated for the cluster. How fast it can be guessed from one address is
  limited, a request that another site's page tells your browser to make is
  refused, and the stores behind them will not delete what they hold for
  anyone. Nomad's own UI is never published.
- **Secrets are stored in the cluster**, not in your files, and reach a
  container only when it starts.
- **What orca installs is pinned**: Nomad, Docker and the network plugins by
  checksum, and every image orca chooses by digest.

**What it does not**

- **Services are not isolated from each other.** Any container can reach any
  other's ports, and a group is a name, not a boundary: what keeps one group's
  secrets out of another group's services is orca's file format, not a
  permission. Do not run code you do not trust.
- **Root on any machine is the whole cluster.** The token is there, and so is
  everything it protects. Anyone who can SSH in as root can do anything orca
  can.
- **The private network between machines is trusted to carry traffic.** orca
  does not encrypt what machines say to each other, and the ports Nomad's
  machines use among themselves ask for no token: the firewall keeps them to
  the cluster's machines, and with `firewall: false` on a shared network
  that rule is the only one orca still writes.
- **A secrets export is as strong as its passphrase.** Anyone who has the
  file can try guesses for as long as they like.

More than one machine has been built and tested against generated
configuration, but has not yet been run on real hardware.

[How orca works](docs/how-it-works.md) describes each of these in more
detail.
