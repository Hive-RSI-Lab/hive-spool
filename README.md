# hive-spool

A door module: the Hive takes requests from files dropped in a spool folder (inbox) and writes what it delivered back as files (outbox), so a system outside the Hive's network namespace, like the box feeding a shadow Hive, can hand it work and collect the answers without the loopback API or the daemon's token.
