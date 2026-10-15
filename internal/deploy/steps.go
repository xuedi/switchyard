package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
	"github.com/xuedi/switchyard/internal/sqlcheck"
)

const sqlMarker = "----- switchyard: pending SQL -----"

// deployment reads the box's Deployment and the container switchyard moves.
func (r *Runner) deployment(ctx context.Context) (kube.Deployment, kube.Container, error) {
	b := r.Box()
	var d kube.Deployment
	if err := r.k.Get(ctx, &d, "-n", b.Namespace, "get", "deployment", b.Deployment); err != nil {
		return d, kube.Container{}, err
	}
	c, ok := d.Spec.Template.Spec.Pick(b.Container)
	if !ok {
		return d, c, fmt.Errorf("%s has no container %q", b.Deployment, b.Container)
	}
	return d, c, nil
}

func (r *Runner) raw(ctx context.Context, what string) (map[string]any, error) {
	var m map[string]any
	err := r.k.Get(ctx, &m, "-n", r.Box().Namespace, "get", what)
	return m, err
}

// dig walks into nested maps, creating the missing ones.
func dig(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	return m
}

// container finds the box's container in a raw pod spec.
func container(spec map[string]any, name string) (map[string]any, bool) {
	list, _ := spec["containers"].([]any)
	for _, x := range list {
		c, _ := x.(map[string]any)
		if c != nil && (name == "" || c["name"] == name) {
			return c, true
		}
	}
	return nil, false
}

// port is the container's first port, 80 when it declares none.
func port(c map[string]any) int {
	ports, _ := c["ports"].([]any)
	for _, x := range ports {
		if p, ok := x.(map[string]any); ok {
			if n, ok := p["containerPort"].(float64); ok {
				return int(n)
			}
		}
	}
	return 80
}

// --- migrations and the other commands, in Jobs ---

// migrations runs the pending-SQL check and the migrate command in the new image. drops: the
// check found destructive statements and Deploy anyway let them through.
func (r *Runner) migrations(ctx context.Context, tag string, allow bool) (drops bool, err error) {
	b := r.Box()
	if b.PendingSQL == "" && b.Migrate == "" {
		return false, nil
	}
	r.step(ctx, "migrate", "migrations in %s", tag)
	if b.PendingSQL != "" {
		cmd := append([]string{"sh", "-c", `"$@" >/tmp/switchyard-pending.sql || exit $?; echo "` + sqlMarker + `"; cat /tmp/switchyard-pending.sql`, "sh"}, strings.Fields(b.PendingSQL)...)
		out, err := r.job(ctx, "pending-sql", tag, cmd)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		before, sql, found := strings.Cut(out, sqlMarker)
		switch {
		case err != nil && !allow:
			r.tail(before, 20)
			return false, &blocked{[]string{"the pending-SQL command failed (" + err.Error() + "), see the log"}}
		case err != nil:
			r.tail(before, 20)
			r.logf("the pending-SQL command failed: Deploy anyway goes on")
		case !found:
			return false, errors.New("the pending-SQL Job printed no SQL marker")
		default:
			list, summary := sqlcheck.Check(sql)
			r.logf("%s", summary)
			for _, x := range list {
				r.logf("BLOCKED %s", x)
			}
			if len(list) > 0 && !allow {
				return false, &blocked{list}
			}
			if len(list) > 0 {
				r.logf("destructive migrations allowed for this deploy")
				drops = true
			}
		}
	}
	if b.Migrate != "" {
		r.logf("migrate: %s", b.Migrate)
		out, err := r.job(ctx, "migrate", tag, strings.Fields(b.Migrate))
		r.tail(out, 30)
		if err != nil {
			return drops, fmt.Errorf("migrate: %w", err)
		}
	}
	return drops, nil
}

// after runs the cache command after a switch; a failure is only logged.
func (r *Runner) after(ctx context.Context, tag string) {
	b := r.Box()
	if b.Cache == "" {
		return
	}
	r.step(ctx, "after", "cache command: %s", b.Cache)
	out, err := r.job(ctx, "cache", tag, strings.Fields(b.Cache))
	r.tail(out, 10)
	if err != nil {
		r.logf("the cache command failed: %v", err)
	}
}

func (r *Runner) tail(out string, n int) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		if l != "" {
			r.logf("  | %s", l)
		}
	}
}

