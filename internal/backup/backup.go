// Package backup saves and restores a namespace's databases and volumes: dated archives on a
// PVC, made by Jobs (a CronJob for the schedule), listed from the last Job's log, with the
// databases' own dump tools. One run per namespace at a time; the state lives in the ConfigMap
// switchyard-backup next to the app.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/kube"
)

const (
	StateName = "switchyard-backup" // the ConfigMap, and the CronJob
	kindLabel = "switchyard/backup" // on every Job of a set: manual, scheduled, pre-restore, restore, index
	keepRuns  = 15
	logLines  = 300
)

// Run is one backup, restore or listing.
type Run struct {
	Kind      string // manual, scheduled, pre-restore, restore, index
	Status    string // running, ok, failed
	Message   string
	Started   string
	Finished  string
	TS        string // the archive's timestamp: its files' prefix
	By        string
	Job       string
	Log       []string
	Scaled    map[string]int `json:",omitempty"` // scaled to 0 by this run, back after a restart too
	Suspended []string       `json:",omitempty"` // CronJobs it suspended
}

type File struct {
	Name string
	Size int64
}

// State is the ConfigMap's: the runs, the target as the last Job listed it.
type State struct {
	Runs      []Run                      `json:"runs"` // newest first
	Files     []File                     `json:"files"`
	Manifests map[string]json.RawMessage `json:"manifests"`
	IndexAt   string                     `json:"indexAt"`
	Seen      []string                   `json:"seen"` // the CronJob's Jobs already read
}

// Refused is a click that cannot be done now; nothing was started.
type Refused string

func (r Refused) Error() string { return string(r) }

// Engine holds a Runner per backup set.
type Engine struct {
	K       kube.Kube
	BoxBusy func(ns string) string // a box of the namespace is running: what
	mu      sync.Mutex
	r       map[string]*Runner
}

func NewEngine(k kube.Kube, boxBusy func(string) string) *Engine {
	return &Engine{K: k, BoxBusy: boxBusy, r: map[string]*Runner{}}
}

func (e *Engine) Runner(b config.Backup) *Runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.r[b.Namespace]
	if r == nil {
		r = &Runner{k: e.K, e: e}
		e.r[b.Namespace] = r
	}
	r.mu.Lock()
	r.set = b
	r.mu.Unlock()
	return r
}

// Busy names what runs in the namespace's backup set, "" when nothing does.
func (e *Engine) Busy(ns string) string {
	e.mu.Lock()
	r := e.r[ns]
	e.mu.Unlock()
	if r == nil {
		return ""
	}
	if run := r.Current(); run != nil {
		return "a " + run.Kind + " backup run"
	}
	return ""
}

type Runner struct {
	k kube.Kube
	e *Engine

	mu       sync.Mutex
	set      config.Backup
	run      *Run
	syncedAt time.Time
	stateMu  sync.Mutex
}

func (r *Runner) Set() config.Backup {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set
}

// Current is a copy of the running run, nil when none runs.
func (r *Runner) Current() *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil {
		return nil
	}
	c := *r.run
	c.Log = slices.Clone(r.run.Log)
	return &c
}

func now() string { return time.Now().Format(time.RFC3339) }

func (r *Runner) itemNames() []string {
	var n []string
	for _, it := range r.Set().Items {
		n = append(n, it.Name)
	}
	return n
}

func (r *Runner) ns() string { return "--namespace=" + r.Set().Namespace }

func (r *Runner) logf(format string, a ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, a...)
	r.mu.Lock()
	if r.run != nil {
		r.run.Log = append(r.run.Log, line)
		if n := len(r.run.Log); n > logLines {
			r.run.Log = r.run.Log[n-logLines:]
		}
	}
	ns := r.set.Namespace
	r.mu.Unlock()
	log.Printf("%s backup: %s", ns, fmt.Sprintf(format, a...))
}

// --- state ---

