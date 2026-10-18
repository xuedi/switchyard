// Package stack starts and stops a namespace as a whole, or one workload of it, through the
// scale subresource only. Stop suspends the CronJobs, scales the Deployments and then the
// StatefulSets to 0; Start brings them back the other way round, the StatefulSets ready first.
// What Start brings back is kept in the ConfigMap switchyard-running in switchyard's own
// namespace, not on the workloads: switchyard may scale them, not change them.
package stack

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

	"github.com/xuedi/switchyard/internal/kube"
)

const (
	Home       = "switchyard"         // switchyard's own namespace
	MemoryName = "switchyard-running" // the ConfigMap there
	wait       = 5 * time.Minute      // per group of workloads, for the pods to go or be ready
	logLines   = 200
)

// Saved is what Start brings back: the replicas, and who stopped it (namespace or workload).
type Saved struct {
	Replicas int    `json:"replicas"`
	By       string `json:"by"`
}

type Memory struct {
	Saved     map[string]map[string]Saved `json:"saved"`     // namespace -> deployment/<name> -> saved
	Suspended map[string][]string         `json:"suspended"` // namespace -> the CronJobs its Stop suspended
	Resumed   map[string]string           `json:"resumed"`   // namespace -> when its Start resumed them
}

// Run is one Start or Stop, of a namespace or (Target) one workload.
type Run struct {
	Kind     string // start or stop
	Target   string // deployment/<name> or statefulset/<name>, "" for the namespace
	Status   string // running, ok, failed
	Message  string
	Started  string
	Finished string
	By       string
	Log      []string
}

// Refused is a click that cannot be done now; nothing was started.
type Refused string

func (r Refused) Error() string { return string(r) }

type Engine struct {
	K    kube.Kube
	Busy func(ns string) string // a deploy or a backup runs there: what

	mu       sync.Mutex
	runs     map[string]*Run // the running or the last one, per namespace
	mem      *Memory         // nil until read
	memMu    sync.Mutex
	memErr   error // the last read's, not tried again for a while
	memErrAt time.Time
}

func NewEngine(k kube.Kube, busy func(string) string) *Engine {
	return &Engine{K: k, Busy: busy, runs: map[string]*Run{}}
}

func now() string { return time.Now().Format(time.RFC3339) }

// Last is a copy of the namespace's running or last run, nil when there was none.
func (e *Engine) Last(ns string) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.runs[ns]
	if r == nil {
		return nil
	}
	c := *r
	c.Log = slices.Clone(r.Log)
	return &c
}

// Running names the Start or Stop under way in the namespace, "" when none is.
func (e *Engine) Running(ns string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.runs[ns]; r != nil && r.Status == "running" {
		what := "a " + r.Kind + " of " + ns
		if r.Target != "" {
			what = "a " + r.Kind + " of " + ns + "/" + r.Target
		}
		return what
	}
	return ""
}

func (e *Engine) logf(ns, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	e.mu.Lock()
	if r := e.runs[ns]; r != nil {
		r.Log = append(r.Log, time.Now().Format("15:04:05")+" "+msg)
		if n := len(r.Log); n > logLines {
			r.Log = r.Log[n-logLines:]
		}
	}
	e.mu.Unlock()
	log.Printf("%s: %s", ns, msg)
}

// --- the memory ---

// Memory is a copy of the ConfigMap's content, read once and then kept: only switchyard writes it.
func (e *Engine) Memory(ctx context.Context) (Memory, error) {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	if err := e.loadMemory(ctx); err != nil {
		return Memory{}, err
	}
	return copyMemory(*e.mem), nil
}

func (e *Engine) loadMemory(ctx context.Context) error {
	if e.mem != nil {
		return nil
	}
	if e.memErr != nil && time.Since(e.memErrAt) < 30*time.Second {
		return e.memErr
	}
	var cm struct{ Data map[string]string }
	m := Memory{}
	err := e.K.Get(ctx, &cm, "--namespace="+Home, "get", "--", "configmap", MemoryName)
	if err != nil && !kube.IsNotFound(err) {
		e.memErr, e.memErrAt = err, time.Now()
		return err
	}
	if s := cm.Data["memory"]; s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return fmt.Errorf("%s: %w", MemoryName, err)
		}
	}
	e.mem = &m
	return nil
}

func copyMemory(m Memory) Memory {
	var c Memory
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &c)
	if c.Saved == nil {
		c.Saved = map[string]map[string]Saved{}
	}
	if c.Suspended == nil {
		c.Suspended = map[string][]string{}
	}
	if c.Resumed == nil {
		c.Resumed = map[string]string{}
	}
	return c
}

