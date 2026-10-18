package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/auth"
	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/stack"
	"net/http"
)

// account is where switchyard's own service account lives; each box's namespace gets a Role
// bound to it.
const (
	accountNS   = "switchyard"
	accountName = "switchyard"
)

func defaultKubeconfig() string {
	if k := os.Getenv("KUBECONFIG"); k != "" {
		return strings.Split(k, string(os.PathListSeparator))[0]
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

type checkResult struct {
	Kubectl, KubectlVersion, KubectlError string
	Kubeconfig, KubeconfigError           string
	Contexts                              []string
	Current                               string
	Listen                                string
}

// setupCheck: is there a kubectl, a kubeconfig, which contexts.
func (s *Server) setupCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := checkResult{Kubeconfig: s.kubeconfig, Listen: s.listen}
	bin, err := kube.Find()
	if err != nil {
		res.KubectlError = err.Error() + ". Install it (https://kubernetes.io/docs/tasks/tools/), then reload this page."
		writeJSON(w, res)
		return
	}
	res.Kubectl = bin
	k := kube.Kube{Bin: bin, Kubeconfig: res.Kubeconfig}
	var v struct {
		ClientVersion struct{ GitVersion string } `json:"clientVersion"`
	}
	if out, err := k.Run(ctx, nil, "version", "--client", "-o", "json"); err == nil {
		json.Unmarshal(out, &v)
		res.KubectlVersion = v.ClientVersion.GitVersion
	}
	if _, err := os.Stat(res.Kubeconfig); err != nil {
		res.KubeconfigError = "no kubeconfig at " + res.Kubeconfig + ": copy the cluster's admin kubeconfig there, or name another one with switchyard setup -kubeconfig <file>"
		writeJSON(w, res)
		return
	}
	res.Contexts, err = contexts(ctx, k)
	if err != nil {
		res.KubeconfigError = err.Error()
		writeJSON(w, res)
		return
	}
	cur, _ := k.Run(ctx, nil, "config", "current-context")
	res.Current = strings.TrimSpace(string(cur))
	writeJSON(w, res)
}

type setupForm struct {
	Kubeconfig, Context string
	User, Password      string
	Listen              string
}

func contexts(ctx context.Context, k kube.Kube) ([]string, error) {
	out, err := k.Run(ctx, nil, "config", "get-contexts", "-o", "name")
	return strings.Fields(string(out)), err
}

// form reads the installer's form; the kubeconfig is always the one switchyard was started with.
func (s *Server) form(r *http.Request) setupForm {
	var f setupForm
	json.NewDecoder(r.Body).Decode(&f)
	f.Kubeconfig = s.kubeconfig
	return f
}

// setupTest: the context answers, and may create what switchyard needs.
func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	f := s.form(r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg, err := testAdmin(ctx, f)
	if err != nil {
		writeJSON(w, map[string]any{"OK": false, "Message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"OK": true, "Message": msg})
}

func adminKube(f setupForm) (kube.Kube, error) {
	bin, err := kube.Find()
	return kube.Kube{Bin: bin, Kubeconfig: f.Kubeconfig, Context: f.Context}, err
}

func testAdmin(ctx context.Context, f setupForm) (string, error) {
	k, err := adminKube(f)
	if err != nil {
		return "", err
	}
	if list, err := contexts(ctx, k); err != nil || !slices.Contains(list, f.Context) {
		return "", fmt.Errorf("%s has no context %q", f.Kubeconfig, f.Context)
	}
	out, err := k.Run(ctx, nil, "get", "namespaces", "-o", "name")
	if err != nil {
		return "", err
	}
	for _, what := range [][]string{{"create", "namespaces"}, {"create", "roles", "--all-namespaces"}, {"create", "rolebindings", "--all-namespaces"},
		{"create", "clusterroles"}, {"create", "clusterrolebindings"}} {
		ans, _ := k.Run(ctx, nil, append([]string{"auth", "can-i"}, what...)...)
		if strings.TrimSpace(string(ans)) != "yes" {
			return "", fmt.Errorf("context %s may not %s: switchyard needs an admin context for the setup", f.Context, strings.Join(what, " "))
		}
	}
	return fmt.Sprintf("context %s answers, %d namespaces, and may set switchyard up", f.Context, len(strings.Fields(string(out)))), nil
}

// setupRun does the setup: switchyard's service account and its token in the namespace
// switchyard, the scoped kubeconfig, config.json, the systemd unit.
func (s *Server) setupRun(w http.ResponseWriter, r *http.Request) {
	f := s.form(r)
	f.User = strings.TrimSpace(f.User)
	if f.User == "" || len(f.Password) < 8 {
		fail(w, badRequest("a user name and a password of at least 8 characters, please"))
		return
	}
	if f.Listen == "" {
		f.Listen = s.listen
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if _, err := testAdmin(ctx, f); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	admin, _ := adminKube(f)
	cluster, err := writeKubeconfig(ctx, admin)
	if err == nil {
		err = writeRunningRBAC(ctx, admin)
	}
	if err != nil {
		fail(w, err)
		return
	}
	hash, err := auth.Hash(f.Password)
	if err != nil {
		fail(w, err)
		return
	}
	err = s.store.Update(func(c *config.Config) error {
		*c = config.Config{Listen: f.Listen, User: f.User, Hash: hash, Secret: auth.NewSecret(), Kubectl: admin.Bin,
			AdminKubeconfig: f.Kubeconfig, AdminContext: f.Context, Cluster: cluster}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	unit, err := writeUnit()
	if err != nil {
		fail(w, err)
		return
	}
	cfg := s.store.Get()
	s.audit(r, cfg.User, "setup", "", "", nil)
	s.start(cfg)
	s.newSession(w, r, cfg)
	writeJSON(w, map[string]any{"Unit": unit, "Systemd": os.Getenv("INVOCATION_ID") != "", "Listen": f.Listen,
		"Commands": []string{"systemctl --user daemon-reload", "systemctl --user enable --now switchyard", "loginctl enable-linger " + os.Getenv("USER")}})
}

// writeKubeconfig creates switchyard's service account with a token and writes a kubeconfig
// that uses only that token. Returns the cluster's name.
func writeKubeconfig(ctx context.Context, admin kube.Kube) (string, error) {
	objs := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": accountNS}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": accountName, "namespace": accountNS}},
		{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/service-account-token",
			"metadata": map[string]any{"name": accountName + "-token", "namespace": accountNS,
				"annotations": map[string]string{"kubernetes.io/service-account.name": accountName}}},
	}
	for _, o := range objs {
		if err := admin.Apply(ctx, o); err != nil {
			return "", err
		}
	}
	var token string
	for i := 0; i < 20 && token == ""; i++ {
		var sec struct {
			Data map[string][]byte `json:"data"`
		}
		if err := admin.Get(ctx, &sec, "--namespace="+accountNS, "get", "--", "secret", accountName+"-token"); err != nil {
			return "", err
		}
		token = string(sec.Data["token"])
		if token == "" {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if token == "" {
		return "", errors.New("the token of the service account switchyard did not appear")
	}
	var view struct {
		Clusters []struct {
			Name    string         `json:"name"`
			Cluster map[string]any `json:"cluster"`
		} `json:"clusters"`
	}
	if err := admin.Get(ctx, &view, "config", "view", "--raw", "--minify", "--flatten"); err != nil {
		return "", err
	}
	if len(view.Clusters) != 1 {
		return "", errors.New("the admin context names no cluster")
	}
	cl := view.Clusters[0]
	kc := map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": "switchyard",
		"clusters": []any{map[string]any{"name": cl.Name, "cluster": cl.Cluster}},
		"users":    []any{map[string]any{"name": "switchyard", "user": map[string]any{"token": token}}},
		"contexts": []any{map[string]any{"name": "switchyard", "context": map[string]any{"cluster": cl.Name, "user": "switchyard"}}},
	}
	return cl.Name, config.WriteFile(config.Kubeconfig(), kc)
}

// writeRunningRBAC lets switchyard's service account read the workloads of every namespace and
// scale them, through the scale subresource only, and keep what Start brings back in a
// ConfigMap of its own namespace.
func writeRunningRBAC(ctx context.Context, admin kube.Kube) error {
	rule := func(group string, res []string, verbs []string, names ...string) map[string]any {
		m := map[string]any{"apiGroups": []string{group}, "resources": res, "verbs": verbs}
		if len(names) > 0 {
			m["resourceNames"] = names
		}
		return m
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "switchyard"}
	read := []string{"get", "list"}
	subjects := []any{map[string]string{"kind": "ServiceAccount", "name": accountName, "namespace": accountNS}}
	objs := []map[string]any{
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": stack.MemoryName, "labels": labels},
			"rules": []any{
				rule("", []string{"pods", "services"}, read),
				rule("apps", []string{"deployments", "statefulsets"}, read),
				rule("batch", []string{"cronjobs"}, read),
				rule("metrics.k8s.io", []string{"pods"}, read),
				rule("apps", []string{"deployments/scale", "statefulsets/scale"}, []string{"get", "patch", "update"}),
			}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": stack.MemoryName, "labels": labels},
			"roleRef":  map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": stack.MemoryName},
			"subjects": subjects},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": map[string]any{"name": stack.MemoryName, "namespace": accountNS, "labels": labels},
			"rules": []any{
				rule("", []string{"configmaps"}, []string{"create"}),
				rule("", []string{"configmaps"}, []string{"get", "patch", "update"}, stack.MemoryName),
			}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": map[string]any{"name": stack.MemoryName, "namespace": accountNS, "labels": labels},
			"roleRef":  map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": stack.MemoryName},
			"subjects": subjects},
	}
	for _, o := range objs {
		if err := admin.Apply(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

func writeUnit() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	d, _ := os.UserConfigDir()
	path := filepath.Join(d, "systemd", "user", "switchyard.service")
	unit := fmt.Sprintf(`[Unit]
Description=switchyard: canary deploys and rollbacks of image tags
After=network-online.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(unit), 0o644)
}
