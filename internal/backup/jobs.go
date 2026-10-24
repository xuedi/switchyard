package backup

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/xuedi/switchyard/internal/config"
)

// The Jobs a backup set runs, as pod specs: init containers in order, all on the target PVC.
// A step never fails its container: it marks itself in /sy/failed and the next one runs, so the
// after-hook always gets its turn; the last container fails the Job when anything is marked.

const (
	indexStart = "SWITCHYARD-INDEX"
	indexEnd   = "SWITCHYARD-END"
	runLine    = "SWITCHYARD-RUN"
)

// ItemName: what an item may be called, it goes into file names and scripts.
var ItemName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// DBName: a database or user name switchyard writes into SQL.
var DBName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var notLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// cname makes a step's name a container name: an item may have . and _ in its name.
func cname(s string) string {
	s = notLabel.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s[:min(len(s), 63)], "-")
}

// cluster is what a Job needs from the cluster, read when it is made.
type cluster struct {
	images map[string]string // item name -> its database's image
	tools  string
	s3     string // the buckets' image
	hooks  map[string]*hookSpec
	live   map[string]string // workload -> image, for the manifest
}

// hookSpec is a hook's container with what it needs from its workload's pod spec.
type hookSpec struct {
	container map[string]any
	volumes   []any
	args      []any // the command, split at spaces
	pull      []any
	account   string
}

type step struct {
	name   string
	image  string
	script string
	env    []any
	mounts []any
	tries  int // a hook's
}

func secretEnv(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
}

func mount(name, path string, ro bool) map[string]any {
	m := map[string]any{"name": name, "mountPath": path}
	if ro {
		m["readOnly"] = true
	}
	return m
}

func itemDir(n int, it config.Item) string {
	d := fmt.Sprintf("/v/%d", n)
	if it.SubPath != "" {
		d += "/" + strings.Trim(it.SubPath, "/")
	}
	return d
}

