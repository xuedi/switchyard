package web

import (
	"context"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
	"github.com/xuedi/switchyard/internal/stack"
)

// RunningView is what the Running page shows: every namespace with its workloads.
type RunningView struct {
	Version    string
	Cluster    string
	Time       string
	Online     bool
	Enabled    bool // the ClusterRole is there
	Error      string
	Metrics    bool
	Namespaces []NamespaceView
}

type NamespaceView struct {
	Name       string
	State      string // running, starting, partly, stopping, stopped
	Busy       string // what runs there: a deploy, a backup, a start or stop
	Production bool   // its backup set is: Stop wants the name typed in
	Role       bool   // switchyard has a Role here: Stop suspends the CronJobs
	Replicas   int
	Ready      int
	CPU, Mem   int64
	CanStart   bool
	CanStop    bool
	Workloads  []WorkloadView
	CronJobs   []CronView
	Run        *stack.Run
}

type WorkloadView struct {
	Kind      string
	Name      string
	Obj       string
	State     string
	Replicas  int
	Ready     int
	Image     string
	Canary    bool
	Saved     int // what Start brings back, 0 for 1
	Addresses []string
	CPU, Mem  int64
	Pods      []PodView
}

type PodView struct {
	Name       string
	Ready      bool
	Phase      string
	Waiting    string
	Started    string
	Restarts   int
	CPU, Mem   int64
	Containers []ContainerView // when there are several
}

type ContainerView struct {
	Name     string
	Ready    bool
	Restarts int
}

type CronView struct {
	Name     string
	Schedule string
	Suspend  bool
	Held     bool // suspended by the namespace's Stop
	Active   int
	Last     string
}

func (s *Server) runningView() any {
	cfg := s.store.Get()
	v := RunningView{Version: s.version, Time: time.Now().Format("15:04:05")}
	if cfg == nil {
		return v
	}
	v.Cluster = cfg.Cluster
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k := s.eng().K
	c, err := stack.Read(ctx, k, "")
	if kube.IsOffline(err) {
		return v
	}
	v.Online = true
	if err != nil {
		if strings.Contains(err.Error(), "(Forbidden)") {
			return v
		}
		v.Error = err.Error()
		return v
	}
	v.Enabled = true
	use := s.podUsage(ctx, k)
	v.Metrics = use != nil
	mem, _ := s.stk().Memory(ctx)

	byNS := map[string]*NamespaceView{}
	var order []string
	ns := func(name string) *NamespaceView {
		n := byNS[name]
		if n == nil {
			n = &NamespaceView{Name: name}
			byNS[name] = n
			order = append(order, name)
		}
		return n
	}
	for _, w := range c.Workloads {
		if stack.Hidden(w.Namespace, cfg.RunningHide) {
			continue
		}
		n := ns(w.Namespace)
		wv := WorkloadView{Kind: w.Kind, Name: w.Name, Obj: w.Obj(), State: w.State(), Replicas: w.Replicas, Ready: w.Ready,
			Canary: w.Canary, Saved: mem.Saved[w.Namespace][w.Obj()].Replicas, Addresses: c.Addresses(w)}
		if w.Image != "" {
			ref := registry.Parse(w.Image)
			wv.Image = path.Base(ref.Repo) + ":" + ref.Tag
		}
		for _, p := range w.Pods {
			pv := PodView{Name: p.Metadata.Name, Ready: p.Ready(), Phase: p.Status.Phase, Waiting: p.Waiting(),
				Started: p.Status.StartTime, Restarts: p.Restarts()}
			u := use[w.Namespace+"/"+p.Metadata.Name]
			pv.CPU, pv.Mem = u.CPU, u.Mem
			if len(p.Status.ContainerStatuses) > 1 {
				for _, cs := range p.Status.ContainerStatuses {
					pv.Containers = append(pv.Containers, ContainerView{Name: cs.Name, Ready: cs.Ready, Restarts: cs.RestartCount})
				}
			}
			wv.CPU += pv.CPU
			wv.Mem += pv.Mem
			wv.Pods = append(wv.Pods, pv)
		}
		n.Workloads = append(n.Workloads, wv)
		n.CPU += wv.CPU
		n.Mem += wv.Mem
		if !w.Canary {
			n.Replicas += w.Replicas
			n.Ready += min(w.Ready, w.Replicas)
		}
	}
	for _, cj := range c.CronJobs {
		if n := byNS[cj.Namespace]; n != nil {
			obj := "cronjob/" + cj.Name
			n.CronJobs = append(n.CronJobs, CronView{Name: cj.Name, Schedule: cj.Schedule, Suspend: cj.Suspend,
				Held: slices.Contains(mem.Suspended[cj.Namespace], obj), Active: cj.Active, Last: cj.LastSchedule})
		}
	}
	slices.Sort(order)
	for _, name := range order {
		n := byNS[name]
		set, hasSet := s.store.Backup(name)
		n.Production = hasSet && set.Production
		n.Role = hasSet
		for _, b := range cfg.Boxes {
			if b.Namespace == name {
				n.Role = true
			}
		}
		n.Busy = first(s.stk().Running(name), s.boxBusy(name), s.bk().Busy(name))
		n.Run = s.stk().Last(name)
		n.State = namespaceState(n)
		for _, w := range n.Workloads {
			if w.Canary {
				continue
			}
			n.CanStart = n.CanStart || w.Replicas == 0
			n.CanStop = n.CanStop || w.Replicas > 0
		}
		n.CanStart = n.CanStart || len(mem.Suspended[name]) > 0
		v.Namespaces = append(v.Namespaces, *n)
	}
	return v
}

