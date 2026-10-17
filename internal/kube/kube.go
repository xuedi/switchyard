// Package kube runs kubectl: JSON in and out, one kubeconfig and context per Kube. switchyard
// uses its own scoped kubeconfig for everything but the setup and a box in a new namespace.
package kube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Kube struct {
	Bin        string // kubectl
	Kubeconfig string // "" is kubectl's default
	Context    string // "" is the kubeconfig's current one
}

// Error is kubectl's complaint. Offline: the API server did not answer at all.
type Error struct {
	Msg      string
	Offline  bool
	NotFound bool
}

func (e *Error) Error() string { return e.Msg }

func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.NotFound
}

func IsOffline(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Offline
}

var offline = regexp.MustCompile(`(?i)unable to connect to the server|connection refused|no route to host|i/o timeout|deadline exceeded|tls handshake timeout|network is unreachable|no such host`)

// Find looks for kubectl in PATH and in the places a user service's PATH lacks.
func Find() (string, error) {
	if p, err := exec.LookPath("kubectl"); err == nil {
		return filepath.Abs(p)
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".local/bin/kubectl"), filepath.Join(home, "bin/kubectl"), "/usr/local/bin/kubectl", "/usr/bin/kubectl"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("kubectl not found in PATH, ~/.local/bin, ~/bin or /usr/local/bin")
}

func (k Kube) args(args []string) []string {
	var a []string
	if k.Kubeconfig != "" {
		a = append(a, "--kubeconfig="+k.Kubeconfig)
	}
	if k.Context != "" {
		a = append(a, "--context="+k.Context)
	}
	return append(a, args...)
}

// Run runs kubectl with the args and returns its stdout. Names from outside go after a "--",
// so none of them can become an option.
func (k Kube) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, k.Bin, k.args(append([]string{"--request-timeout=10s"}, args...))...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), &Error{Msg: "kubectl " + strings.Join(args, " ") + ": " + msg,
			Offline: offline.MatchString(msg), NotFound: strings.Contains(msg, "(NotFound)") || strings.Contains(msg, " not found")}
	}
	return out.Bytes(), nil
}

// Get runs kubectl with -o json and decodes the answer into out.
func (k Kube) Get(ctx context.Context, out any, args ...string) error {
	i := slices.Index(args, "--")
	if i < 0 {
		i = len(args)
	}
	b, err := k.Run(ctx, nil, slices.Concat(args[:i], []string{"-o", "json"}, args[i:])...)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// Apply applies one object, given as anything that marshals to JSON.
func (k Kube) Apply(ctx context.Context, obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = k.Run(ctx, b, "apply", "-f", "-")
	return err
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// PortForward forwards a free local port to the pod's port until ctx ends and returns
// 127.0.0.1:<port>. It goes through the kubelet, so a NetworkPolicy in front of the pod does
// not get in the way.
func (k Kube) PortForward(ctx context.Context, ns, pod string, port int) (string, error) {
	cmd := exec.CommandContext(ctx, k.Bin, k.args([]string{"--namespace=" + ns, "port-forward", "--address=127.0.0.1", "--", "pod/" + pod, fmt.Sprintf(":%d", port)})...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return "", err
	}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwarding.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- "127.0.0.1:" + m[1]:
				default:
				}
			}
		}
		cmd.Wait()
	}()
	select {
	case addr := <-found:
		return addr, nil
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		return "", fmt.Errorf("port-forward to %s: %s", pod, strings.TrimSpace(errb.String()))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidLabel: a DNS-1123 label, what a namespace or a container may be called.
func ValidLabel(s string) bool { return len(s) <= 63 && label.MatchString(s) }

// ValidName: a DNS-1123 subdomain, what a Deployment, a CronJob or a ConfigMap may be called.
func ValidName(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if !ValidLabel(p) {
			return false
		}
	}
	return true
}

// --- the parts of the objects switchyard reads ---

type Meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences,omitempty"`
}

type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type PodSpec struct {
	Containers         []Container             `json:"containers"`
	ImagePullSecrets   []struct{ Name string } `json:"imagePullSecrets"`
	ServiceAccountName string                  `json:"serviceAccountName"`
}

// Pick returns the container with that name, or the first one for "".
func (s PodSpec) Pick(name string) (Container, bool) {
	for _, c := range s.Containers {
		if name == "" || c.Name == name {
			return c, true
		}
	}
	return Container{}, false
}

type Deployment struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Metadata Meta    `json:"metadata"`
			Spec     PodSpec `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int `json:"replicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

func (d Deployment) Replicas() int {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

type CronJob struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		JobTemplate struct {
			Spec struct {
				Template struct {
					Spec PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

type Pod struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
		InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type ContainerStatus struct {
	RestartCount int `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
}

func (p Pod) Ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func (p Pod) Restarts() int {
	n := 0
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

// Waiting is why a container does not start, e.g. ImagePullBackOff, "" when none waits.
func (p Pod) Waiting() string {
	for _, c := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if w := c.State.Waiting; w != nil && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			return strings.TrimSpace(w.Reason + " " + w.Message)
		}
	}
	return ""
}

type Job struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
		Conditions     []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// Phase is running, complete or failed.
func (j Job) Phase() string {
	for _, c := range j.Status.Conditions {
		if c.Status == "True" && (c.Type == "Complete" || c.Type == "Failed") {
			return strings.ToLower(c.Type)
		}
	}
	return "running"
}

// Selector writes labels as a label selector, sorted; extra terms are appended as they are.
func Selector(labels map[string]string, extra ...string) string {
	var s []string
	for k, v := range labels {
		s = append(s, k+"="+v)
	}
	slices.Sort(s)
	return strings.Join(append(s, extra...), ",")
}