// pod builds the pod spec: the steps as init containers, the last one as the container.
func pod(b config.Backup, c cluster, steps []step, items []config.Item, rw bool) map[string]any {
	volumes := []any{
		map[string]any{"name": "sy-target", "persistentVolumeClaim": map[string]any{"claimName": b.Target}},
		map[string]any{"name": "sy-run", "emptyDir": map[string]any{}},
		map[string]any{"name": "sy-work", "emptyDir": map[string]any{}},
	}
	base := []any{mount("sy-target", "/target", false), mount("sy-run", "/sy", false), mount("sy-work", "/work", false)}
	var itemMounts []any
	for n, it := range items {
		if it.Kind != "volume" {
			continue
		}
		name := fmt.Sprintf("sy-v%d", n)
		volumes = append(volumes, map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": it.PVC, "readOnly": !rw}})
		itemMounts = append(itemMounts, mount(name, fmt.Sprintf("/v/%d", n), !rw))
	}
	tz := b.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	var containers []any
	for _, s := range steps {
		if h := c.hooks[s.name]; h != nil && s.image == "" {
			hc := maps.Clone(h.container)
			hc["command"] = append([]any{"sh", "-c", hookScript(s.name, s.tries), "hook"}, h.args...)
			hc["volumeMounts"] = append(append([]any{}, h.container["volumeMounts"].([]any)...), mount("sy-run", "/sy", false))
			containers = append(containers, hc)
			continue
		}
		mounts := append(append([]any{}, base...), itemMounts...)
		containers = append(containers, map[string]any{
			"name": cname(s.name), "image": s.image, "command": []any{"sh", "-c", s.script},
			"env":             append([]any{map[string]any{"name": "TZ", "value": tz}}, s.env...),
			"volumeMounts":    mounts,
			"securityContext": map[string]any{"allowPrivilegeEscalation": false},
		})
	}
	spec := map[string]any{
		"restartPolicy":  "Never",
		"initContainers": containers[:len(containers)-1],
		"containers":     containers[len(containers)-1:],
		"volumes":        volumes,
	}
	var pull []any
	for _, name := range []string{"before", "after"} {
		h := c.hooks[name]
		if h == nil {
			continue
		}
		spec["volumes"] = append(spec["volumes"].([]any), h.volumes...)
		pull = append(pull, h.pull...)
		if h.account != "" {
			spec["serviceAccountName"] = h.account
		}
	}
	if len(pull) > 0 {
		spec["imagePullSecrets"] = pull
	}
	return spec
}

// hookStep runs the hook's command, tries times at most; its last failure marks it, the run goes
// on.
func hookStep(name string, tries int) step { return step{name: name, tries: tries} }

// hookScript tries the hook every 10 s: its first connection can be refused like a dump's, and
// an after-hook that gives up early can leave the app in maintenance mode.
func hookScript(name string, tries int) string {
	return `i=1; until "$@"; do if [ $i -ge ` + fmt.Sprint(tries) + ` ]; then echo "` + name + ` hook failed"; touch /sy/failed/` + name + `-hook; exit; fi
  echo "` + name + ` hook failed, again in 10 s"; i=$((i+1)); sleep 10; done`
}

// newHook makes the hook's container from its workload's raw pod spec, with its own volume names.
func newHook(name string, h *config.Hook, podSpec map[string]any) (*hookSpec, error) {
	list, _ := podSpec["containers"].([]any)
	if len(list) == 0 {
		return nil, fmt.Errorf("%s has no container", h.Workload)
	}
	c, _ := list[0].(map[string]any)
	prefix := "h" + name[:1] + "-"
	var mounts []any
	if ms, ok := c["volumeMounts"].([]any); ok {
		for _, x := range ms {
			if m, ok := x.(map[string]any); ok {
				m["name"] = prefix + fmt.Sprint(m["name"])
				mounts = append(mounts, m)
			}
		}
	}
	var volumes []any
	if vs, ok := podSpec["volumes"].([]any); ok {
		for _, x := range vs {
			if v, ok := x.(map[string]any); ok {
				v["name"] = prefix + fmt.Sprint(v["name"])
				volumes = append(volumes, v)
			}
		}
	}
	sc, _ := c["securityContext"].(map[string]any)
	if sc == nil {
		sc = map[string]any{}
	}
	if psc, ok := podSpec["securityContext"].(map[string]any); ok {
		for _, k := range []string{"runAsUser", "runAsGroup", "runAsNonRoot", "seccompProfile"} {
			if _, set := sc[k]; !set && psc[k] != nil {
				sc[k] = psc[k]
			}
		}
	}
	var args []any
	for _, a := range strings.Fields(h.Command) {
		args = append(args, a)
	}
	hc := map[string]any{"name": name + "-hook", "image": c["image"],
		"volumeMounts": append([]any{}, mounts...), "securityContext": sc}
	for _, k := range []string{"env", "envFrom", "workingDir", "resources"} {
		if c[k] != nil {
			hc[k] = c[k]
		}
	}
	pull, _ := podSpec["imagePullSecrets"].([]any)
	account, _ := podSpec["serviceAccountName"].(string)
	return &hookSpec{container: hc, args: args, volumes: volumes, pull: pull, account: account}, nil
}

// startScript: /sy/failed open to all, a hook runs as its workload's user.
const startScript = `mkdir -p /sy/failed; chmod 1777 /sy/failed; date +%s > /sy/start
while :; do TS=$(date +%Y-%m-%d_%H-%M); ls /target/"$TS"_* >/dev/null 2>&1 || break; echo "$TS is taken, waiting"; sleep 10; done
echo "$TS" > /sy/ts; echo "run $TS"`

// waitDB waits for the database at $SVC, as user, every 10 s, tries times at most: a new pod's
// first connection can be refused while the cluster's firewall catches up with it, and a
// scheduled run may catch up right after a boot. It answers at once when the database does.
func waitDB(it config.Item, user, db string, tries int) string {
	ready := `command -v pg_isready >/dev/null || return 0; pg_isready -q -h "$SVC" -U ` + sq(user) + ` -d ` + sq(db)
	if it.Kind == "mariadb" {
		ready = `a=$(command -v mariadb-admin || command -v mysqladmin) || return 0; "$a" ping --silent -h "$SVC" -u ` + sq(user) + ` >/dev/null 2>&1`
	}
	return `ready() { ` + ready + `; }
i=0; until ready || [ $i -ge ` + fmt.Sprint(tries) + ` ]; do [ $i = 0 ] && echo "` + it.Name + `: waiting for $SVC"; i=$((i+1)); sleep 10; done
`
}

// dumpStep dumps a database in its own image, through zstd into the target, or into /work when
// the image has no zstd (the last step compresses it), after waiting for it: 10 minutes for a
// scheduled run, 2 otherwise.
func dumpStep(it config.Item, image string, scheduled bool) step {
	n, f := it.Name, `/target/"$TS"_`+it.Name+`.sql.zst`
	fail := `touch /sy/failed/` + n
	var dump string
	env := []any{map[string]any{"name": "SVC", "value": it.Service}, map[string]any{"name": "DB", "value": it.Database},
		map[string]any{"name": "DBUSER", "value": it.User}}
	switch it.Kind {
	case "mariadb":
		dump = `"$(command -v mariadb-dump || command -v mysqldump)" --single-transaction -h "$SVC" -u "$DBUSER" "$DB"`
		env = append(env, secretEnv("MYSQL_PWD", it.Secret, it.PasswordKey))
	default:
		dump = `pg_dump -h "$SVC" -U "$DBUSER" -d "$DB"`
		env = append(env, secretEnv("PGPASSWORD", it.Secret, it.PasswordKey))
	}
	tries := 12
	if scheduled {
		tries = 60
	}
	script := `TS=$(cat /sy/ts)
` + waitDB(it, it.User, it.Database, tries) + `if command -v zstd >/dev/null; then
  { ` + dump + ` || ` + fail + `; } | zstd -q > ` + f + `.part || ` + fail + `
  if [ -e /sy/failed/` + n + ` ]; then rm -f ` + f + `.part; echo "` + n + ` failed"; else mv ` + f + `.part ` + f + `; echo "` + n + ` $(wc -c < ` + f + `) bytes"; fi
else
  ` + dump + ` > /work/` + n + `.sql && echo "` + n + ` dumped, compressed at the end" || { ` + fail + `; echo "` + n + ` failed"; }
fi`
	return step{name: "dump-" + n, image: image, script: script, env: env}
}

func volumeStep(n int, it config.Item, tools string) step {
	f := `/target/"$TS"_` + it.Name + `.tar.zst`
	fail := `touch /sy/failed/` + it.Name
	excl := ""
	for _, e := range it.Exclude {
		excl += " --exclude=" + sq(e)
	}
	// tar's 1 is "a file changed while it was read": fine for a hot backup
	script := `TS=$(cat /sy/ts)
cd ` + sq(itemDir(n, it)) + ` || { ` + fail + `; exit 0; }
{ tar -cpf -` + excl + ` .; [ $? -le 1 ] || ` + fail + `; } | zstd -q > ` + f + `.part || ` + fail + `
if [ -e /sy/failed/` + it.Name + ` ]; then rm -f ` + f + `.part; echo "` + it.Name + ` failed"; else mv ` + f + `.part ` + f + `; echo "` + it.Name + ` $(wc -c < ` + f + `) bytes"; fi`
	return step{name: "tar-" + it.Name, image: tools, script: script}
}

const indexScript = `cd /target; echo ` + indexStart + `
for f in *; do [ -f "$f" ] && printf 'F\t%s\t%s\n' "$f" "$(wc -c < "$f")"; done
for f in *_manifest.json; do [ -f "$f" ] && printf 'M\t%s\t%s\n' "$f" "$(tr -d '\n\t' < "$f")"; done
echo ` + indexEnd

// pruneScript keeps every run of the newest daily dates, and of the older ones the earliest run
// of each month for the newest monthly months; the rest goes, run by run. Only runs with a
// manifest count: archives from before switchyard, and a run cut off halfway, are never pruned
// (the user deletes those by hand).
const pruneScript = `ls -1 | sed -n 's/^\([0-9]\{4\}-[0-9][0-9]-[0-9][0-9]_[0-9][0-9]-[0-9][0-9]\)_manifest\.json$/\1/p' | sort -u > /sy/runs
while read ts; do echo "$ts 1"; done < /sy/runs | awk -v daily="$KEEP_DAILY" -v monthly="$KEEP_MONTHLY" '
{ ts[NR] = $1; full[NR] = $2; d = substr($1, 1, 10); if (!(d in seen)) { seen[d] = 1; dates[++nd] = d } }
END {
  for (i = nd; i > nd - daily && i > 0; i--) recent[dates[i]] = 1
  for (i = 1; i <= NR; i++) {
    if (substr(ts[i], 1, 10) in recent) continue
    m = substr(ts[i], 1, 7)
    if (!(m in known)) { known[m] = 1; months[++nm] = m }
    if (full[i] && !(m in mfull)) { mfull[m] = 1; keep[m] = ts[i] } else if (!(m in keep)) keep[m] = ts[i]
  }
  for (i = nm; i > nm - monthly && i > 0; i--) kept[months[i]] = 1
  for (i = 1; i <= NR; i++) {
    if (substr(ts[i], 1, 10) in recent) continue
    m = substr(ts[i], 1, 7)
    if (!(m in kept) || keep[m] != ts[i]) print ts[i]
  }
}' | while read ts; do echo "pruned $ts"; rm -f -- "$ts"_*; done`

// lastStep compresses what waits in /work, then, if nothing failed, writes the manifest and
// prunes, else removes the run's own files (nothing would ever prune them); prints the index
// either way, then the run's line.
func lastStep(b config.Backup, c cluster, kind string) step {
	var names []string
	for _, it := range b.Items {
		names = append(names, it.Name)
	}
	live := "null"
	if len(c.live) > 0 && kind != "scheduled" {
		j, _ := json.Marshal(c.live)
		live = string(j)
	}
	script := `TS=$(cat /sy/ts); cd /target
for f in /work/*.sql; do [ -e "$f" ] || continue; n=$(basename "$f" .sql); zstd -q < "$f" > "$TS"_"$n".sql.zst && echo "$n compressed" || touch /sy/failed/"$n"; done
touch .switchyard
if [ -z "$(ls /sy/failed)" ]; then
  { printf '{"run":"%s","kind":"%s","started":%s,"finished":%s,"items":[' "$TS" ` + sq(kind) + ` "$(cat /sy/start)" "$(date +%s)"
    sep=""; for n in $ITEMS; do f=$(ls "$TS"_"$n".* | head -1)
      printf '%s{"name":"%s","file":"%s","size":%s,"sha256":"%s"}' "$sep" "$n" "$f" "$(wc -c < "$f")" "$(sha256sum "$f" | cut -d' ' -f1)"; sep=","; done
    printf '],"live":%s}\n' ` + sq(live) + `
  } > "$TS"_manifest.json.part && mv "$TS"_manifest.json.part "$TS"_manifest.json
  ` + pruneScript + `
  ok=ok
else
  ok=failed; echo "failed: $(ls /sy/failed | tr '\n' ' ')"
  rm -f -- "$TS"_* && echo "removed the files of $TS: a failed run is no archive"
fi
` + indexScript + `
echo ` + runLine + ` "$TS" $ok
[ $ok = ok ]`
	env := []any{
		map[string]any{"name": "ITEMS", "value": strings.Join(names, " ")},
		map[string]any{"name": "KEEP_DAILY", "value": fmt.Sprint(b.KeepDailyOr())},
		map[string]any{"name": "KEEP_MONTHLY", "value": fmt.Sprint(b.KeepMonthlyOr())},
	}
	return step{name: "finish", image: c.tools, script: script, env: env}
}

// s3Env is rclone's remote sy: the item's store, the key from its Secret.
func s3Env(it config.Item) []any {
	env := []any{map[string]any{"name": "BUCKET", "value": it.Bucket}}
	for _, kv := range [][2]string{{"TYPE", "s3"}, {"PROVIDER", "Other"}, {"ENDPOINT", it.Endpoint}, {"REGION", it.Region},
		{"FORCE_PATH_STYLE", fmt.Sprint(!it.VirtualHost)}, {"NO_CHECK_BUCKET", "true"}} {
		if kv[1] != "" {
			env = append(env, map[string]any{"name": "RCLONE_CONFIG_SY_" + kv[0], "value": kv[1]})
		}
	}
	return append(env, secretEnv("RCLONE_CONFIG_SY_ACCESS_KEY_ID", it.Secret, it.AccessKeyKey),
		secretEnv("RCLONE_CONFIG_SY_SECRET_ACCESS_KEY", it.Secret, it.SecretKeyKey))
}

func s3Dir(it config.Item) string { return sq("/work/s3/" + it.Name) }

// bucketSteps copies the bucket into /work (the S3 image), then tars the copy into the target
// (the tools image): the object keys are the paths in the tar. wait: the copy is tried for 10
// minutes.
func bucketSteps(it config.Item, c cluster, scheduled bool) []step {
	d, f := s3Dir(it), `/target/"$TS"_`+it.Name+`.tar.zst`
	fail := `touch /sy/failed/` + it.Name
	tries := 6
	if scheduled {
		tries = 30
	}
	copyScript := `mkdir -p ` + d + `; i=0; until rclone copy -q --retries 1 sy:"$BUCKET" ` + d + `; do i=$((i+1))
  [ $i -ge ` + fmt.Sprint(tries) + ` ] && { ` + fail + `; echo "` + it.Name + `: the copy of $BUCKET failed"; break; }
  echo "` + it.Name + `: the copy of $BUCKET failed, again in 20 s"; sleep 20; done`
	tarScript := `TS=$(cat /sy/ts)
if [ -e /sy/failed/` + it.Name + ` ]; then rm -rf ` + d + `; echo "` + it.Name + ` failed"; exit 0; fi
cd ` + d + ` && n=$(find . -type f | wc -l) && { tar -cpf - . || ` + fail + `; } | zstd -q > ` + f + `.part || ` + fail + `
cd /; rm -rf ` + d + `
if [ -e /sy/failed/` + it.Name + ` ]; then rm -f ` + f + `.part; echo "` + it.Name + ` failed"; else mv ` + f + `.part ` + f + `; echo "` + it.Name + ` $n objects, $(wc -c < ` + f + `) bytes"; fi`
	return []step{{name: "s3-" + it.Name, image: c.s3, script: copyScript, env: s3Env(it)},
		{name: "tar-" + it.Name, image: c.tools, script: tarScript}}
}

// runPod is a backup run: kind is manual, scheduled, pre-restore or pre-deploy.
func runPod(b config.Backup, c cluster, kind string) map[string]any {
	tries := 12
	if kind == "scheduled" {
		tries = 60
	}
	steps := []step{{name: "start", image: c.tools, script: startScript}}
	if b.Before != nil {
		steps = append(steps, hookStep("before", tries))
	}
	for n, it := range b.Items {
		switch it.Kind {
		case "volume":
			steps = append(steps, volumeStep(n, it, c.tools))
		case "s3":
			steps = append(steps, bucketSteps(it, c, kind == "scheduled")...)
		default:
			steps = append(steps, dumpStep(it, c.images[it.Name], kind == "scheduled"))
		}
	}
	if b.After != nil {
		steps = append(steps, hookStep("after", tries))
	}
	steps = append(steps, lastStep(b, c, kind))
	return pod(b, c, steps, b.Items, false)
}

// indexPod only lists the target.
func indexPod(b config.Backup, c cluster) map[string]any {
	return pod(b, c, []step{{name: "index", image: c.tools, script: indexScript}}, nil, false)
}

// Pick is one item of a restore: the archive, and for an import the directory inside a tar.
type Pick struct {
	Item string `json:"item"`
	File string `json:"file"`
	Path string `json:"path,omitempty"` // in the tar, e.g. app/data/images; "" is all of it
}

// restorePod restores the picked items: the dumps unpacked into /work, each database dropped,
// created and imported as its restore user, each volume emptied and extracted, each bucket
// extracted into /work and synced (what the archive does not have is deleted). A failure stops
// the rest; the after-hook runs anyway.
func restorePod(b config.Backup, c cluster, picks []Pick, imp bool) map[string]any {
	byName := map[string]int{}
	for n, it := range b.Items {
		byName[it.Name] = n
	}
	skip := `[ -z "$(ls /sy/failed)" ] || exit 0` + "\n"
	prep := "mkdir -p /sy/failed; chmod 1777 /sy/failed\n"
	var steps []step
	for _, p := range picks {
		n := byName[p.Item]
		it := b.Items[n]
		f := sq("/target/" + p.File)
		fail := `{ touch /sy/failed/` + it.Name + `; echo "` + it.Name + ` failed"; }`
		// a directory of the tar: its members start with ./ when switchyard wrote it
		tarArgs, pre := "", ""
		if p.Path != "" {
			path := strings.Trim(p.Path, "/")
			pre = fmt.Sprintf(`p=%s; s=%d; zstd -dc %s | tar -tf - | head -1 | grep -q '^\./' && { p=./$p; s=$((s+1)); }`+"\n",
				sq(path), strings.Count(path, "/")+1, f)
			tarArgs = ` --strip-components="$s" "$p"`
		}
		if it.Kind == "s3" {
			d := s3Dir(it)
			steps = append(steps, step{name: "untar-" + it.Name, image: c.tools,
				script: skip + pre + `mkdir -p ` + d + ` && zstd -dc ` + f + ` | tar -C ` + d + ` -xf -` + tarArgs + ` || ` + fail},
				step{name: "restore-" + it.Name, image: c.s3, env: s3Env(it),
					script: skip + `rclone sync -q --retries 1 ` + d + ` sy:"$BUCKET" && echo "` + it.Name + ` restored, $(find ` + d + ` -type f | wc -l) objects in $BUCKET" || ` + fail})
			continue
		}
		if it.Dump() {
			prep += `zstd -dc ` + f + ` > /work/` + it.Name + `.sql && echo "` + it.Name + ` unpacked" || touch /sy/failed/` + it.Name + "\n"
			env := []any{map[string]any{"name": "SVC", "value": it.Service}}
			var script string
			if it.Kind == "mariadb" {
				env = append(env, secretEnv("MYSQL_PWD", it.Secret, it.RootKey))
				script = skip + waitDB(it, it.RootUserOr(), it.Database, 12) + `C=$(command -v mariadb || command -v mysql)
"$C" -h "$SVC" -u ` + sq(it.RootUserOr()) + ` -e 'DROP DATABASE IF EXISTS ` + "`" + it.Database + "`" + `; CREATE DATABASE ` + "`" + it.Database + "`" + `;' &&
"$C" -h "$SVC" -u ` + sq(it.RootUserOr()) + ` ` + sq(it.Database) + ` < /work/` + it.Name + `.sql && echo "` + it.Name + ` restored" || ` + fail
			} else {
				env = append(env, secretEnv("PGPASSWORD", it.Secret, it.RootKey))
				psql := `psql -v ON_ERROR_STOP=1 -q -o /dev/null -h "$SVC" -U ` + sq(it.RootUserOr())
				script = skip + waitDB(it, it.RootUserOr(), "postgres", 12) + psql + ` -d postgres -c 'DROP DATABASE IF EXISTS "` + it.Database + `" WITH (FORCE)' -c 'CREATE DATABASE "` + it.Database + `" OWNER "` + it.User + `"' &&
` + psql + ` -d ` + sq(it.Database) + ` -f /work/` + it.Name + `.sql && echo "` + it.Name + ` restored" || ` + fail
			}
			steps = append(steps, step{name: "restore-" + it.Name, image: c.images[it.Name], script: script, env: env})
			continue
		}
		d := sq(itemDir(n, it))
		// a top-level exclude is not in the archive: it keeps what it has
		keep := ""
		for _, e := range it.Exclude {
			if e = strings.TrimPrefix(e, "./"); e != "" && !strings.ContainsAny(e, "/*?[") {
				keep += ` ! -name ` + sq(e)
			}
		}
		script := skip + pre + `mkdir -p ` + d + ` && own=$(stat -c %u:%g ` + d + `) &&
find ` + d + ` -mindepth 1 -maxdepth 1` + keep + ` -exec rm -rf {} + && zstd -dc ` + f + ` | tar -C ` + d + ` -xpf -` + tarArgs + ` || ` + fail
		if imp {
			script += "\n" + `[ -e /sy/failed/` + it.Name + ` ] || chown -R "$own" ` + d
		}
		script += "\n" + `[ -e /sy/failed/` + it.Name + ` ] || echo "` + it.Name + ` restored"`
		steps = append(steps, step{name: "restore-" + it.Name, image: c.tools, script: script})
	}
	steps = append([]step{{name: "unpack", image: c.tools, script: prep}}, steps...)
	if b.After != nil {
		steps = append(steps, hookStep("after", 12))
	}
	steps = append(steps, step{name: "finish", image: c.tools, script: `if [ -n "$(ls /sy/failed)" ]; then ok=failed; echo "failed: $(ls /sy/failed | tr '\n' ' ')"; else ok=ok; fi
` + indexScript + `
echo ` + runLine + ` restore $ok
[ $ok = ok ]`})
	return pod(b, c, steps, b.Items, true)
}
