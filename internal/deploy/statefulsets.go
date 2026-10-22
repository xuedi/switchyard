package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
)

// StatefulSetLabel starts a tag's labels that ask for a StatefulSet of the box's namespace on an
// image: switchyard.statefulset.<name>=<repo>:<tag>@sha256:<digest>.
const StatefulSetLabel = "switchyard.statefulset."

// Move is a managed StatefulSet put on the image its tag wants; forward only.
type Move struct {
	StatefulSet string
	From, To    string
}

// Wanted is what a tag's labels ask for: StatefulSet -> image.
func Wanted(labels map[string]string) map[string]string {
	w := map[string]string{}
	for k, v := range labels {
		if name, ok := strings.CutPrefix(k, StatefulSetLabel); ok {
			w[name] = strings.TrimSpace(v)
		}
	}
	return w
}

// Current is a StatefulSet as it runs: the spec's image, its containers, and the digest all its
// pods run ("" when they differ or one has not pulled; the spec's digest when no pod runs).
type Current struct {
	Image      string
	Digest     string
	Containers int
	Pods       bool
}

// Shown is the image with the digest that runs, when the spec has none.
func (c Current) Shown() string {
	switch {
	case c.Pods && c.Digest == "":
		return c.Image + " (not on every pod)"
	case c.Digest != "" && registry.Digest(c.Image) == "":
		return c.Image + "@" + c.Digest
	}
	return c.Image
}

// Same: the StatefulSet runs image already, by digest; its pods count, not only the spec.
func (c Current) Same(image string) bool {
	if c.Digest != "" && c.Digest == registry.Digest(image) {
		return true
	}
	return !c.Pods && c.Image == image
}

// CurrentOf reads the digest from the pods of the StatefulSet among pods.
func CurrentOf(s kube.StatefulSet, pods []kube.Pod) Current {
	cur := Current{Containers: len(s.Spec.Template.Spec.Containers)}
	if c, ok := s.Spec.Template.Spec.Pick(""); ok {
		cur.Image = c.Image
	}
	for _, p := range pods {
		if !ownedBy(p, s.Metadata.Name) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			d := registry.Digest(cs.ImageID)
			switch {
			case !cur.Pods:
				cur.Digest, cur.Pods = d, true
			case d != cur.Digest:
				cur.Digest = ""
			}
		}
	}
	if !cur.Pods {
		cur.Digest = registry.Digest(cur.Image)
	}
	return cur
}

func ownedBy(p kube.Pod, statefulSet string) bool {
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == "StatefulSet" && o.Name == statefulSet {
			return true
		}
	}
	return false
}

func (r *Runner) statefulSet(ctx context.Context, name string) (Current, error) {
	ns := "--namespace=" + r.Box().Namespace
	var s kube.StatefulSet
	if err := r.k.Get(ctx, &s, ns, "get", "--", "statefulset", name); err != nil {
		return Current{}, err
	}
	var pods struct{ Items []kube.Pod }
	if err := r.k.Get(ctx, &pods, ns, "get", "pods", "-l", kube.Selector(s.Spec.Selector.MatchLabels)); err != nil {
		return Current{}, err
	}
	return CurrentOf(s, pods.Items), nil
}

// plan compares what a tag's labels want with what runs: the managed StatefulSets to move, or
// why the tag cannot go live. A box that manages none leaves the labels alone.
func (r *Runner) plan(ctx context.Context, tag string, labels map[string]string) ([]Move, error) {
	b := r.Box()
	want := Wanted(labels)
	if len(b.StatefulSets) == 0 {
		return nil, nil
	}
	var names []string
	for n := range want {
		names = append(names, n)
	}
	slices.Sort(names)
	var moves []Move
	for _, name := range names {
		img := want[name]
		switch {
		case !kube.ValidName(name):
			return nil, Refused(tag + " has a label for " + name + ", which is no StatefulSet name")
		case !slices.Contains(b.StatefulSets, name):
			return nil, Refused(tag + " wants statefulset " + name + " on " + img + ", and the box does not manage it")
		case registry.Digest(img) == "":
			return nil, Refused(tag + " wants statefulset " + name + " on " + img + ": without @sha256: it is not moved")
		}
		cur, err := r.statefulSet(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("statefulset %s: %w", name, err)
		}
		if cur.Containers != 1 {
			return nil, Refused(fmt.Sprintf("statefulset %s has %d containers: switchyard moves only one", name, cur.Containers))
		}
		if !cur.Same(img) {
			moves = append(moves, Move{StatefulSet: name, From: cur.Shown(), To: img})
		}
	}
	return moves, nil
}

