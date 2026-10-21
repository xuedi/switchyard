// Package deploy moves a box's Deployment from one image tag to another: canary (a copy with the
// new tag next to the original, by pod count), quick (the new tag at once), rollback (a kept
// tag at once), with the migration Jobs in the new image before any traffic moves, the cache
// command after every switch, and the followers moved along. One run per box at a time; the
// history lives in the ConfigMap switchyard-<deployment> next to the app.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
)

const (
	Canary   = "canary"
	Quick    = "quick"
	Rollback = "rollback"

	trackLabel  = "switchyard/track" // only on the canary copy's pods
	logLines    = 400
	keepHistory = 10
)

// Run is one deploy, quick deploy or rollback.
type Run struct {
	Kind     string
	Status   string // running, ok, failed, aborted
	Step     string
	Message  string
	Started  string
	Finished string
	From, To string
	Allow    bool     // Deploy anyway: destructive migrations let through
	Blocked  []string // what stopped it before migrating
	Log      []string
}

// Deployed is a tag that went live; the kept ones are the rollback targets.
type Deployed struct {
	Tag   string
	At    string
	By    string
	Drops bool // it ran destructive migrations on purpose: older tags may break
}

type State struct {
	Deployed []Deployed     `json:"deployed"`          // newest deploy first
	Runs     map[string]Run `json:"runs"`              // the last one of each kind, the running one with its log
	History  []Run          `json:"history,omitempty"` // the finished runs with their logs, newest first
}

// remember puts a finished run at the top of the history; the last of its kind keeps no log.
func (st *State) remember(run Run) {
	st.History = slices.DeleteFunc(st.History, func(x Run) bool { return x.Started == run.Started })
	st.History = append([]Run{run}, st.History...)
	if len(st.History) > keepHistory {
		st.History = st.History[:keepHistory]
	}
	run.Log = nil
	st.Runs[run.Kind] = run
}

// All is every run the state knows, newest first: the history and the last of each kind from
// before it.
func (st State) All() []Run {
	all := slices.Clone(st.History)
	for _, r := range st.Runs {
		if !slices.ContainsFunc(all, func(x Run) bool { return x.Started == r.Started }) {
			all = append(all, r)
		}
	}
	slices.SortFunc(all, func(a, b Run) int { return strings.Compare(b.Started, a.Started) })
	return all
}

// Refused is a click that cannot be done now; nothing was started.
type Refused string

func (r Refused) Error() string { return string(r) }

// Engine holds a Runner per box.
type Engine struct {
	K  kube.Kube
	mu sync.Mutex
	r  map[string]*Runner
}

func NewEngine(k kube.Kube) *Engine { return &Engine{K: k, r: map[string]*Runner{}} }

// Runner returns the box's runner, with the box's current settings.
func (e *Engine) Runner(b config.Box) *Runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.r[b.ID()]
	if r == nil {
		r = &Runner{k: e.K}
		e.r[b.ID()] = r
	}
	r.mu.Lock()
	r.box = b
	r.mu.Unlock()
	return r
}

type Runner struct {
	k kube.Kube

	mu        sync.Mutex
	box       config.Box
	run       *Run // the one running, nil otherwise
	cancel    context.CancelFunc
	abortable bool
	reg       *registry.Client
	regRepo   string
	images    []registry.Image
	imagesAt  time.Time
	imagesErr error
	stateMu   sync.Mutex
}

func (r *Runner) Box() config.Box {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.box
}

// Current is a copy of the running run, nil when none runs; abortable while the original is
// untouched.
func (r *Runner) Current() (*Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil {
		return nil, false
	}
	c := *r.run
	c.Log = slices.Clone(r.run.Log)
	c.Blocked = slices.Clone(r.run.Blocked)
	return &c, r.abortable
}

func now() string { return time.Now().Format(time.RFC3339) }

func (r *Runner) logf(format string, a ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, a...)
	r.mu.Lock()
	if r.run != nil {
		r.run.Log = append(r.run.Log, strings.Split(line, "\n")...)
		if n := len(r.run.Log); n > logLines {
			r.run.Log = r.run.Log[n-logLines:]
		}
	}
	id := r.box.ID()
	r.mu.Unlock()
	log.Printf("%s: %s", id, fmt.Sprintf(format, a...))
}

func (r *Runner) set(f func(run *Run)) {
	r.mu.Lock()
	f(r.run)
	r.mu.Unlock()
}

// --- state ---

// ReadState reads the box's ConfigMap; a missing one is an empty state.
func (r *Runner) ReadState(ctx context.Context) (State, error) {
	b := r.Box()
	var cm struct {
		Data map[string]string `json:"data"`
	}
	st := State{Runs: map[string]Run{}}
	err := r.k.Get(ctx, &cm, "--namespace="+b.Namespace, "get", "--", "configmap", b.State())
	if kube.IsNotFound(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if s := cm.Data["state"]; s != "" {
		if err := json.Unmarshal([]byte(s), &st); err != nil {
			return st, fmt.Errorf("%s: %w", b.State(), err)
		}
	}
	if st.Runs == nil {
		st.Runs = map[string]Run{}
	}
	return st, nil
}

// update reads, changes and writes the state. switchyard is its only writer.
func (r *Runner) update(ctx context.Context, f func(st *State)) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	st, err := r.ReadState(ctx)
	if err != nil {
		return err
	}
	f(&st)
	b := r.Box()
	data, _ := json.Marshal(st)
	return r.k.Apply(ctx, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": b.State(), "namespace": b.Namespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "switchyard"}},
		"data": map[string]string{"state": string(data)},
	})
}

