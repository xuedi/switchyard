<p align="center">
  <img src="assets/switchyard.svg" width="160" alt="switchyard, a release crate on a wagon rolling towards a switch, the lever thrown to track A, the signal green">
</p>

<h1 align="center">switchyard</h1>

<p align="center">
  <a href="https://github.com/xuedi/switchyard/releases"><img src="https://img.shields.io/badge/version-0.6.0-9141ac.svg" alt="Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-EUPL_v1.2-9141ac.svg" alt="License"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.25+-9141ac.svg" alt="Go"></a>
  <img src="https://img.shields.io/badge/deps-stdlib_only-9141ac.svg" alt="Standard library only">
</p>

<p align="center"><b>The tiny canary and rollback tool for image-based apps on a small Kubernetes.</b><br>
Your app pushes a tagged image; switchyard moves a Deployment from tag to tag.<br>
One Go binary next to kubectl. No operator, no CRDs, no router.</p>

---

Argo Rollouts and Flagger are built for clusters with a service mesh and a team to run them.
On a small cluster with one or two apps that is more machinery than the apps themselves, and a
plain `kubectl set image` gives you no canary, no guard for the migrations and no history to
roll back to. switchyard is the middle ground: pick a Deployment, and every newer tag in its
registry gets a **Canary** and a **Deploy** button, every tag that went live a **Roll back**.

## Why switchyard

- **Two drop-downs to add an app.** Pick a namespace, pick its Deployment. switchyard reads the
  registry and the credentials from the Deployment itself; everything else is optional.
- **A canary without a router.** A copy of the Deployment runs the new tag with the same pod
  labels, so the app's own Service sends it a share of the traffic: one pod next to the old
  ones, then as many as the old ones. Small sites have few visitors, so during every step
  switchyard requests your pages from a new pod and an old one, round after round, and aborts
  at the first error.
- **Rollback to any kept tag.** The last five tags that went live are kept; Roll back puts one
  back at once, and its image is usually still on the nodes.
- **A guard against destructive migrations.** The old code keeps running on the migrated
  database, during a canary and after a rollback. Before anything moves, switchyard runs your
  pending-SQL command in the new image and stops on drops, renames and `NOT NULL` without a
  default, with a **Deploy anyway** for the planned last step. Then the migrate command, then
  the traffic.
- **Least privilege.** The setup creates a service account whose Role covers only what the
  boxes need, namespace by namespace; the admin context is used only to add a box.
- **No load when nobody looks.** The page polls every second while it is open; all viewers share
  one snapshot, and with nobody watching switchyard does not call the cluster.

## How it works

Each **box** is one Deployment. Its live tag is whatever the Deployment runs; the history (the
tags that went live, the last runs and their logs) is a ConfigMap `switchyard-<deployment>` next
to it, so a reinstalled switchyard finds it again.

| Button | What happens |
|---|---|
| **Canary** | The pending-SQL check and the migrate command in Jobs of the new image. Then `<deployment>-canary` with 1 pod of the new tag next to the live ones, watched for the pause; then as many pods as the live ones (about half the traffic), watched again. Then the Deployment gets the tag (a normal rolling update), the followers too, the copy goes, and the cache command runs. A failure, or **Abort**, deletes the copy; nothing else changed. |
| **Deploy** | The same migrations, then the tag on the Deployment and the followers at once, then the cache command. |
| **Roll back** | A kept tag on the Deployment and the followers at once, then the cache command. The database is not rolled back. |

**Followers** are CronJobs and Deployments that run the same image (a scheduler, a worker):
they get the tag at every switch. If a rollout does not finish within 10 minutes, the
Deployment goes back to the tag it had.

## Install

switchyard runs as a user service on any always-on Linux machine with `kubectl` and an admin
kubeconfig for the cluster, inside or outside it.

```bash
curl -fsSLO https://github.com/xuedi/switchyard/releases/download/v0.6.0/switchyard-0.6.0-linux-amd64
install -m 0755 switchyard-0.6.0-linux-amd64 ~/.local/bin/switchyard   # or -arm64, -armv7
switchyard setup                                                        # http://127.0.0.1:8080
```