func (e *Engine) change(ctx context.Context, f func(m *Memory)) error {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	if err := e.loadMemory(ctx); err != nil {
		return err
	}
	m := copyMemory(*e.mem)
	f(&m)
	for ns, s := range m.Saved {
		if len(s) == 0 {
			delete(m.Saved, ns)
		}
	}
	for ns, s := range m.Suspended {
		if len(s) == 0 {
			delete(m.Suspended, ns)
		}
	}
	data, _ := json.Marshal(m)
	if err := e.K.Apply(ctx, map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": MemoryName, "namespace": Home,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "switchyard"}},
		"data": map[string]string{"memory": string(data)}}); err != nil {
		return err
	}
	e.mem = &m
	return nil
}

// Holds: the namespace's Stop suspended this CronJob (cronjob/<name>), its Start resumes it.
func (e *Engine) Holds(ns, obj string) bool {
	m, err := e.Memory(context.Background())
	return err == nil && slices.Contains(m.Suspended[ns], obj)
}

// Resumed is when the namespace's last Start resumed its CronJobs, zero when never.
func (e *Engine) Resumed(ns string) time.Time {
	m, _ := e.Memory(context.Background())
	t, _ := time.Parse(time.RFC3339, m.Resumed[ns])
	return t
}

// --- the clicks ---

func (e *Engine) begin(ns, kind, target, by string) error {
	if busy := e.Busy(ns); busy != "" {
		return Refused(busy + ": the " + kind + " waits for it")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.runs[ns]; r != nil && r.Status == "running" {
		return Refused("a " + r.Kind + " is still running in " + ns)
	}
	what := ns
	if target != "" {
		what = ns + "/" + target
	}
	e.runs[ns] = &Run{Kind: kind, Target: target, Status: "running", Started: now(), By: by,
		Log: []string{time.Now().Format("15:04:05") + " " + kind + " " + what + ", by " + by}}
	return nil
}

func (e *Engine) finish(ns string, err error, msg string) {
	if err != nil {
		e.logf(ns, "%v", err)
	} else {
		e.logf(ns, "%s", msg)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.runs[ns]
	r.Finished = now()
	if err != nil {
		r.Status, r.Message = "failed", err.Error()
	} else {
		r.Status, r.Message = "ok", msg
	}
}

func find(c Cluster, obj string) (Workload, bool) {
	for _, w := range c.Workloads {
		if w.Obj() == obj {
			return w, true
		}
	}
	return Workload{}, false
}

// Stop scales the namespace (target "") or one workload to 0, saving what ran.
func (e *Engine) Stop(ns, target, by string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Read(ctx, e.K, ns)
	if err != nil {
		return err
	}
	var todo []Workload
	if target != "" {
		w, ok := find(c, target)
		switch {
		case !ok:
			return Refused("no " + target + " in " + ns)
		case w.Replicas == 0:
			return Refused(target + " is stopped already")
		}
		todo = []Workload{w}
	} else {
		for _, w := range c.Workloads {
			if w.Replicas > 0 && !w.Canary {
				todo = append(todo, w)
			}
		}
		if len(todo) == 0 {
			return Refused("nothing runs in " + ns)
		}
	}
	if err := e.begin(ns, "stop", target, by); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		err := e.stop(ctx, ns, target, c, todo)
		e.finish(ns, err, "stopped")
	}()
	return nil
}

func (e *Engine) stop(ctx context.Context, ns, target string, c Cluster, todo []Workload) error {
	if target == "" {
		for _, cj := range c.CronJobs {
			if cj.Suspend {
				continue
			}
			obj := "cronjob/" + cj.Name
			_, err := e.K.Run(ctx, nil, "--namespace="+ns, "patch", "--type=merge", "-p", `{"spec":{"suspend":true}}`, "--", obj)
			if err != nil {
				if strings.Contains(err.Error(), "(Forbidden)") {
					e.logf(ns, "%s keeps running: switchyard has no Role in %s to suspend it", obj, ns)
					continue
				}
				return err
			}
			if err := e.change(ctx, func(m *Memory) { m.Suspended[ns] = append(m.Suspended[ns], obj) }); err != nil {
				return err
			}
			e.logf(ns, "%s suspended", obj)
		}
	}
	by := "namespace"
	if target != "" {
		by = "workload"
	}
	var late []string
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		var group []string
		for _, w := range todo {
			if w.Kind != kind {
				continue
			}
			err := e.change(ctx, func(m *Memory) {
				if m.Saved[ns] == nil {
					m.Saved[ns] = map[string]Saved{}
				}
				m.Saved[ns][w.Obj()] = Saved{Replicas: w.Replicas, By: by}
			})
			if err != nil {
				return err
			}
			if _, err := e.K.Run(ctx, nil, "--namespace="+ns, "scale", "--replicas=0", "--", w.Obj()); err != nil {
				return err
			}
			e.logf(ns, "%s scaled from %d to 0", w.Obj(), w.Replicas)
			group = append(group, w.Obj())
		}
		err := e.await(ctx, ns, group, func(w Workload) bool { return len(w.Pods) == 0 }, "gone")
		var slow slowError
		if errors.As(err, &slow) {
			late = append(late, slow...)
			e.logf(ns, "the pods of %s still there after %s, going on", strings.Join(slow, ", "), wait)
		} else if err != nil {
			return err
		}
	}
	if len(late) > 0 {
		return fmt.Errorf("scaled to 0, but the pods of %s still there after %s", strings.Join(late, ", "), wait)
	}
	return nil
}

