# Example: a Symfony app with Doctrine

A shop served by FrankenPHP (or any PHP server) from the releases volume.

| File | |
|---|---|
| `app.env` | switchyard's config for it |
| `hooks/prepare` | fetches the latest `main` into the release, reports the commit |
| `hooks/pending-sql` | Doctrine's dry run: the SQL of the pending migrations, for the guard |
| `hooks/build` | composer, assets, migrations, cache warm-up |
| `hooks/after-rollback` | clears the app cache |
| `slots.yaml` | the app's own part: two slot Deployments and Services, a scheduler that follows the live release |

```bash
switchyard manifests --config app.env | kubectl apply -f -
kubectl apply -f slots.yaml
kubectl -n shop create job --from=cronjob/quick quick-$(date +%s)
```

`BUILD_IMAGE` is the app's image with PHP, composer and git; `IMAGE` only runs `switchyard.sh`
(bash, kubectl, curl, awk). The Caddyfile needs the block `# BEGIN shop` ... `# END shop` in
the shop's `reverse_proxy`, see [docs/routers.md](../../docs/routers.md).