// save writes the running run into the state, so a restarted switchyard knows of it.
func (r *Runner) save(ctx context.Context) {
	run, _ := r.Current()
	if run == nil {
		return
	}
	if err := r.update(ctx, func(st *State) { st.Runs[run.Kind] = *run }); err != nil {
		log.Printf("%s: state: %v", r.Box().ID(), err)
	}
}

// Recover is called at the start: a run the state says is running died with the last
// switchyard. Its canary copy goes, and it is marked failed.
func (r *Runner) Recover(ctx context.Context) {
	st, err := r.ReadState(ctx)
	if err != nil {
		log.Printf("%s: recover: %v", r.Box().ID(), err)
		return
	}
	for kind, run := range st.Runs {
		if run.Status != "running" {
			continue
		}
		b := r.Box()
		log.Printf("%s: the %s to %s was cut off by a restart, marking it failed", b.ID(), kind, run.To)
		r.k.Run(ctx, nil, "--namespace="+b.Namespace, "delete", "--ignore-not-found", "--", "deployment", b.Deployment+"-canary")
		r.update(ctx, func(st *State) {
			run.Status, run.Finished = "failed", now()
			run.Message = "switchyard stopped during step " + run.Step + "; the canary copy was removed, check the Deployment"
			st.remember(run)
		})
	}
}

// --- registry ---

// Images returns the newest tags of the box's image, read at most once a minute.
func (r *Runner) Images(ctx context.Context, image string) ([]registry.Image, error) {
	r.mu.Lock()
	fresh := r.regRepo == repo(image) && time.Since(r.imagesAt) < time.Minute
	images, err := r.images, r.imagesErr
	r.mu.Unlock()
	if fresh {
		return images, err
	}
	c, err := r.registry(ctx, image)
	if err == nil {
		images, err = c.Images(ctx, 30)
	}
	r.mu.Lock()
	r.images, r.imagesErr, r.imagesAt = images, err, time.Now()
	if err != nil {
		r.reg = nil // the pull secret may have changed
	}
	r.mu.Unlock()
	return images, err
}

// Image reads one tag of the box's repository.
func (r *Runner) Image(ctx context.Context, image, tag string) (registry.Image, error) {
	c, err := r.registry(ctx, image)
	if err != nil {
		return registry.Image{}, err
	}
	return c.Image(ctx, tag)
}

func repo(image string) string {
	ref := registry.Parse(image)
	return ref.Registry + "/" + ref.Repo
}

