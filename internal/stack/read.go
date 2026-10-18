package stack

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/xuedi/switchyard/internal/kube"
)

// Workload is a Deployment or a StatefulSet with its pods.
type Workload struct {
	Kind      string // Deployment or StatefulSet
	Namespace string
	Name      string
	Replicas  int // wanted
	Ready     int
	Image     string
	Canary    bool
	Labels    map[string]string // the pod template's
	HostPorts []int
	Pods      []kube.Pod
}

func (w Workload) Obj() string { return strings.ToLower(w.Kind) + "/" + w.Name }

// State is running, starting, stopping or stopped.
func (w Workload) State() string {
	switch {
	case w.Replicas == 0 && len(w.Pods) == 0:
		return "stopped"
	case w.Replicas == 0:
		return "stopping"
	case w.Ready >= w.Replicas:
		return "running"
	}
	return "starting"
}

type CronJob struct {
	Namespace    string
	Name         string
	Schedule     string
	TimeZone     string
	Suspend      bool
	Active       int
	LastSchedule string
}

type Service struct {
	Namespace string
	Name      string
	Type      string
	Selector  map[string]string
	Ports     []struct{ Port, NodePort int }
	Ingress   []string // a LoadBalancer's addresses
}

// Cluster is what one read found, in one namespace or all of them.
type Cluster struct {
	Workloads []Workload
	CronJobs  []CronJob
	Services  []Service
	HostIP    string // a node's address, for the NodePorts
}

