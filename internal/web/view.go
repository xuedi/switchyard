package web

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/deploy"
	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
)

// View is what a box's page shows, read once a second while it is open.
type View struct {
	Version   string
	Cluster   string
	Time      string
	Online    bool
	Error     string
	Box       string
	Namespace string
	Name      string
	Links     []config.Link
	Repo      string
	Live      string
	Subject   string
	Replicas  int
	Ready     int
	Canary    *Canary
	Images    []Row
	Keep      int
	Pause     int
	Runs      map[string]deploy.Run // the last of each kind, without the log
	Run       *deploy.Run           // the running one, or the newest finished, with its log
	Busy      bool
	Abortable bool
}

type Canary struct {
	Tag             string
	Replicas, Ready int
	Share           int // about this many percent of the traffic
}

type Row struct {
	Tag      string
	Created  string
	Version  string
	Subject  string
	Revision string
	Meta     []Meta
	Live     bool
	Kept     bool
	Canary   bool
	Newer    bool // than the live one
	Drops    bool
	By, At   string // how and when it went live
	Back     int    // kept tags between the live one and it, negative when newer
	Missing  bool   // kept, but gone from the registry
}

type Meta struct{ Key, Value string }

func (s *Server) view(id string) any {
	cfg := s.store.Get()
	v := View{Version: s.version, Box: id, Time: time.Now().Format("15:04:05")}
	if cfg == nil {
		return v
	}
	v.Cluster = cfg.Cluster
	b, ok := s.store.Box(id)
	if !ok {
		v.Error = "no such box"
		return v
	}
	v.Namespace, v.Name, v.Keep, v.Pause = b.Namespace, b.Deployment, b.KeepOr(), b.PauseOr()
	v.Links, _ = config.ParseLinks(b.Links)
	if b.Site != "" {
		v.Links = append([]config.Link{{Label: "site", URL: b.Site}}, v.Links...)
	}
	run := s.eng().Runner(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	err := s.eng().K.Get(ctx, &list, "--namespace="+b.Namespace, "get", "--ignore-not-found", "--", "deployment/"+b.Deployment, "deployment/"+b.Deployment+"-canary", "configmap/"+b.State())
	if kube.IsOffline(err) {
		return v
	}
	v.Online = true
	if err != nil {
		v.Error = err.Error()
		return v
	}
	var dep, cp *kube.Deployment
	var st deploy.State
	for _, raw := range list.Items {
		var head struct {
			Kind     string
			Metadata kube.Meta
			Data     map[string]string
		}
		json.Unmarshal(raw, &head)
		switch {
		case head.Kind == "ConfigMap":
			json.Unmarshal([]byte(head.Data["state"]), &st)
		case head.Metadata.Name == b.Deployment:
			dep = &kube.Deployment{}
			json.Unmarshal(raw, dep)
		default:
			cp = &kube.Deployment{}
			json.Unmarshal(raw, cp)
		}
	}
	if dep == nil {
		v.Error = "deployment " + b.Deployment + " not found in " + b.Namespace
		return v
	}
	c, _ := dep.Spec.Template.Spec.Pick(b.Container)
	ref := registry.Parse(c.Image)
	v.Repo, v.Live = ref.Registry+"/"+ref.Repo, ref.Tag
	v.Replicas, v.Ready = dep.Replicas(), dep.Status.ReadyReplicas
	if cp != nil {
		cc, _ := cp.Spec.Template.Spec.Pick(b.Container)
		v.Canary = &Canary{Tag: registry.Parse(cc.Image).Tag, Replicas: cp.Replicas(), Ready: cp.Status.ReadyReplicas}
		if total := v.Canary.Ready + v.Ready; total > 0 {
			v.Canary.Share = 100 * v.Canary.Ready / total
		}
	}

	v.Runs = map[string]deploy.Run{}
	var newest *deploy.Run
	for kind, r := range st.Runs {
		if newest == nil || r.Started > newest.Started {
			full := r
			newest = &full
		}
		r.Log = nil
		v.Runs[kind] = r
	}
	if cur, abortable := run.Current(); cur != nil {
		v.Run, v.Busy, v.Abortable = cur, true, abortable
		lite := *cur
		lite.Log = nil
		v.Runs[cur.Kind] = lite
	} else {
		v.Run = newest
	}

	images, err := run.Images(ctx, c.Image)
	if err != nil {
		v.Error = "registry: " + err.Error()
	}
	v.Images = rows(ctx, run, c.Image, images, st, v.Live, v.Canary)
	for _, r := range v.Images {
		if r.Live {
			v.Subject = r.Subject
		}
	}
	return v
}

// rows lists the tags newer than the live one, the live one and the kept ones, newest first.
func rows(ctx context.Context, run *deploy.Runner, image string, images []registry.Image, st deploy.State, live string, cp *Canary) []Row {
	byTag := map[string]registry.Image{}
	for _, img := range images {
		byTag[img.Tag] = img
	}
	kept := map[string]deploy.Deployed{}
	for _, d := range st.Deployed {
		kept[d.Tag] = d
	}
	// the live and kept tags may be older than the newest the registry gave
	missing := map[string]bool{}
	for _, tag := range append([]string{live}, keys(kept)...) {
		if _, ok := byTag[tag]; ok || tag == "" {
			continue
		}
		if img, err := run.Image(ctx, image, tag); err == nil {
			byTag[tag] = img
		} else {
			byTag[tag] = registry.Image{Tag: tag}
			missing[tag] = true
		}
	}
	liveCreated := byTag[live].Created

	var list []Row
	for tag, img := range byTag {
		_, isKept := kept[tag]
		newer := img.Created.After(liveCreated)
		if tag != live && !isKept && !newer {
			continue
		}
		r := Row{Tag: tag, Live: tag == live, Kept: isKept, Newer: newer && tag != live, Missing: missing[tag],
			Version: img.Labels["org.opencontainers.image.version"], Subject: img.Labels["switchyard.subject"],
			Revision: img.Labels["org.opencontainers.image.revision"], Canary: cp != nil && cp.Tag == tag}
		if !img.Created.IsZero() {
			r.Created = img.Created.Format(time.RFC3339)
		}
		for k, val := range img.Labels {
			if key, ok := strings.CutPrefix(k, "switchyard.meta."); ok {
				r.Meta = append(r.Meta, Meta{key, val})
			}
		}
		sort.Slice(r.Meta, func(i, j int) bool { return r.Meta[i].Key < r.Meta[j].Key })
		if d, ok := kept[tag]; ok {
			r.By, r.At, r.Drops = d.By, d.At, d.Drops
		}
		list = append(list, r)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Created != list[j].Created {
			return list[i].Created > list[j].Created
		}
		return list[i].Tag > list[j].Tag
	})
	// Back counts the kept tags (and the live one) between the live one and each row
	at := -1
	var order []string
	for _, r := range list {
		if r.Kept || r.Live {
			order = append(order, r.Tag)
		}
		if r.Live {
			at = len(order) - 1
		}
	}
	for i := range list {
		if j := slices.Index(order, list[i].Tag); j >= 0 && at >= 0 {
			list[i].Back = j - at
		}
	}
	return list
}

func keys(m map[string]deploy.Deployed) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