// registry is a client for the image's repository with the pull secrets of the pod spec and of
// its service account.
func (r *Runner) registry(ctx context.Context, image string) (*registry.Client, error) {
	ref := registry.Parse(image)
	r.mu.Lock()
	if r.reg != nil && r.regRepo == repo(image) {
		c := r.reg
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()
	b := r.Box()
	names, err := PullSecrets(ctx, r.k, b.Namespace, b.Deployment)
	if err != nil {
		return nil, err
	}
	user, pass := "", ""
	for _, n := range names {
		var s struct {
			Data map[string][]byte `json:"data"`
		}
		if err := r.k.Get(ctx, &s, "--namespace="+b.Namespace, "get", "--", "secret", n); err != nil {
			continue
		}
		if u, p, ok := registry.Creds(s.Data[".dockerconfigjson"], ref.Registry); ok {
			user, pass = u, p
			break
		}
	}
	c := registry.New(ref, user, pass)
	r.mu.Lock()
	r.reg, r.regRepo = c, repo(image)
	r.mu.Unlock()
	return c, nil
}

// PullSecrets names the Deployment's image pull secrets and those of its service account.
func PullSecrets(ctx context.Context, k kube.Kube, ns, deployment string) ([]string, error) {
	var d kube.Deployment
	if err := k.Get(ctx, &d, "--namespace="+ns, "get", "--", "deployment", deployment); err != nil {
		return nil, err
	}
	var names []string
	for _, s := range d.Spec.Template.Spec.ImagePullSecrets {
		names = append(names, s.Name)
	}
	sa := d.Spec.Template.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	var acc struct {
		ImagePullSecrets []struct{ Name string } `json:"imagePullSecrets"`
	}
	if err := k.Get(ctx, &acc, "--namespace="+ns, "get", "--", "serviceaccount", sa); err == nil {
		for _, s := range acc.ImagePullSecrets {
			if !slices.Contains(names, s.Name) {
				names = append(names, s.Name)
			}
		}
	}
	return names, nil
}

// --- starting and stopping ---

// Start checks the click and starts the run in the background; by goes into its log.
func (r *Runner) Start(kind, tag string, allow bool, by string) error {
	r.mu.Lock()
	if r.run != nil {
		r.mu.Unlock()
		return Refused("a " + r.run.Kind + " is still running")
	}
	r.mu.Unlock()
	if tag == "" || strings.ContainsAny(tag, " /:@") {
		return Refused("no tag given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b := r.Box()
	d, c, err := r.deployment(ctx)
	if err != nil {
		return err
	}
	live := registry.Parse(c.Image).Tag
	if tag == live {
		return Refused(tag + " is already live")
	}
	switch kind {
	case Canary, Quick:
		reg, err := r.registry(ctx, c.Image)
		if err != nil {
			return err
		}
		if _, err := reg.Image(ctx, tag); err != nil {
			return Refused("tag " + tag + ": " + err.Error())
		}
	case Rollback:
		st, err := r.ReadState(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(st.Deployed, func(x Deployed) bool { return x.Tag == tag }) {
			return Refused(tag + " is not one of the kept tags")
		}
	default:
		return Refused("unknown kind " + kind)
	}
	if kind == Canary && d.Replicas() < 1 {
		return Refused(b.Deployment + " is scaled to 0: a canary needs a running original")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run != nil {
		return Refused("a " + r.run.Kind + " is still running")
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	r.run = &Run{Kind: kind, Status: "running", Started: now(), From: live, To: tag, Allow: allow,
		Log: []string{time.Now().Format("15:04:05") + " " + kind + " to " + tag + ", started by " + by}}
	r.cancel, r.abortable = runCancel, kind != Rollback
	go r.exec(runCtx)
	return nil
}

// Abort stops a canary or a quick deploy before the original Deployment is touched.
func (r *Runner) Abort(by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil {
		return Refused("nothing is running")
	}
	if !r.abortable {
		return Refused("too late to abort: the " + r.run.Kind + " is switching the Deployment")
	}
	r.run.Log = append(r.run.Log, time.Now().Format("15:04:05")+" abort by "+by)
	r.cancel()
	return nil
}

func (r *Runner) noAbort() {
	r.mu.Lock()
	r.abortable = false
	r.mu.Unlock()
}

// blocked is a stop before migrating.
type blocked struct{ list []string }

func (b *blocked) Error() string { return "stopped before migrating" }

func (r *Runner) exec(ctx context.Context) {
	run, _ := r.Current()
	bg := context.Background()
	r.save(bg)
	var drops bool
	err := func() error {
		var err error
		switch run.Kind {
		case Canary, Quick:
			if drops, err = r.migrations(ctx, run.To, run.Allow); err != nil {
				return err
			}
			if run.Kind == Canary {
				err = r.canary(ctx, run.From, run.To)
			} else {
				err = r.switchTo(ctx, run.From, run.To)
			}
		case Rollback:
			r.noAbort()
			err = r.switchTo(ctx, run.From, run.To)
		}
		if err != nil {
			return err
		}
		r.after(bg, run.To)
		return nil
	}()

	r.set(func(x *Run) {
		x.Finished = now()
		var bl *blocked
		switch {
		case err == nil:
			x.Status, x.Step, x.Message = "ok", "done", x.To+" is live"
		case errors.As(err, &bl):
			x.Status, x.Blocked = "failed", bl.list
			x.Message = "stopped before migrating, " + x.From + " still live (Deploy anyway lets it through)"
		case ctx.Err() != nil:
			x.Status, x.Message = "aborted", "aborted in step "+x.Step+", "+x.From+" still live"
		default:
			x.Status, x.Message = "failed", "failed in step "+x.Step+": "+err.Error()
		}
	})
	if ctx.Err() != nil && err != nil {
		r.logf("aborted")
	} else if err != nil {
		r.logf("%v", err)
	} else {
		r.logf("done: %s is live", run.To)
	}
	final, _ := r.Current()
	b := r.Box()
	if err := r.update(bg, func(st *State) {
		st.remember(*final)
		if final.Status != "ok" {
			return
		}
		if final.Kind == Rollback {
			return // the order of the kept tags stays: a rollback goes back, it adds nothing
		}
		st.Deployed = slices.DeleteFunc(st.Deployed, func(x Deployed) bool { return x.Tag == final.To })
		st.Deployed = append([]Deployed{{Tag: final.To, At: final.Finished, By: final.Kind, Drops: drops}}, st.Deployed...)
		if !slices.ContainsFunc(st.Deployed, func(x Deployed) bool { return x.Tag == final.From }) && final.From != "" {
			st.Deployed = append(st.Deployed, Deployed{Tag: final.From, At: final.Started, By: "before"})
		}
		if len(st.Deployed) > b.KeepOr() {
			st.Deployed = st.Deployed[:b.KeepOr()]
		}
	}); err != nil {
		log.Printf("%s: state: %v", b.ID(), err)
	}
	r.mu.Lock()
	r.run, r.cancel = nil, nil
	r.mu.Unlock()
}

func (r *Runner) step(ctx context.Context, step, format string, a ...any) {
	r.set(func(x *Run) { x.Step = step })
	r.logf(format, a...)
	r.save(context.WithoutCancel(ctx))
}
