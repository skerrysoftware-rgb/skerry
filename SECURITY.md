# Security

skerry runs untrusted code (dependencies, repository scripts, coding
agents) and is meant to keep it inside each workspace's VM. A way out of
that is a vulnerability: code in a VM running on, reading from, or
writing to your machine outside its checkout; reaching your machine or
local network; getting a service's password; or affecting another
workspace.

## Reporting

Please report vulnerabilities privately, by email to
**skerry.software@gmail.com**, not in a public issue. Include what you
found, how to reproduce it, and the skerry version (`skerry version`).

You'll get a reply within a few days. Please give us a chance to fix the
problem before you disclose it; we'll agree on a date with you and credit
you in the fix unless you'd rather not be named.

The [README's Security section](README.md#security) describes what skerry
protects against, and the known gaps it doesn't cover yet.