// job runs command in a Job made from the Deployment's pod spec (its environment, volumes and
// security context) with the image tag, and returns its log.
func (r *Runner) job(ctx context.Context, what, tag string, command []string) (string, error) {
	b := r.Box()
	d, err := r.raw(ctx, "deployment/"+b.Deployment)
	if err != nil {
		return "", err
	}
	spec := dig(d, "spec", "template", "spec")
	c, ok := container(spec, b.Container)
	if !ok {
		return "", fmt.Errorf("no container %q", b.Container)
	}
	c["image"] = registry.WithTag(c["image"].(string), tag)
	c["command"] = command
	delete(c, "args")
	for _, k := range []string{"readinessProbe", "livenessProbe", "startupProbe", "lifecycle", "ports"} {
		delete(c, k)
	}
	spec["containers"] = []any{c}
	spec["restartPolicy"] = "Never"
	labels := map[string]string{"app.kubernetes.io/managed-by": "switchyard", "switchyard/box": b.Deployment, "switchyard/job": what}
	name := fmt.Sprintf("%s-%s-%d", b.Deployment, what, time.Now().Unix())
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	job := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": b.Namespace, "labels": labels},
		"spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 1800, "ttlSecondsAfterFinished": 86400,
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": spec}},
	}
	body, _ := json.Marshal(job)
	if _, err := r.k.Run(ctx, body, "create", "-f", "-"); err != nil {
		return "", err
	}
	r.logf("job %s started", name)
	stop := func() {
		r.k.Run(context.Background(), nil, "-n", b.Namespace, "delete", "job", name, "--ignore-not-found", "--wait=false")
	}
	stuckSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			stop()
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		var j kube.Job
		if err := r.k.Get(ctx, &j, "-n", b.Namespace, "get", "job", name); err != nil {
			if ctx.Err() != nil {
				continue
			}
			return "", err
		}
		if phase := j.Phase(); phase != "running" {
			out, _ := r.k.Run(context.WithoutCancel(ctx), nil, "-n", b.Namespace, "logs", "job/"+name, "--tail=-1")
			if phase == "failed" {
				return string(out), fmt.Errorf("job %s failed", name)
			}
			return string(out), nil
		}
		var pods struct{ Items []kube.Pod }
		if r.k.Get(ctx, &pods, "-n", b.Namespace, "get", "pods", "-l", "job-name="+name) == nil {
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
				stop()
				return "", fmt.Errorf("job %s does not start: %s", name, waiting)
			}
		}
	}
}

// --- moving the Deployment and the followers ---

// setImage gives every container of obj that runs the box's repository the tag.
func (r *Runner) setImage(ctx context.Context, obj, tag string) error {
	b := r.Box()
	_, live, err := r.deployment(ctx)
	if err != nil {
		return err
	}
	repo := registry.Parse(live.Image)
	var spec kube.PodSpec
	if strings.HasPrefix(obj, "cronjob/") {
		var cj kube.CronJob
		if err := r.k.Get(ctx, &cj, "-n", b.Namespace, "get", obj); err != nil {
			return err
		}
		spec = cj.Spec.JobTemplate.Spec.Template.Spec
	} else {
		var d kube.Deployment
		if err := r.k.Get(ctx, &d, "-n", b.Namespace, "get", obj); err != nil {
			return err
		}
		spec = d.Spec.Template.Spec
	}
	args := []string{"-n", b.Namespace, "set", "image", obj}
	for _, c := range spec.Containers {
		if p := registry.Parse(c.Image); p.Registry == repo.Registry && p.Repo == repo.Repo {
			args = append(args, c.Name+"="+registry.WithTag(c.Image, tag))
		}
	}
	if len(args) == 5 {
		return fmt.Errorf("%s runs no container of %s", obj, repo.Repo)
	}
	_, err = r.k.Run(ctx, nil, args...)
	return err
}

func (r *Runner) rollout(ctx context.Context, deployment string) error {
	_, err := r.k.Run(ctx, nil, "-n", r.Box().Namespace, "rollout", "status", "deployment/"+deployment, "--timeout=600s", "--request-timeout=0")
	return err
}