// statefulSets moves the managed StatefulSets whose image the tag wants changed: the namespace's
// backup first, then one after the other, each waited for. Forward only: one that moved stays.
func (r *Runner) statefulSets(ctx context.Context, tag string) error {
	b := r.Box()
	if len(b.StatefulSets) == 0 {
		return nil
	}
	_, c, err := r.deployment(ctx)
	if err != nil {
		return err
	}
	img, err := r.Image(ctx, c.Image, tag)
	if err != nil {
		return err
	}
	moves, err := r.plan(ctx, tag, img.Labels)
	if err != nil {
		return err
	}
	if len(moves) == 0 {
		if len(Wanted(img.Labels)) > 0 {
			r.logf("the managed statefulsets already run what %s wants", tag)
		}
		return nil
	}
	r.set(func(x *Run) { x.Moves = moves })
	r.step(ctx, "statefulsets", "%d statefulset(s) to move, the backup of %s first", len(moves), b.Namespace)
	if r.e.Backup == nil {
		return fmt.Errorf("no backup to run")
	}
	ts, err := r.e.Backup(ctx, b.Namespace, "the deploy of "+b.ID()+" to "+tag)
	if err != nil {
		return fmt.Errorf("the backup before moving the statefulsets: %w", err)
	}
	r.logf("backup %s written", ts)
	for _, m := range moves {
		r.logf("statefulset %s: %s -> %s", m.StatefulSet, m.From, m.To)
		patch, _ := json.Marshal([]any{map[string]any{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": m.To}})
		if _, err := r.k.Run(context.WithoutCancel(ctx), nil, "--namespace="+b.Namespace, "patch", "--type=json", "-p", string(patch), "--", "statefulset", m.StatefulSet); err != nil {
			return fmt.Errorf("statefulset %s: %w", m.StatefulSet, err)
		}
		if err := r.waitStatefulSet(ctx, m.StatefulSet); err != nil {
			r.logf("statefulset %s stays on %s: forward only, the backup %s is the way back", m.StatefulSet, m.To, ts)
			return fmt.Errorf("statefulset %s: %w", m.StatefulSet, err)
		}
		r.logf("statefulset %s ready on %s", m.StatefulSet, m.To)
	}
	return nil
}

// waitStatefulSet waits until every pod runs the new template and is ready; a new pod that cannot
// pull or start fails it, and so does an old one that is stuck: the controller does not replace
// a pod that never got ready, it has to be deleted.
func (r *Runner) waitStatefulSet(ctx context.Context, name string) error {
	ns := "--namespace=" + r.Box().Namespace
	start := time.Now()
	deadline := start.Add(10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var s kube.StatefulSet
		if err := r.k.Get(ctx, &s, ns, "get", "--", "statefulset", name); err != nil {
			return err
		}
		if s.RolledOut() {
			return nil
		}
		var pods struct{ Items []kube.Pod }
		if r.k.Get(ctx, &pods, ns, "get", "pods", "-l", kube.Selector(s.Spec.Selector.MatchLabels)) == nil {
			for _, p := range pods.Items {
				w := p.Waiting()
				if !ownedBy(p, name) || !strings.Contains(w, "ErrImage") && !strings.Contains(w, "CrashLoopBackOff") && !strings.Contains(w, "CreateContainerConfigError") {
					continue
				}
				switch {
				case p.Metadata.Labels["controller-revision-hash"] != s.Status.UpdateRevision && time.Since(start) > time.Minute:
					return fmt.Errorf("pod %s is stuck on the old image (%s) and the StatefulSet does not replace it: delete the pod, it comes back on the new one", p.Metadata.Name, w)
				case time.Since(p.Metadata.CreationTimestamp) > 3*time.Minute && p.Metadata.Labels["controller-revision-hash"] == s.Status.UpdateRevision:
					return fmt.Errorf("pod %s: %s", p.Metadata.Name, w)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after 10 minutes")
		}
	}
}

// staysOn logs, for a rollback, the managed StatefulSets the tag it goes back to wants on other
// images: a rollback moves the app only.
func (r *Runner) staysOn(ctx context.Context, tag string) {
	b := r.Box()
	if len(b.StatefulSets) == 0 {
		return
	}
	_, c, err := r.deployment(ctx)
	if err != nil {
		return
	}
	img, err := r.Image(ctx, c.Image, tag)
	if err != nil {
		return
	}
	for name, want := range Wanted(img.Labels) {
		if !slices.Contains(b.StatefulSets, name) {
			continue
		}
		if cur, err := r.statefulSet(ctx, name); err == nil && !cur.Same(want) {
			r.logf("statefulset %s stays on %s (%s was built with %s): a rollback moves the app only, going back is a restore", name, cur.Shown(), tag, want)
		}
	}
}