The installer checks `kubectl` and the kubeconfig, tests the admin context, asks for a user, a
password and the address to listen on, and then:

- creates the namespace `switchyard` with a service account and its token,
- writes `~/.config/switchyard/kubeconfig` with only that token, and `config.json` (both 0600),
- writes `~/.config/systemd/user/switchyard.service`.

Then stop the installer and start the service for good:

```bash
systemctl --user daemon-reload
systemctl --user enable --now switchyard
loginctl enable-linger $USER   # keep it running without a login
```

switchyard speaks plain HTTP: keep it on 127.0.0.1 behind a reverse proxy with HTTPS. Every
link is relative, so any path prefix works. A login with a signed session cookie guards every
page; clicks are protected against cross-site requests.

## Adding a box

**Add a box**, pick the namespace and the Deployment. That is all that is required; under
**More**:

| Field | Meaning |
|---|---|
| Container | which one, when the pod has several |
| Followers | CronJobs and Deployments in the namespace that run the same image |
| Pending-SQL command | prints the SQL of the pending migrations, nothing when none; run in the new image before anything moves |
| Migrate command | runs the migrations; in the new image, after the check, before any traffic |
| Cache command | runs after every switch, forward and back |
| Site | e.g. `https://shop.example.org`: its host goes as `Host`, its scheme as `X-Forwarded-Proto` with every probe, and it is linked |
| Pages | the paths requested from a new and an old pod during each canary step |
| Links | more links in the header, `label=url ...` |
| Seconds per canary step | 120 if empty |
| Tags kept for rollback | 5 if empty |

The commands run in a Job made from the Deployment's pod spec: the same environment, volumes
and security context, the new image, the probes and ports left out. A command is split at
spaces, no shell.

Adding, changing or removing a box writes switchyard's Role in that namespace, so it needs the
admin context once. If the namespace has no pull secret for the image's registry, the form
takes a token: it is stored as the Secret `switchyard-pull` and added to the pods' service
account.

## The image contract

What your app's build has to deliver for switchyard:

- **Immutable tags**, one per release: a tag is read once and never again. Moving tags such as
  `latest` are ignored.
- The label **`org.opencontainers.image.created`** (or the image's creation time): tags are
  ordered by it, since their names need not sort.
- Optional, shown on the page: `org.opencontainers.image.version` and `revision`,
  `switchyard.subject` (e.g. the commit subject), `switchyard.meta.<key>` (e.g. one commit per
  repository).
- If the app has migrations: a pending-SQL command (stdout the SQL, logs on stderr, a line
  `-- Version <name>` or `-- Migration <name>` before each migration's statements), a migrate
  command, and `sh` plus a writable `/tmp` in the image for the check's wrapper.
- Readiness probes that mean it: the canary waits for ready pods.

Registries: ghcr.io, Docker Hub and any registry with the OCI distribution API and bearer or
basic auth.

## Migrations

During a canary the old code runs on the migrated database, and a rollback goes back to such
code later. So a migration may only add: new tables, nullable columns or ones with a default,
indexes, foreign keys, backfills. switchyard blocks:

- `DROP TABLE`, `TRUNCATE`, `RENAME TABLE`, `DELETE FROM`
- `ALTER TABLE ... DROP` a column, `RENAME`, `CHANGE`, `MODIFY`, `ALTER COLUMN ... TYPE` or
  `SET NOT NULL`
- `ADD` a column `NOT NULL` without a default

`CHANGE` and `MODIFY` are blocked even when they only change a comment: without the live schema
a comment change cannot be told from a type change. Split a destructive change: add the new
thing, deploy, move the code over, deploy, then drop the old thing with **Deploy anyway** once
no kept tag needs it. The tags table marks a tag that dropped something, and Roll back warns
when it would cross one.

## Development

```bash
just check     # gofmt, vet, the builds for amd64, arm64 and armv7
just run       # the installer or the boxes on 127.0.0.1:8080
just release   # the release binaries in dist/
```

Standard library only. `XDG_CONFIG_HOME=/tmp/sy just run` keeps a test setup apart from your
own.

## License

[EUPL v1.2](LICENSE)
