package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/deploy"
	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/registry"
)

const pullSecret = "switchyard-pull"

// admin is the admin context, needed to add, change or remove a box: it writes the Role in the
// box's namespace.
func (s *Server) admin() (kube.Kube, error) {
	cfg := s.store.Get()
	k := kube.Kube{Bin: cfg.Kubectl, Kubeconfig: cfg.AdminKubeconfig, Context: cfg.AdminContext}
	if k.Kubeconfig != "" {
		if _, err := os.Stat(k.Kubeconfig); err != nil {
			return k, fmt.Errorf("the admin kubeconfig %s is not here: adding, changing or removing a box needs the admin context %s once", k.Kubeconfig, k.Context)
		}
	}
	return k, nil
}

func (s *Server) adminCtx(w http.ResponseWriter, r *http.Request) (kube.Kube, context.Context, context.CancelFunc, bool) {
	k, err := s.admin()
	if err != nil {
		fail(w, badRequest(err.Error()))
		return k, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	return k, ctx, cancel, true
}

func names(items []struct{ Metadata kube.Meta }) []string {
	var n []string
	for _, i := range items {
		n = append(n, i.Metadata.Name)
	}
	slices.Sort(n)
	return n
}

func (s *Server) adminNamespaces(w http.ResponseWriter, r *http.Request) {
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	var list struct {
		Items []struct{ Metadata kube.Meta }
	}
	if err := k.Get(ctx, &list, "get", "namespaces"); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, names(list.Items))
}

func (s *Server) adminDeployments(w http.ResponseWriter, r *http.Request) {
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	ns := r.URL.Query().Get("ns")
	if err := checkName("namespace", ns, true); err != nil {
		fail(w, err)
		return
	}
	var list struct{ Items []kube.Deployment }
	if err := k.Get(ctx, &list, "--namespace="+ns, "get", "deployments"); err != nil {
		fail(w, err)
		return
	}
	var out []string
	for _, d := range list.Items {
		if d.Spec.Template.Metadata.Labels["switchyard/track"] == "" {
			out = append(out, d.Metadata.Name)
		}
	}
	slices.Sort(out)
	writeJSON(w, out)
}

type details struct {
	Containers  []kube.Container
	Image       string
	Registry    string
	Followers   []string // CronJobs and Deployments running the same repository
	PullSecrets []string
	HasCreds    bool
}

// adminDetails: what the "more" part of the form offers for a Deployment.
func (s *Server) adminDetails(w http.ResponseWriter, r *http.Request) {
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	ns, name, cname := r.URL.Query().Get("ns"), r.URL.Query().Get("d"), r.URL.Query().Get("c")
	if err := checkNames(ns, name, cname); err != nil {
		fail(w, err)
		return
	}
	var d kube.Deployment
	if err := k.Get(ctx, &d, "--namespace="+ns, "get", "--", "deployment", name); err != nil {
		fail(w, err)
		return
	}
	c, ok := d.Spec.Template.Spec.Pick(cname)
	if !ok {
		fail(w, badRequest("no container "+cname))
		return
	}
	ref := registry.Parse(c.Image)
	res := details{Containers: d.Spec.Template.Spec.Containers, Image: c.Image, Registry: ref.Registry}
	same := func(spec kube.PodSpec) bool {
		return slices.ContainsFunc(spec.Containers, func(x kube.Container) bool {
			p := registry.Parse(x.Image)
			return p.Registry == ref.Registry && p.Repo == ref.Repo
		})
	}
	var cjs struct{ Items []kube.CronJob }
	if k.Get(ctx, &cjs, "--namespace="+ns, "get", "cronjobs") == nil {
		for _, cj := range cjs.Items {
			if same(cj.Spec.JobTemplate.Spec.Template.Spec) {
				res.Followers = append(res.Followers, "cronjob/"+cj.Metadata.Name)
			}
		}
	}
	var deps struct{ Items []kube.Deployment }
	if k.Get(ctx, &deps, "--namespace="+ns, "get", "deployments") == nil {
		for _, o := range deps.Items {
			if o.Metadata.Name != name && o.Spec.Template.Metadata.Labels["switchyard/track"] == "" && same(o.Spec.Template.Spec) {
				res.Followers = append(res.Followers, "deployment/"+o.Metadata.Name)
			}
		}
	}
	res.PullSecrets, _ = deploy.PullSecrets(ctx, k, ns, name)
	for _, n := range res.PullSecrets {
		var sec struct {
			Data map[string][]byte `json:"data"`
		}
		if k.Get(ctx, &sec, "--namespace="+ns, "get", "--", "secret", n) == nil {
			if _, _, ok := registry.Creds(sec.Data[".dockerconfigjson"], ref.Registry); ok {
				res.HasCreds = true
			}
		}
	}
	writeJSON(w, res)
}