func (r *Runner) ReadState(ctx context.Context) (State, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	st := State{}
	err := r.k.Get(ctx, &cm, r.ns(), "get", "--", "configmap", StateName)
	if kube.IsNotFound(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if s := cm.Data["state"]; s != "" {
		if err := json.Unmarshal([]byte(s), &st); err != nil {
			return st, fmt.Errorf("%s: %w", StateName, err)
		}
	}
	return st, nil
}

// update reads, changes and writes the state, with the set mirrored next to it.
func (r *Runner) update(ctx context.Context, f func(st *State)) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	st, err := r.ReadState(ctx)
	if err != nil {
		return err
	}
	f(&st)
	if len(st.Runs) > keepRuns {
		st.Runs = st.Runs[:keepRuns]
	}
	for i := range st.Runs {
		if n := len(st.Runs[i].Log); i >= 3 && n > 40 {
			st.Runs[i].Log = st.Runs[i].Log[n-40:]
		}
	}
	if len(st.Seen) > 30 {
		st.Seen = st.Seen[len(st.Seen)-30:]
	}
	set := r.Set()
	data, _ := json.Marshal(st)
	setJSON, _ := json.Marshal(set)
	return r.k.Apply(ctx, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": StateName, "namespace": set.Namespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "switchyard"}},
		"data": map[string]string{"state": string(data), "set": string(setJSON)},
	})
}

// save writes the running run into the state.
func (r *Runner) save(ctx context.Context) {
	run := r.Current()
	if run == nil {
		return
	}
	if err := r.update(ctx, func(st *State) { putRun(st, *run) }); err != nil {
		log.Printf("%s backup: state: %v", r.Set().Namespace, err)
	}
}

func putRun(st *State, run Run) {
	for i := range st.Runs {
		if st.Runs[i].Started == run.Started && st.Runs[i].Kind == run.Kind {
			st.Runs[i] = run
			return
		}
	}
	st.Runs = append([]Run{run}, st.Runs...)
	sort.SliceStable(st.Runs, func(i, j int) bool { return later(st.Runs[i].Started, st.Runs[j].Started) })
}

// Recover is called at the start: a run the state says is running died with the last
// switchyard. Its Job goes, what it scaled down comes back, and it is marked failed.
func (r *Runner) Recover(ctx context.Context) {
	st, err := r.ReadState(ctx)
	if err != nil {
		log.Printf("%s backup: recover: %v", r.Set().Namespace, err)
		return
	}
	for _, run := range st.Runs {
		if run.Status != "running" || run.Kind == "scheduled" {
			continue
		}
		log.Printf("%s backup: the %s started %s was cut off by a restart, marking it failed", r.Set().Namespace, run.Kind, run.Started)
		if run.Job != "" {
			r.k.Run(ctx, nil, r.ns(), "delete", "--ignore-not-found", "--wait=false", "--", "job", run.Job)
		}
		r.scaleUp(ctx, &run, false)
		run.Status, run.Finished = "failed", now()
		run.Message = "switchyard stopped during the " + run.Kind + "; its Job was removed and the app scaled back, check the data"
		r.update(ctx, func(st *State) { putRun(st, run) })
	}
}

// --- what a Job needs from the cluster ---