func namespaceState(n *NamespaceView) string {
	if n.Run != nil && n.Run.Status == "running" && n.Run.Target == "" {
		return map[string]string{"start": "starting", "stop": "stopping"}[n.Run.Kind]
	}
	count, all := map[string]int{}, 0
	for _, w := range n.Workloads {
		if !w.Canary {
			count[w.State]++
			all++
		}
	}
	switch {
	case count["stopped"] == all:
		return "stopped"
	case count["stopped"]+count["stopping"] == all:
		return "stopping"
	case count["running"] == all:
		return "running"
	case count["running"]+count["starting"] == all:
		return "starting"
	}
	return "partly"
}

// podUsage is metrics-server's view, read at most every 15 s; nil without metrics-server.
func (s *Server) podUsage(ctx context.Context, k kube.Kube) map[string]stack.Use {
	s.mu.Lock()
	use, at := s.usage, s.usageAt
	s.mu.Unlock()
	if time.Since(at) < 15*time.Second {
		return use
	}
	use, err := stack.Usage(ctx, k)
	if err != nil {
		use = nil
	}
	s.mu.Lock()
	s.usage, s.usageAt = use, time.Now()
	s.mu.Unlock()
	return use
}

func (s *Server) rsnap() *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := s.snaps["running"]
	if sn == nil {
		sn = newSnapshot(s.runningView, 2*time.Second, 15*time.Second)
		s.snaps["running"] = sn
		go sn.loop()
	}
	return sn
}

func (s *Server) runningState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.rsnap().get())
}

// runningClick starts or stops a namespace (n), or one workload of it (w: deployment/<name> or
// statefulset/<name>).
func (s *Server) runningClick(w http.ResponseWriter, r *http.Request, kind string) {
	q := r.URL.Query()
	ns, target := q.Get("n"), q.Get("w")
	what := ns
	if target != "" {
		what = ns + "/" + target
	}
	cfg := s.store.Get()
	var err error
	switch {
	case !kube.ValidLabel(ns) || stack.Hidden(ns, cfg.RunningHide):
		err = badRequest("not a namespace switchyard starts or stops")
	case target != "" && !validTarget(target):
		err = badRequest("not a Deployment or StatefulSet")
	case kind == "stop" && target == "" && s.production(ns) && q.Get("typed") != ns:
		err = badRequest("a production namespace: type its name in to stop it")
	case kind == "start":
		err = s.stk().Start(ns, target, s.by(r))
	default:
		err = s.stk().Stop(ns, target, s.by(r))
	}
	s.audit(r, cfg.User, kind, what, "", err)
	if err != nil {
		fail(w, err)
		return
	}
	s.rsnap().refresh()
	w.WriteHeader(http.StatusNoContent)
}

func validTarget(t string) bool {
	kind, name, ok := strings.Cut(t, "/")
	return ok && (kind == "deployment" || kind == "statefulset") && kube.ValidName(name)
}

func (s *Server) production(ns string) bool {
	set, ok := s.store.Backup(ns)
	return ok && set.Production
}

// runningEnable writes the ClusterRole the page needs, with the admin context.
func (s *Server) runningEnable(w http.ResponseWriter, r *http.Request) {
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	err := writeRunningRBAC(ctx, k)
	s.audit(r, s.store.Get().User, "running-enable", "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	s.rsnap().refresh()
	w.WriteHeader(http.StatusNoContent)
}
