- [ ] Reaching an internal port from your own machine (pprof, a DB console).
      Probably `orca forward <group>/<service> <port>`; not sure it's the right
      shape yet.
- [ ] Stages: one set of service files deployed as prod and test, each with
      its own vars. Held back until prod/test duplication actually bites.
- [ ] Redis logs "vm.overcommit_memory must be enabled": without it the fork
      for a snapshot or AOF rewrite can fail under memory pressure. Decide
      whether bootstrap should set it.

Possible later:

- An age/sops file layer for secrets on top of Nomad's variable store.
- WAL archiving for Postgres, for a recovery point of seconds rather than the
  backup interval.
- Nomad ACLs, if the private network between machines ever stops being
  trusted.