// switchTo puts the tag on the Deployment and its followers, a normal rolling update. If the
// rollout fails, everything goes back to from.
func (r *Runner) switchTo(ctx context.Context, from, tag string) error {
	b := r.Box()
	r.noAbort()
	ctx = context.WithoutCancel(ctx)
	r.step(ctx, "switch", "%s to %s", b.Deployment, tag)
	err := r.setImage(ctx, "deployment/"+b.Deployment, tag)
	if err == nil {
		err = r.rollout(ctx, b.Deployment)
	}
	if err != nil {
		r.logf("%v", err)
		r.logf("back to %s", from)
		if err2 := r.setImage(ctx, "deployment/"+b.Deployment, from); err2 != nil {
			r.logf("could not go back: %v", err2)
		}
		return fmt.Errorf("%s did not roll out", tag)
	}
	for _, f := range b.Followers {
		if err := r.setImage(ctx, f, tag); err != nil {
			r.logf("follower %s: %v", f, err)
		} else {
			r.logf("follower %s on %s", f, tag)
		}
	}
	return nil
}

// --- the canary ---

// canary runs a copy of the Deployment with the new tag and the same pod labels, so its Service
// sends it a share of the traffic: 1 pod next to the n old ones, then as many as there are old
// ones; each step watched for the pause. Then the original gets the tag and the copy goes.
func (r *Runner) canary(ctx context.Context, from, tag string) error {
	b := r.Box()
	name := b.Deployment + "-canary"
	d, err := r.raw(ctx, "deployment/"+b.Deployment)
	if err != nil {
		return err
	}
	var orig kube.Deployment
	raw, _ := json.Marshal(d)
	json.Unmarshal(raw, &orig)
	n := orig.Replicas()

	spec := d["spec"].(map[string]any)
	delete(spec, "paused")
	spec["replicas"] = 1
	dig(spec, "selector", "matchLabels")[trackLabel] = "canary"
	dig(spec, "template", "metadata", "labels")[trackLabel] = "canary"
	c, ok := container(dig(spec, "template", "spec"), b.Container)
	if !ok {
		return fmt.Errorf("no container %q", b.Container)
	}
	c["image"] = registry.WithTag(c["image"].(string), tag)
	appPort := port(c)
	labels := map[string]any{"app.kubernetes.io/managed-by": "switchyard", trackLabel: "canary"}
	copyObj := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": b.Namespace, "labels": labels}, "spec": spec}

	remove := func() {
		r.k.Run(context.Background(), nil, "-n", b.Namespace, "delete", "deployment", name, "--ignore-not-found")
	}
	remove()
	body, _ := json.Marshal(copyObj)
	r.step(ctx, "one", "canary: %s with 1 pod of %s next to %d of %s", name, tag, n, from)
	if _, err := r.k.Run(ctx, body, "create", "-f", "-"); err != nil {
		return err
	}
	defer remove()

	oldBroke := false
	steps := []int{1}
	if n > 1 {
		steps = append(steps, n)
	}
	for i, pods := range steps {
		if i > 0 {
			r.step(ctx, "half", "canary: %d pods of %s next to %d of %s, about half the traffic", pods, tag, n, from)
			if _, err := r.k.Run(ctx, nil, "-n", b.Namespace, "scale", "deployment/"+name, fmt.Sprintf("--replicas=%d", pods)); err != nil {
				return err
			}
		}
		if err := r.waitReady(ctx, name, kube.Selector(orig.Spec.Selector.MatchLabels, trackLabel+"=canary")); err != nil {
			return err
		}
		r.logf("about %d %% of the traffic on %s", 100*pods/(pods+n), tag)
		switch r.watch(ctx, orig.Spec.Selector.MatchLabels, name, appPort) {
		case 1:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s failed with %d pods", tag, pods)
		case 2:
			r.logf("the old release broke on the migrated database: all traffic to %s now", tag)
			oldBroke = true
		}
		if oldBroke {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.switchTo(ctx, from, tag)
}

// waitReady waits until the copy's pods are ready; a pod that cannot pull or start fails it.
func (r *Runner) waitReady(ctx context.Context, name, selector string) error {
	b := r.Box()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var d kube.Deployment
		if err := r.k.Get(ctx, &d, "-n", b.Namespace, "get", "deployment", name); err != nil {
			return err
		}
		if d.Status.ReadyReplicas >= d.Replicas() && d.Status.UpdatedReplicas >= d.Replicas() {
			return nil
		}
		var pods struct{ Items []kube.Pod }
		if r.k.Get(ctx, &pods, "-n", b.Namespace, "get", "pods", "-l", selector) == nil {
			for _, p := range pods.Items {
				if w := p.Waiting(); strings.Contains(w, "ErrImage") || strings.Contains(w, "CrashLoopBackOff") || strings.Contains(w, "CreateContainerConfigError") {
					if time.Since(p.Metadata.CreationTimestamp) > 3*time.Minute {
						return fmt.Errorf("pod %s: %s", p.Metadata.Name, w)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after 10 minutes", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// pods returns the ready pods of a selector and their restarts.
func (r *Runner) pods(ctx context.Context, selector string) ([]kube.Pod, int, error) {
	var list struct{ Items []kube.Pod }
	if err := r.k.Get(ctx, &list, "-n", r.Box().Namespace, "get", "pods", "-l", selector); err != nil {
		return nil, 0, err
	}
	var ready []kube.Pod
	restarts := 0
	for _, p := range list.Items {
		restarts += p.Restarts()
		if p.Ready() {
			ready = append(ready, p)
		}
	}
	return ready, restarts, nil
}

// watch keeps an eye on the copy for the pause: its pods stay ready and do not restart, and the
// box's pages answer below 400 from a new pod and from an old one, round after round. 0: fine,
// 1: the new one failed, 2: the old one failed.
func (r *Runner) watch(ctx context.Context, origLabels map[string]string, name string, appPort int) int {
	b := r.Box()
	newSel := kube.Selector(origLabels, trackLabel+"=canary")
	oldSel := kube.Selector(origLabels, "!"+trackLabel)
	newPods, restarts, err := r.pods(ctx, newSel)
	if err != nil || len(newPods) == 0 {
		r.logf("no ready canary pod: %v", err)
		return 1
	}
	oldPods, _, _ := r.pods(ctx, oldSel)

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var newAddr, oldAddr string
	if len(b.Pages) > 0 {
		if newAddr, err = r.k.PortForward(wctx, b.Namespace, newPods[0].Metadata.Name, appPort); err != nil {
			r.logf("%v", err)
			return 1
		}
		if len(oldPods) > 0 {
			if oldAddr, err = r.k.PortForward(wctx, b.Namespace, oldPods[0].Metadata.Name, appPort); err != nil {
				r.logf("old pod: %v", err)
				oldAddr = ""
			}
		}
	}
	host, proto := "", ""
	if u, err := url.Parse(b.Site); err == nil && u.Host != "" {
		host, proto = u.Host, u.Scheme
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	requests, redirects, slowest := 0, 0, time.Duration(0)
	probe := func(addr string) error {
		for _, page := range b.Pages {
			req, err := http.NewRequestWithContext(wctx, "GET", "http://"+addr+page, nil)
			if err != nil {
				return err
			}
			if host != "" {
				req.Host = host
				req.Header.Set("X-Forwarded-Proto", proto)
			}
			t := time.Now()
			resp, err := client.Do(req)
			requests++
			if err != nil {
				return fmt.Errorf("%s: %v", page, err)
			}
			resp.Body.Close()
			if d := time.Since(t); d > slowest {
				slowest = d
			}
			if resp.StatusCode >= 400 {
				return fmt.Errorf("%s answered %d", page, resp.StatusCode)
			}
			if resp.StatusCode >= 300 {
				redirects++
			}
		}
		return nil
	}

	end := time.Now().Add(time.Duration(b.PauseOr()) * time.Second)
	for time.Now().Before(end) {
		if ctx.Err() != nil {
			return 1
		}
		var d kube.Deployment
		if err := r.k.Get(ctx, &d, "-n", b.Namespace, "get", "deployment", name); err != nil || d.Status.ReadyReplicas < d.Replicas() {
			r.logf("the canary is not healthy: %d of %d pods ready %v", d.Status.ReadyReplicas, d.Replicas(), errOrEmpty(err))
			return 1
		}
		if _, now, err := r.pods(ctx, newSel); err == nil && now > restarts {
			r.logf("a canary pod restarted")
			return 1
		}
		if newAddr == "" {
			select {
			case <-ctx.Done():
				return 1
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if err := probe(newAddr); err != nil {
			if ctx.Err() == nil {
				r.logf("new pod: %v", err)
			}
			return 1
		}
		if oldAddr != "" {
			if err := probe(oldAddr); err != nil {
				if ctx.Err() != nil {
					return 1
				}
				r.logf("old pod: %v", err)
				return 2
			}
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(time.Second):
		}
	}
	if newAddr != "" {
		r.logf("%d requests, no errors, %d redirects, slowest %s", requests, redirects, slowest.Round(10*time.Millisecond))
	} else {
		r.logf("pods stayed ready")
	}
	return 0
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
