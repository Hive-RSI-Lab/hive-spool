# hive-spool

A door module: the Hive takes requests from a JSON Lines file a relay appends (inbox) and appends its answers and what it delivered to another (outbox), so a system outside the Hive's network namespace, like the relay feeding a shadow Hive, can hand it work and collect the answers without the loopback API or the daemon's token.

- How it works, and both file formats: [docs/README.md](docs/README.md)
- Tests: `go test ./...` (a fake Hive API and the two files in a temp folder)