// Start brings the namespace (target "") or one workload back to what its Stop saved, or 1.
func (e *Engine) Start(ns, target, by string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Read(ctx, e.K, ns)
	if err != nil {
		return err
	}
	m, err := e.Memory(ctx)
	if err != nil {
		return err
	}
	saved := m.Saved[ns]
	count := func(w Workload) int {
		if s, ok := saved[w.Obj()]; ok && s.Replicas > 0 {
			return s.Replicas
		}
		return 1
	}
	todo := map[string]int{}
	if target != "" {
		w, ok := find(c, target)
		switch {
		case !ok:
			return Refused("no " + target + " in " + ns)
		case w.Replicas > 0:
			return Refused(target + " runs already")
		}
		todo[w.Obj()] = count(w)
	} else {
		// what the namespace's Stop stopped; without such a Stop, everything at 0
		for _, w := range c.Workloads {
			if w.Replicas == 0 && !w.Canary && saved[w.Obj()].By == "namespace" {
				todo[w.Obj()] = count(w)
			}
		}
		if len(todo) == 0 {
			for _, w := range c.Workloads {
				if w.Replicas == 0 && !w.Canary {
					todo[w.Obj()] = count(w)
				}
			}
		}
		if len(todo) == 0 && len(m.Suspended[ns]) == 0 {
			return Refused("everything in " + ns + " runs already")
		}
	}
	if err := e.begin(ns, "start", target, by); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		err := e.start(ctx, ns, target, c, todo)
		e.finish(ns, err, "started")
	}()
	return nil
}

func (e *Engine) start(ctx context.Context, ns, target string, c Cluster, todo map[string]int) error {
	var late []string
	for _, kind := range []string{"StatefulSet", "Deployment"} {
		var group []string
		for _, w := range c.Workloads {
			n, ok := todo[w.Obj()]
			if !ok || w.Kind != kind {
				continue
			}
			if _, err := e.K.Run(ctx, nil, "--namespace="+ns, "scale", fmt.Sprintf("--replicas=%d", n), "--", w.Obj()); err != nil {
				return err
			}
			e.logf(ns, "%s scaled to %d", w.Obj(), n)
			if err := e.change(ctx, func(m *Memory) { delete(m.Saved[ns], w.Obj()) }); err != nil {
				return err
			}
			group = append(group, w.Obj())
		}
		err := e.await(ctx, ns, group, func(w Workload) bool { return w.Replicas > 0 && w.Ready >= w.Replicas }, "ready")
		var slow slowError
		if errors.As(err, &slow) {
			late = append(late, slow...)
			e.logf(ns, "%s not ready after %s, going on", strings.Join(slow, ", "), wait)
		} else if err != nil {
			return err
		}
	}
	if target == "" {
		m, err := e.Memory(ctx)
		if err != nil {
			return err
		}
		for _, obj := range m.Suspended[ns] {
			if _, err := e.K.Run(ctx, nil, "--namespace="+ns, "patch", "--type=merge", "-p", `{"spec":{"suspend":false}}`, "--", obj); err != nil && !kube.IsNotFound(err) {
				return err
			}
			e.logf(ns, "%s resumed", obj)
		}
		if err := e.change(ctx, func(m *Memory) { delete(m.Suspended, ns); m.Resumed[ns] = now() }); err != nil {
			return err
		}
	}
	if len(late) > 0 {
		return fmt.Errorf("started, but not ready after %s: %s", wait, strings.Join(late, ", "))
	}
	return nil
}

type slowError []string

func (s slowError) Error() string { return "not done in time: " + strings.Join(s, ", ") }

// await reads the namespace every 2 s until done holds for every workload of the group, or
// wait is over (a slowError naming the ones left).
func (e *Engine) await(ctx context.Context, ns string, group []string, done func(Workload) bool, what string) error {
	if len(group) == 0 {
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		c, err := Read(ctx, e.K, ns)
		if err != nil && !kube.IsOffline(err) {
			return err
		}
		var left []string
		for _, obj := range group {
			if w, ok := find(c, obj); ok && !done(w) {
				left = append(left, obj)
			}
		}
		if err == nil && len(left) == 0 {
			e.logf(ns, "%s %s", strings.Join(group, ", "), what)
			return nil
		}
		if time.Now().After(deadline) {
			return slowError(left)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
