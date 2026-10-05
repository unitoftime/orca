- [ ] I feel like orca secret edit shouldnt return back certificates. its just too much and not really useful for users i dont think. also one bug is that i did orca secret edit and it showed the certificats as changing even though they were unedited

- [ ] Reaching an internal port from your own machine (pprof, a DB console).
      Probably `orca forward <group>/<service> <port>`; not sure it's the right
      shape yet.
- [ ] Stages: one set of service files deployed as prod and test, each with
      its own vars. Held back until prod/test duplication actually bites.
- [ ] Export and import secrets for resetting clusters. just password protected encrypted files I think. and alternatively just plaintext export. idk though because I kind of want to be able to just edit all my secrets in a file and encrypt them and store them in the repo easily. I'm not sure how to capture that into an encrypted file. But I think the main thing i want is this: export cluster secrets, import cluster secrets, view and edit cluster secrets, encrypt cluster secrets so I can transfer or store them someplace externally.

Possible later:

- An age/sops file layer for secrets on top of Nomad's variable store.
- WAL archiving for Postgres, for a recovery point of seconds rather than the
  backup interval.
- Nomad ACLs, if the private network between machines ever stops being
  trusted.