func (s *Server) adminBox(w http.ResponseWriter, r *http.Request) {
	b, ok := s.store.Box(r.URL.Query().Get("b"))
	if !ok {
		fail(w, badRequest("no such box"))
		return
	}
	writeJSON(w, b)
}

type boxForm struct {
	Box           config.Box
	Original      string // the box's ID when it is changed, "" for a new one
	RegistryUser  string
	RegistryToken string
}

// adminSave adds or changes a box: the pull secret if one was given, the Role, the config.
func (s *Server) adminSave(w http.ResponseWriter, r *http.Request) {
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	var f boxForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	b := f.Box
	b.Namespace, b.Deployment = strings.TrimSpace(b.Namespace), strings.TrimSpace(b.Deployment)
	if b.Namespace == "" || b.Deployment == "" {
		fail(w, badRequest("pick a namespace and a Deployment"))
		return
	}
	if err := checkNames(b.Namespace, b.Deployment, b.Container); err != nil {
		fail(w, err)
		return
	}
	for _, f := range b.Followers {
		kind, name, _ := strings.Cut(f, "/")
		if kind != "cronjob" && kind != "deployment" || !kube.ValidName(name) {
			fail(w, badRequest("a follower is cronjob/<name> or deployment/<name>, not "+f))
			return
		}
	}
	if f.Original != "" {
		if _, ok := s.store.Box(f.Original); !ok {
			fail(w, badRequest("no such box "+f.Original))
			return
		}
	}
	if b.Site != "" {
		if u, err := url.Parse(b.Site); err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			fail(w, badRequest("the site is an http or https URL, not "+b.Site))
			return
		}
	}
	if _, err := config.ParseLinks(b.Links); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	for _, p := range b.Pages {
		if !strings.HasPrefix(p, "/") {
			fail(w, badRequest("a page is a path starting with /: "+p))
			return
		}
	}
	if _, exists := s.store.Box(b.ID()); exists && f.Original != b.ID() {
		fail(w, badRequest(b.ID()+" is a box already"))
		return
	}
	var d kube.Deployment
	if err := k.Get(ctx, &d, "--namespace="+b.Namespace, "get", "--", "deployment", b.Deployment); err != nil {
		fail(w, err)
		return
	}
	c, ok := d.Spec.Template.Spec.Pick(b.Container)
	if !ok {
		fail(w, badRequest("no container "+b.Container))
		return
	}
	if f.RegistryToken != "" {
		if err := addPullSecret(ctx, k, b.Namespace, d.Spec.Template.Spec.ServiceAccountName, registry.Parse(c.Image).Registry, f.RegistryUser, f.RegistryToken); err != nil {
			fail(w, err)
			return
		}
	}
	err := s.store.Update(func(cfg *config.Config) error {
		cfg.Boxes = slices.DeleteFunc(cfg.Boxes, func(x config.Box) bool { return x.ID() == f.Original || x.ID() == b.ID() })
		cfg.Boxes = append(cfg.Boxes, b)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	nss := []string{b.Namespace}
	if old, _, _ := strings.Cut(f.Original, "/"); old != "" && old != b.Namespace {
		nss = append(nss, old)
	}
	for _, ns := range nss {
		if err := s.writeRole(ctx, k, ns); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, map[string]string{"Box": b.ID()})
}

func (s *Server) adminRemove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("b")
	b, ok := s.store.Box(id)
	if !ok {
		fail(w, badRequest("no such box"))
		return
	}
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	if cur, _ := s.eng().Runner(b).Current(); cur != nil {
		fail(w, deploy.Refused("a "+cur.Kind+" is running"))
		return
	}
	err := s.store.Update(func(cfg *config.Config) error {
		cfg.Boxes = slices.DeleteFunc(cfg.Boxes, func(x config.Box) bool { return x.ID() == id })
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.writeRole(ctx, k, b.Namespace); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addPullSecret stores the registry login as the Secret switchyard-pull and adds it to the
// service account's pull secrets, so new pods pull with it and switchyard reads the registry.
func addPullSecret(ctx context.Context, k kube.Kube, ns, sa, reg, user, token string) error {
	if user == "" {
		user = "switchyard"
	}
	server := reg
	if reg == "docker.io" {
		server = "https://index.docker.io/v1/"
	}
	auths, _ := json.Marshal(map[string]any{"auths": map[string]any{server: map[string]string{
		"username": user, "password": token, "auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + token))}}})
	err := k.Apply(ctx, map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata":   map[string]any{"name": pullSecret, "namespace": ns, "labels": map[string]string{"app.kubernetes.io/managed-by": "switchyard"}},
		"stringData": map[string]string{".dockerconfigjson": string(auths)}})
	if err != nil {
		return err
	}
	if sa == "" {
		sa = "default"
	}
	var acc struct {
		ImagePullSecrets []map[string]string `json:"imagePullSecrets"`
	}
	if err := k.Get(ctx, &acc, "--namespace="+ns, "get", "--", "serviceaccount", sa); err != nil {
		return err
	}
	if slices.ContainsFunc(acc.ImagePullSecrets, func(x map[string]string) bool { return x["name"] == pullSecret }) {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{"imagePullSecrets": append(acc.ImagePullSecrets, map[string]string{"name": pullSecret})})
	_, err = k.Run(ctx, nil, "--namespace="+ns, "patch", "--type=merge", "-p", string(patch), "--", "serviceaccount", sa)
	return err
}

// checkName refuses a name Kubernetes would not allow, before it reaches kubectl.
func checkName(what, s string, label bool) error {
	if label && !kube.ValidLabel(s) || !label && !kube.ValidName(s) {
		return badRequest(fmt.Sprintf("%q is not a valid %s name", s, what))
	}
	return nil
}

// checkNames: a namespace, a Deployment and a container ("" for the first one).
func checkNames(ns, deployment, container string) error {
	err := errors.Join(checkName("namespace", ns, true), checkName("Deployment", deployment, false))
	if container != "" {
		err = errors.Join(err, checkName("container", container, true))
	}
	return err
}

// writeRole gives switchyard's service account what the namespace's boxes need, nothing else;
// with no box left it goes.
func (s *Server) writeRole(ctx context.Context, k kube.Kube, ns string) error {
	var states, secrets, accounts []string
	for _, b := range s.store.Get().Boxes {
		if b.Namespace != ns {
			continue
		}
		states = append(states, b.State())
		names, err := deploy.PullSecrets(ctx, k, ns, b.Deployment)
		if err != nil {
			return err
		}
		secrets = append(secrets, names...)
		var d kube.Deployment
		if k.Get(ctx, &d, "--namespace="+ns, "get", "--", "deployment", b.Deployment) == nil {
			sa := d.Spec.Template.Spec.ServiceAccountName
			if sa == "" {
				sa = "default"
			}
			accounts = append(accounts, sa)
		}
	}
	if len(states) == 0 {
		_, err := k.Run(ctx, nil, "--namespace="+ns, "delete", "--ignore-not-found", "--", "rolebinding,role", accountName)
		return err
	}
	slices.Sort(secrets)
	slices.Sort(accounts)
	rule := func(group string, res []string, verbs []string, names ...string) map[string]any {
		m := map[string]any{"apiGroups": []string{group}, "resources": res, "verbs": verbs}
		if len(names) > 0 {
			m["resourceNames"] = names
		}
		return m
	}
	rules := []any{
		rule("apps", []string{"deployments"}, []string{"get", "list", "watch", "create", "patch", "update", "delete"}),
		rule("apps", []string{"deployments/scale"}, []string{"get", "patch", "update"}),
		rule("apps", []string{"replicasets"}, []string{"get", "list", "watch"}),
		rule("", []string{"pods"}, []string{"get", "list", "watch"}),
		rule("", []string{"pods/log"}, []string{"get"}),
		rule("", []string{"pods/portforward"}, []string{"get", "create"}),
		rule("batch", []string{"jobs"}, []string{"get", "list", "watch", "create", "delete"}),
		rule("batch", []string{"cronjobs"}, []string{"get", "list", "patch"}),
		rule("", []string{"configmaps"}, []string{"create"}),
		rule("", []string{"configmaps"}, []string{"get", "patch", "update"}, states...),
	}
	if len(secrets) > 0 {
		rules = append(rules, rule("", []string{"secrets"}, []string{"get"}, slices.Compact(secrets)...))
	}
	if len(accounts) > 0 {
		rules = append(rules, rule("", []string{"serviceaccounts"}, []string{"get"}, slices.Compact(accounts)...))
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "switchyard"}
	if err := k.Apply(ctx, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": accountName, "namespace": ns, "labels": labels}, "rules": rules}); err != nil {
		return err
	}
	return k.Apply(ctx, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": accountName, "namespace": ns, "labels": labels},
		"roleRef":  map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": accountName},
		"subjects": []any{map[string]string{"kind": "ServiceAccount", "name": accountName, "namespace": accountNS}}})
}
