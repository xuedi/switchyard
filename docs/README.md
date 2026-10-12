# Architecture

How switchyard works, one topic per file: the general picture, the code is the detail.

| File | What it covers |
|---|---|
| [overview.md](overview.md) | the two halves (dashboard and kit), where they run, routes, the container |
| [release-state.md](release-state.md) | releases, slots, the state ConfigMap, the canary, quick and rollback flows |
| [kit.md](kit.md) | `switchyard manifests`, `switchyard.sh`, the build Job and the app's hooks |
| [migrations.md](migrations.md) | why a migration may only add, the guard, Deploy anyway |
| [routers.md](routers.md) | how traffic is split, the router adapter interface, Caddy |
| [polling.md](polling.md) | `/state`, the shared snapshot, idle mode, offline vs. error |
| [starting-jobs.md](starting-jobs.md) | the clicks, the `starter` guards, the Job copy, `check` |
| [cluster-access.md](cluster-access.md) | the two service accounts, RBAC and the admission policy |