func (r *Runner) cluster(ctx context.Context) (cluster, error) {
	b := r.Set()
	c := cluster{images: map[string]string{}, hooks: map[string]*hookSpec{}, live: map[string]string{}}
	for _, it := range b.Items {
		if it.Kind == "volume" {
			continue
		}
		var sts struct {
			Spec struct {
				Template struct {
					Spec kube.PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := r.k.Get(ctx, &sts, r.ns(), "get", "--", "statefulset", it.StatefulSetOr()); err != nil {
			return c, fmt.Errorf("the database of %s: %w", it.Name, err)
		}
		ct, ok := sts.Spec.Template.Spec.Pick("")
		if !ok {
			return c, fmt.Errorf("statefulset %s has no container", it.StatefulSetOr())
		}
		c.images[it.Name] = ct.Image
		if c.tools == "" && it.Kind == "mariadb" {
			c.tools = ct.Image
		}
	}
	if b.Tools != "" {
		c.tools = b.Tools
	}
	if c.tools == "" {
		return c, Refused("the set needs a tools image (sh, tar, zstd, sha256sum): it has no MariaDB to borrow one from")
	}
	for name, h := range map[string]*config.Hook{"before": b.Before, "after": b.After} {
		if h == nil {
			continue
		}
		var w map[string]any
		if err := r.k.Get(ctx, &w, r.ns(), "get", "--", h.Workload); err != nil {
			return c, fmt.Errorf("the %s hook: %w", name, err)
		}
		spec, _ := w["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tpl["spec"].(map[string]any)
		hs, err := newHook(name, h, podSpec)
		if err != nil {
			return c, err
		}
		c.hooks[name] = hs
	}
	var list struct {
		Items []struct {
			Kind     string
			Metadata kube.Meta
			Spec     struct {
				Template struct {
					Spec kube.PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
	}
	if r.k.Get(ctx, &list, r.ns(), "get", "deployments,statefulsets") == nil {
		for _, w := range list.Items {
			if ct, ok := w.Spec.Template.Spec.Pick(""); ok && w.Metadata.Labels["switchyard/track"] == "" {
				c.live[strings.ToLower(w.Kind)+"/"+w.Metadata.Name] = ct.Image
			}
		}
	}
	return c, nil
}

// --- Jobs ---

func jobObject(ns, name, kind string, podSpec map[string]any) map[string]any {
	labels := map[string]string{"app.kubernetes.io/managed-by": "switchyard", kindLabel: kind}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 6 * 3600, "ttlSecondsAfterFinished": 7 * 86400,
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec}},
	}
}

// result is what a Job's log says: its lines without the index, the index, the run's line.
type result struct {
	lines []string
	files []File
	man   map[string]json.RawMessage
	ts    string
	ok    bool
}

// prefixed is a line of kubectl logs --prefix: [pod/<pod>/<container>] line.
var prefixed = regexp.MustCompile(`^\[pod/[^/]+/([^\]]+)\] ?(.*)$`)

// rank orders the containers' logs as the steps ran; --all-containers does not.
func rank(c string, items []string) int {
	switch {
	case c == "start" || c == "unpack":
		return 0
	case c == "before-hook":
		return 1
	case c == "after-hook":
		return 900
	case c == "finish" || c == "index":
		return 1000
	}
	_, name, _ := strings.Cut(c, "-")
	return 10 + slices.Index(items, name)
}

func parse(out string, items []string) result {
	res := result{man: map[string]json.RawMessage{}}
	var all []string
	byC := map[string][]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		m := prefixed.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if byC[m[1]] == nil {
			all = append(all, m[1])
		}
		byC[m[1]] = append(byC[m[1]], m[2])
	}
	slices.SortStableFunc(all, func(a, b string) int { return rank(a, items) - rank(b, items) })
	var lines []string
	for _, c := range all {
		lines = append(lines, byC[c]...)
	}
	in := false
	for _, l := range lines {
		switch {
		case l == indexStart:
			in, res.files = true, []File{}
		case l == indexEnd:
			in = false
		case in:
			f := strings.SplitN(l, "\t", 3)
			if len(f) == 3 && f[0] == "F" {
				var size int64
				fmt.Sscan(f[2], &size)
				res.files = append(res.files, File{Name: f[1], Size: size})
			} else if len(f) == 3 && f[0] == "M" && json.Valid([]byte(f[2])) {
				res.man[f[1]] = json.RawMessage(f[2])
			}
		case strings.HasPrefix(l, runLine+" "):
			f := strings.Fields(l)
			if len(f) == 3 {
				res.ts, res.ok = f[1], f[2] == "ok"
			}
		default:
			res.lines = append(res.lines, l)
		}
	}
	return res
}

// runJob creates a Job and waits for it, the log into the run as it goes; the index it printed
// goes into the state.
func (r *Runner) runJob(ctx context.Context, kind string, podSpec map[string]any) (result, error) {
	b := r.Set()
	name := fmt.Sprintf("switchyard-%s-%d", kind, time.Now().Unix())
	body, _ := json.Marshal(jobObject(b.Namespace, name, kind, podSpec))
	if _, err := r.k.Run(ctx, body, "create", "-f", "-"); err != nil {
		return result{}, err
	}
	r.mu.Lock()
	if r.run != nil {
		r.run.Job = name
	}
	r.mu.Unlock()
	r.logf("job %s started", name)
	r.save(ctx)
	head := r.Current().Log
	stuckSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			r.k.Run(context.Background(), nil, r.ns(), "delete", "--ignore-not-found", "--wait=false", "--", "job", name)
			return result{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var j kube.Job
		if err := r.k.Get(ctx, &j, r.ns(), "get", "--", "job", name); err != nil {
			continue
		}
		out, _ := r.k.Run(ctx, nil, r.ns(), "logs", "--all-containers", "--prefix", "--ignore-errors", "--tail=-1", "--", "job/"+name)
		res := parse(string(out), r.itemNames())
		r.mu.Lock()
		if r.run != nil {
			r.run.Log = append(slices.Clone(head), res.lines...)
			if n := len(r.run.Log); n > logLines {
				r.run.Log = r.run.Log[n-logLines:]
			}
		}
		r.mu.Unlock()
		if phase := j.Phase(); phase != "running" {
			if res.files != nil {
				r.update(ctx, func(st *State) { st.Files, st.Manifests, st.IndexAt = res.files, res.man, now() })
			}
			if phase == "failed" || !res.ok && kind != "index" {
				return res, fmt.Errorf("job %s failed", name)
			}
			return res, nil
		}
		var pods struct{ Items []kube.Pod }
		if r.k.Get(ctx, &pods, r.ns(), "get", "pods", "-l", "job-name="+name) == nil {
			waiting := ""
			for _, p := range pods.Items {
				if w := p.Waiting(); strings.Contains(w, "ImagePull") || strings.Contains(w, "ErrImage") || strings.Contains(w, "CreateContainerConfigError") {
					waiting = w
				}
			}
			switch {
			case waiting == "":
				stuckSince = time.Time{}
			case stuckSince.IsZero():
				stuckSince = time.Now()
			case time.Since(stuckSince) > 5*time.Minute:
				r.k.Run(context.Background(), nil, r.ns(), "delete", "--ignore-not-found", "--wait=false", "--", "job", name)
				return result{}, fmt.Errorf("job %s does not start: %s", name, waiting)
			}
		}
	}
}

// activeJob names a Job of the set that still runs (a scheduled one, most likely).
func (r *Runner) activeJob(ctx context.Context) (string, error) {
	var jobs struct{ Items []kube.Job }
	if err := r.k.Get(ctx, &jobs, r.ns(), "get", "jobs", "-l", kindLabel); err != nil {
		return "", err
	}
	for _, j := range jobs.Items {
		if j.Phase() == "running" {
			return j.Metadata.Name, nil
		}
	}
	return "", nil
}

// --- scaling the app down and back ---

// scaleDown scales the namespace's Deployments and the StatefulSets that are not the items'
// databases to 0, and with suspend its CronJobs; all remembered in the run.
func (r *Runner) scaleDown(ctx context.Context, suspend bool) error {
	b := r.Set()
	dbs := map[string]bool{}
	for _, it := range b.Items {
		if it.Kind != "volume" {
			dbs["statefulset/"+it.StatefulSetOr()] = true
		}
	}
	var list struct {
		Items []struct {
			Kind     string
			Metadata kube.Meta
			Spec     struct {
				Replicas *int `json:"replicas"`
				Suspend  bool `json:"suspend"`
			} `json:"spec"`
		}
	}
	what := "deployments,statefulsets"
	if suspend {
		what += ",cronjobs"
	}
	if err := r.k.Get(ctx, &list, r.ns(), "get", what); err != nil {
		return err
	}
	for _, w := range list.Items {
		obj := strings.ToLower(w.Kind) + "/" + w.Metadata.Name
		switch {
		case w.Kind == "CronJob":
			if w.Spec.Suspend {
				continue
			}
			if _, err := r.k.Run(ctx, nil, r.ns(), "patch", "--type=merge", "-p", `{"spec":{"suspend":true}}`, "--", obj); err != nil {
				return err
			}
			r.mu.Lock()
			r.run.Suspended = append(r.run.Suspended, obj)
			r.mu.Unlock()
			r.logf("%s suspended", obj)
			r.save(ctx)
		case dbs[obj] || w.Spec.Replicas == nil || *w.Spec.Replicas == 0:
		default:
			if _, err := r.k.Run(ctx, nil, r.ns(), "scale", "--replicas=0", "--", obj); err != nil {
				return err
			}
			r.mu.Lock()
			if r.run.Scaled == nil {
				r.run.Scaled = map[string]int{}
			}
			r.run.Scaled[obj] = *w.Spec.Replicas
			r.mu.Unlock()
			r.logf("%s down", obj)
			r.save(ctx)
		}
	}
	return nil
}

// scaleUp brings back what the run scaled down and resumes its CronJobs; wait: until ready.
func (r *Runner) scaleUp(ctx context.Context, run *Run, wait bool) {
	for obj, n := range run.Scaled {
		if _, err := r.k.Run(ctx, nil, r.ns(), "scale", fmt.Sprintf("--replicas=%d", n), "--", obj); err != nil {
			r.logf("could not scale %s back to %d: %v", obj, n, err)
		} else {
			r.logf("%s back to %d", obj, n)
		}
	}
	for _, obj := range run.Suspended {
		if _, err := r.k.Run(ctx, nil, r.ns(), "patch", "--type=merge", "-p", `{"spec":{"suspend":false}}`, "--", obj); err != nil {
			r.logf("could not resume %s: %v", obj, err)
		}
	}
	if !wait {
		return
	}
	for obj := range run.Scaled {
		if _, err := r.k.Run(ctx, nil, r.ns(), "rollout", "status", "--timeout=600s", "--request-timeout=0", "--", obj); err != nil {
			r.logf("%s not ready: %v", obj, err)
		}
	}
}

// --- starting ---

func (r *Runner) begin(kind, by string) error {
	b := r.Set()
	if busy := r.e.BoxBusy(b.Namespace); busy != "" {
		return Refused(busy + ": the backup waits for it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if job, err := r.activeJob(ctx); err != nil {
		return err
	} else if job != "" {
		return Refused("the Job " + job + " still runs")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run != nil {
		return Refused("a " + r.run.Kind + " is still running")
	}
	r.run = &Run{Kind: kind, Status: "running", Started: now(), By: by,
		Log: []string{time.Now().Format("15:04:05") + " " + kind + ", started by " + by}}
	return nil
}

// started saves the new run, so a restarted switchyard knows of it.
func (r *Runner) started() { r.save(context.Background()) }

func (r *Runner) finish(err error, msg string) {
	r.mu.Lock()
	r.run.Finished = now()
	if err != nil {
		r.run.Status, r.run.Message = "failed", err.Error()
	} else {
		r.run.Status, r.run.Message = "ok", msg
	}
	r.mu.Unlock()
	if err != nil {
		r.logf("%v", err)
	} else {
		r.logf("%s", msg)
	}
	r.save(context.Background())
	r.mu.Lock()
	r.run = nil
	r.mu.Unlock()
}

// Start runs a backup now; a cold set scales the app down for it.
func (r *Runner) Start(by string) error {
	if err := r.begin("manual", by); err != nil {
		return err
	}
	go func() {
		ctx := context.Background()
		r.started()
		ts, err := r.backup(ctx, "manual", r.Set().Cold)
		r.finish(err, "backup "+ts+" written")
	}()
	return nil
}

// Refresh lists the target again.
func (r *Runner) Refresh(by string) error {
	if err := r.begin("index", by); err != nil {
		return err
	}
	go func() {
		ctx := context.Background()
		c, err := r.cluster(ctx)
		if err == nil {
			_, err = r.runJob(ctx, "index", indexPod(r.Set(), c))
		}
		r.finish(err, "listed")
	}()
	return nil
}

// backup runs one backup Job, cold or hot, and returns its timestamp.
func (r *Runner) backup(ctx context.Context, kind string, cold bool) (string, error) {
	c, err := r.cluster(ctx)
	if err != nil {
		return "", err
	}
	if cold {
		err := r.scaleDown(ctx, false)
		defer func() {
			run := r.Current()
			r.scaleUp(ctx, run, true)
			r.mu.Lock()
			r.run.Scaled = nil
			r.mu.Unlock()
		}()
		if err != nil {
			return "", err
		}
	}
	res, err := r.runJob(ctx, kind, runPod(r.Set(), c, kind))
	r.mu.Lock()
	r.run.TS = res.ts
	r.mu.Unlock()
	return res.ts, err
}

// Restore puts a run's items back (or, imp, foreign archives picked per item), after a safety
// backup of the same set unless noSafety.
func (r *Runner) Restore(ts string, picks []Pick, imp, noSafety bool, by string) error {
	b := r.Set()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := r.ReadState(ctx)
	if err != nil {
		return err
	}
	files := map[string]bool{}
	for _, f := range st.Files {
		files[f.Name] = true
	}
	if len(picks) == 0 {
		return Refused("pick at least one item")
	}
	seen := map[string]bool{}
	for i, p := range picks {
		n := slices.IndexFunc(b.Items, func(it config.Item) bool { return it.Name == p.Item })
		if n < 0 || seen[p.Item] {
			return Refused("no item " + p.Item + " in the set, or twice")
		}
		seen[p.Item] = true
		if !imp {
			picks[i].File, picks[i].Path = ts+"_"+p.Item+b.Items[n].Ext(), ""
		}
		f := picks[i].File
		switch {
		case !strings.HasPrefix(f, ts+"_") || !files[f]:
			return Refused(f + " is not in the target (Refresh lists it again)")
		case picks[i].Path != "" && (b.Items[n].Kind != "volume" || !safePath.MatchString(picks[i].Path) || strings.Contains(picks[i].Path, "..")):
			return Refused("the path " + picks[i].Path + " is a directory in the tar, e.g. app/data/images")
		}
	}
	if err := r.begin("restore", by); err != nil {
		return err
	}
	r.mu.Lock()
	r.run.TS = ts
	r.mu.Unlock()
	go func() {
		ctx := context.Background()
		r.started()
		r.finish(r.restore(ctx, ts, picks, imp, noSafety), ts+" restored")
	}()
	return nil
}

var safePath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)

func (r *Runner) restore(ctx context.Context, ts string, picks []Pick, imp, noSafety bool) error {
	c, err := r.cluster(ctx)
	if err != nil {
		return err
	}
	if noSafety {
		r.logf("no safety backup, as asked")
	} else {
		r.logf("safety backup first")
		res, err := r.runJob(ctx, "pre-restore", runPod(r.Set(), c, "pre-restore"))
		if err != nil {
			return fmt.Errorf("the safety backup failed, nothing was restored: %w", err)
		}
		r.logf("safety backup %s written", res.ts)
	}
	err = r.scaleDown(ctx, true)
	defer func() {
		r.scaleUp(ctx, r.Current(), true)
		r.mu.Lock()
		r.run.Scaled, r.run.Suspended = nil, nil
		r.mu.Unlock()
	}()
	if err != nil {
		return err
	}
	_, err = r.runJob(ctx, "restore", restorePod(r.Set(), c, picks, imp))
	return err
}

// --- the schedule and the CronJob's runs ---

// Sync keeps the CronJob as the set says and reads the runs it made; at most every 20 seconds
// unless forced.
func (r *Runner) Sync(ctx context.Context, force bool) error {
	r.mu.Lock()
	if !force && time.Since(r.syncedAt) < 20*time.Second {
		r.mu.Unlock()
		return nil
	}
	r.syncedAt = time.Now()
	r.mu.Unlock()
	if err := r.applyCron(ctx); err != nil {
		return err
	}
	return r.readScheduled(ctx)
}

func (r *Runner) applyCron(ctx context.Context) error {
	b := r.Set()
	var cur struct{ Metadata kube.Meta }
	err := r.k.Get(ctx, &cur, r.ns(), "get", "--", "cronjob", StateName)
	exists := err == nil
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	if b.Schedule == "" {
		if exists {
			_, err := r.k.Run(ctx, nil, r.ns(), "delete", "--ignore-not-found", "--", "cronjob", StateName)
			return err
		}
		return nil
	}
	c, err := r.cluster(ctx)
	if err != nil {
		return err
	}
	c.live = nil
	job := jobObject(b.Namespace, "", "scheduled", runPod(b, c, "scheduled"))["spec"]
	spec := map[string]any{"schedule": b.Schedule, "concurrencyPolicy": "Forbid", "startingDeadlineSeconds": 3600,
		"successfulJobsHistoryLimit": 3, "failedJobsHistoryLimit": 3,
		"jobTemplate": map[string]any{"metadata": map[string]any{"labels": map[string]string{kindLabel: "scheduled"}}, "spec": job}}
	if b.TimeZone != "" {
		spec["timeZone"] = b.TimeZone
	}
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:8])
	if exists && cur.Metadata.Annotations["switchyard/spec"] == hash {
		return nil
	}
	return r.k.Apply(ctx, map[string]any{"apiVersion": "batch/v1", "kind": "CronJob",
		"metadata": map[string]any{"name": StateName, "namespace": b.Namespace,
			"labels":      map[string]string{"app.kubernetes.io/managed-by": "switchyard"},
			"annotations": map[string]string{"switchyard/spec": hash}},
		"spec": spec})
}

// readScheduled reads the CronJob's finished Jobs not read yet: a run each, and the newest one's
// index.
func (r *Runner) readScheduled(ctx context.Context) error {
	var jobs struct{ Items []kube.Job }
	if err := r.k.Get(ctx, &jobs, r.ns(), "get", "jobs", "-l", kindLabel+"=scheduled"); err != nil {
		return err
	}
	st, err := r.ReadState(ctx)
	if err != nil {
		return err
	}
	var add []Run
	var newest result
	newestAt := st.IndexAt
	var seen []string
	for _, j := range jobs.Items {
		name := j.Metadata.Name
		if slices.Contains(st.Seen, name) {
			continue
		}
		phase := j.Phase()
		started := local(j.Status.StartTime)
		if started == "" {
			started = j.Metadata.CreationTimestamp.Local().Format(time.RFC3339)
		}
		run := Run{Kind: "scheduled", Status: "running", Started: started, Job: name, By: "the schedule"}
		out, _ := r.k.Run(ctx, nil, r.ns(), "logs", "--all-containers", "--prefix", "--ignore-errors", "--tail=-1", "--", "job/"+name)
		res := parse(string(out), r.itemNames())
		run.Log, run.TS = res.lines, res.ts
		if n := len(run.Log); n > logLines {
			run.Log = run.Log[n-logLines:]
		}
		if phase != "running" {
			run.Status, run.Finished = "ok", local(j.Status.CompletionTime)
			if phase == "failed" || !res.ok {
				run.Status, run.Message = "failed", "the scheduled backup failed, see its log"
			} else {
				run.Message = "backup " + res.ts + " written"
			}
			if run.Finished == "" {
				for _, c := range j.Status.Conditions {
					run.Finished = local(c.LastTransitionTime)
				}
			}
			seen = append(seen, name)
			if res.files != nil && later(run.Finished, newestAt) {
				newest, newestAt = res, run.Finished
			}
		}
		add = append(add, run)
	}
	if len(add) == 0 {
		return nil
	}
	return r.update(ctx, func(st *State) {
		for _, run := range add {
			putRun(st, run)
		}
		st.Seen = append(st.Seen, seen...)
		if newest.files != nil {
			st.Files, st.Manifests, st.IndexAt = newest.files, newest.man, newestAt
		}
	})
}

// SaveSet mirrors the set into the namespace's ConfigMap, for a reinstalled switchyard.
func (r *Runner) SaveSet(ctx context.Context) error { return r.update(ctx, func(*State) {}) }

// local writes the API's UTC times as switchyard writes its own.
func local(t string) string {
	x, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return t
	}
	return x.Local().Format(time.RFC3339)
}

func later(a, b string) bool {
	x, err1 := time.Parse(time.RFC3339, a)
	y, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && (err2 != nil || x.After(y))
}

// Remove deletes the CronJob; the archives and the ConfigMap stay.
func (r *Runner) Remove(ctx context.Context) error {
	if run := r.Current(); run != nil {
		return Refused("a " + run.Kind + " is running")
	}
	_, err := r.k.Run(ctx, nil, r.ns(), "delete", "--ignore-not-found", "--", "cronjob", StateName)
	return err
}