// Read lists the workloads, their pods, the CronJobs and the Services, in one call; ns "" reads
// every namespace.
func Read(ctx context.Context, k kube.Kube, ns string) (Cluster, error) {
	args := []string{"get", "--all-namespaces", "deployments,statefulsets,cronjobs,pods,services"}
	if ns != "" {
		args = []string{"--namespace=" + ns, "get", "deployments,statefulsets,cronjobs,pods,services"}
	}
	var list struct{ Items []json.RawMessage }
	var c Cluster
	if err := k.Get(ctx, &list, args...); err != nil {
		return c, err
	}
	var pods []kube.Pod
	for _, raw := range list.Items {
		var head struct{ Kind string }
		json.Unmarshal(raw, &head)
		switch head.Kind {
		case "Deployment", "StatefulSet":
			var d kube.Deployment
			json.Unmarshal(raw, &d)
			w := Workload{Kind: head.Kind, Namespace: d.Metadata.Namespace, Name: d.Metadata.Name, Replicas: d.Replicas(),
				Ready: d.Status.ReadyReplicas, Labels: d.Spec.Template.Metadata.Labels,
				Canary: d.Spec.Template.Metadata.Labels["switchyard/track"] != ""}
			if ct, ok := d.Spec.Template.Spec.Pick(""); ok {
				w.Image = ct.Image
			}
			for _, ct := range d.Spec.Template.Spec.Containers {
				for _, p := range ct.Ports {
					if p.HostPort > 0 {
						w.HostPorts = append(w.HostPorts, p.HostPort)
					}
				}
			}
			c.Workloads = append(c.Workloads, w)
		case "CronJob":
			var cj struct {
				Metadata kube.Meta
				Spec     struct {
					Schedule string
					TimeZone string
					Suspend  bool
				}
				Status struct {
					Active           []json.RawMessage
					LastScheduleTime string
				}
			}
			json.Unmarshal(raw, &cj)
			c.CronJobs = append(c.CronJobs, CronJob{Namespace: cj.Metadata.Namespace, Name: cj.Metadata.Name, Schedule: cj.Spec.Schedule,
				TimeZone: cj.Spec.TimeZone, Suspend: cj.Spec.Suspend, Active: len(cj.Status.Active), LastSchedule: cj.Status.LastScheduleTime})
		case "Pod":
			var p kube.Pod
			json.Unmarshal(raw, &p)
			pods = append(pods, p)
			if c.HostIP == "" {
				c.HostIP = p.Status.HostIP
			}
		case "Service":
			var sv struct {
				Metadata kube.Meta
				Spec     struct {
					Type     string
					Selector map[string]string
					Ports    []struct{ Port, NodePort int }
				}
				Status struct {
					LoadBalancer struct {
						Ingress []struct{ IP, Hostname string }
					}
				}
			}
			json.Unmarshal(raw, &sv)
			s := Service{Namespace: sv.Metadata.Namespace, Name: sv.Metadata.Name, Type: sv.Spec.Type, Selector: sv.Spec.Selector, Ports: sv.Spec.Ports}
			for _, in := range sv.Status.LoadBalancer.Ingress {
				s.Ingress = append(s.Ingress, in.IP+in.Hostname)
			}
			c.Services = append(c.Services, s)
		}
	}
	index := map[string]int{}
	for i, w := range c.Workloads {
		index[w.Namespace+"/"+w.Obj()] = i
	}
	for _, p := range pods {
		for _, o := range p.Metadata.OwnerReferences {
			obj := ""
			switch o.Kind {
			case "StatefulSet":
				obj = "statefulset/" + o.Name
			case "ReplicaSet":
				if hash := p.Metadata.Labels["pod-template-hash"]; hash != "" {
					obj = "deployment/" + strings.TrimSuffix(o.Name, "-"+hash)
				}
			}
			if i, ok := index[p.Metadata.Namespace+"/"+obj]; ok {
				c.Workloads[i].Pods = append(c.Workloads[i].Pods, p)
			}
		}
	}
	slices.SortFunc(c.Workloads, func(a, b Workload) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return c, nil
}

// Addresses: where the workload is reached from outside, its pods' host ports and the
// NodePorts and LoadBalancers of the Services that select it.
func (c Cluster) Addresses(w Workload) []string {
	var out []string
	for _, p := range w.Pods {
		for _, ct := range p.Spec.Containers {
			for _, port := range ct.Ports {
				if port.HostPort > 0 && p.Status.HostIP != "" {
					out = append(out, p.Status.HostIP+":"+strconv.Itoa(port.HostPort))
				}
			}
		}
	}
	if len(w.Pods) == 0 {
		for _, port := range w.HostPorts {
			out = append(out, ":"+strconv.Itoa(port))
		}
	}
	for _, s := range c.Services {
		if s.Namespace != w.Namespace || len(s.Selector) == 0 || !selects(s.Selector, w.Labels) {
			continue
		}
		for _, port := range s.Ports {
			switch {
			case s.Type == "LoadBalancer" && len(s.Ingress) > 0:
				for _, in := range s.Ingress {
					out = append(out, in+":"+strconv.Itoa(port.Port))
				}
			case (s.Type == "NodePort" || s.Type == "LoadBalancer") && port.NodePort > 0:
				out = append(out, c.HostIP+":"+strconv.Itoa(port.NodePort))
			}
		}
	}
	return slices.Compact(out)
}

func selects(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Use is a pod's CPU (millicores) and memory (bytes).
type Use struct{ CPU, Mem int64 }

// Usage reads every pod's usage from metrics-server, by namespace/pod.
func Usage(ctx context.Context, k kube.Kube) (map[string]Use, error) {
	out, err := k.Run(ctx, nil, "get", "--raw", "/apis/metrics.k8s.io/v1beta1/pods")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata   kube.Meta
			Containers []struct {
				Usage struct{ CPU, Memory string }
			}
		}
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	use := map[string]Use{}
	for _, p := range list.Items {
		var u Use
		for _, c := range p.Containers {
			u.CPU += milli(c.Usage.CPU)
			u.Mem += bytes(c.Usage.Memory)
		}
		use[p.Metadata.Namespace+"/"+p.Metadata.Name] = u
	}
	return use, nil
}

// milli reads a CPU quantity: 250m, 12345678n, 2.
func milli(q string) int64 {
	for suffix, div := range map[string]float64{"n": 1e6, "u": 1e3, "m": 1} {
		if n, ok := strings.CutSuffix(q, suffix); ok {
			f, _ := strconv.ParseFloat(n, 64)
			return int64(f / div)
		}
	}
	f, _ := strconv.ParseFloat(q, 64)
	return int64(f * 1000)
}

// bytes reads a memory quantity: 123456Ki, 1Gi, 500M, 1024.
func bytes(q string) int64 {
	for _, u := range []struct {
		suffix string
		mul    float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}} {
		if n, ok := strings.CutSuffix(q, u.suffix); ok {
			f, _ := strconv.ParseFloat(n, 64)
			return int64(f * u.mul)
		}
	}
	f, _ := strconv.ParseFloat(q, 64)
	return int64(f)
}

// Hidden namespaces are never listed nor switched: the cluster's own and switchyard's.
func Hidden(ns string, extra []string) bool {
	return strings.HasPrefix(ns, "kube-") || strings.HasSuffix(ns, "-system") || ns == "k0s-autopilot" ||
		ns == "local-path-storage" || ns == Home || slices.Contains(extra, ns)
}
