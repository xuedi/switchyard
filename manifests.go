package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// The cluster side: switchyard.sh, the build Job, the router adapters and the manifests,
// rendered from one config file per app by `switchyard manifests --config app.env`.
//
//go:embed kit/*.sh kit/*.awk kit/*.yaml kit/router/*.sh
var kit embed.FS

// appKeys are the keys of an app's config file and their defaults; "" with required set means
// it must be given. Keys starting with BUILD_ENV_ are the build Job's environment.
var appKeys = []struct {
	key, def string
	required bool
}{
	{"APP", "", true},
	{"NAMESPACE", "", true},
	{"STATE", "{APP}-release", false},
	{"JOB_PREFIX", "", false},
	{"JOB_LABEL", "app.kubernetes.io/name=switchyard", false},
	{"IMAGE", "", true},
	{"IMAGE_PULL_POLICY", "IfNotPresent", false},
	{"TZ", "", false},
	{"RUN_AS", "1000:1000", false},
	{"SLOT_A", "{APP}-a", false},
	{"SLOT_B", "{APP}-b", false},
	{"PORT", "8080", false},
	{"CODE_VOLUME", "code", false},
	{"RELEASES_PVC", "{APP}-releases", false},
	{"RELEASES_SIZE", "10Gi", false},
	{"RELEASES_ACCESS", "ReadWriteOnce", false},
	{"STORAGE_CLASS", "", false},
	{"REPLICAS", "2", false},
	{"STEPS", "10 50", false},
	{"PAUSE", "120", false},
	{"DRAIN", "30", false},
	{"KEEP", "5", false},
	{"HOST", "", false},
	{"FORWARDED_PROTO", "", false},
	{"HEALTH", "/", false},
	{"PAGES", "/", false},
	{"FOLLOWERS", "", false},
	{"RELEASE_ENV", "", false},
	{"RELEASE_VALUE", "{id}", false},
	{"ROUTER", "caddy", false},
	{"CADDY_ADMIN", "", false},
	{"CADDY_CONFIGMAP", "", false},
	{"CADDY_KEY", "Caddyfile", false},
	{"CADDY_BLOCK", "{APP}", false},
	{"CADDY_COOKIE", "", false},
	{"BUILD_IMAGE", "{IMAGE}", false},
	{"BUILD_MOUNT", "/app", false},
	{"BUILD_ENV_FROM", "", false},
	{"BUILD_MOUNTS", "", false},
	{"BUILD_CPU", "200m", false},
	{"BUILD_MEMORY", "2Gi", false},
	{"HOOKS", "hooks", false},
}

// hooks an app may have; build is required.
var hookNames = []string{"prepare", "pending-sql", "build", "after-rollback"}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsName  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	keyLine  = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(.*)$`)
)

// parseEnv reads KEY=value lines. Values are taken literally: no expansion, quotes around the
// whole value removed, " #" starts a comment in an unquoted one.
func parseEnv(r io.Reader) (map[string]string, error) {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		g := keyLine.FindStringSubmatch(line)
		if g == nil {
			return nil, fmt.Errorf("line %d: not KEY=value: %s", n, line)
		}
		v := strings.TrimSpace(g[2])
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if _, dup := m[g[1]]; dup {
			return nil, fmt.Errorf("line %d: %s twice", n, g[1])
		}
		m[g[1]] = v
	}
	return m, sc.Err()
}

type nameValue struct{ Name, Value string }

type objRef struct{ Kind, Name, Path string }

// app is an app's config, resolved and checked, as the templates use it.
type app struct {
	env map[string]string // every key, defaults filled in

	App, Namespace, State, Prefix     string
	JobLabelKey, JobLabelValue        string
	Image, BuildImage, PullPolicy     string
	TZ                                string
	UID, GID                          int
	SlotA, SlotB                      string
	ReleasesPVC, ReleasesSize         string
	AccessMode, StorageClass          string
	RouterNamespace, RouterConfigMap  string
	BuildMount, BuildCPU, BuildMemory string
	BuildEnv                          []nameValue
	BuildEnvFrom, BuildMounts         []objRef
	Followers                         []string // kubectl's kind/name
	CronJobFollowers                  []string
	Deployments                       []string // the slots and the followers that are Deployments
	Kinds, CronJobs                   []string
	JobAccount, DashboardAccount      string
	RouterRole, Policy, Scripts       string
	hooks                             map[string][]byte
}

func loadApp(path string) (*app, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := parseEnv(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	a, err := resolveApp(m)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := a.env["HOOKS"]
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(path), dir)
	}
	a.hooks = map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	for _, e := range entries {
		if !slices.Contains(hookNames, e.Name()) {
			return nil, fmt.Errorf("hooks: %s is none of %s", e.Name(), strings.Join(hookNames, ", "))
		}
		if a.hooks[e.Name()], err = os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
			return nil, err
		}
	}
	if a.hooks["build"] == nil {
		return nil, fmt.Errorf("hooks: %s has no build hook", dir)
	}
	return a, nil
}

func resolveApp(m map[string]string) (*app, error) {
	env := map[string]string{}
	known := map[string]bool{}
	for _, k := range appKeys {
		known[k.key] = true
	}
	for k, v := range m {
		if !known[k] && !strings.HasPrefix(k, "BUILD_ENV_") {
			return nil, fmt.Errorf("unknown key %s", k)
		}
		env[k] = v
	}
	for _, k := range appKeys {
		if _, ok := env[k.key]; ok {
			continue
		}
		if k.required {
			return nil, fmt.Errorf("%s is required", k.key)
		}
		def := strings.ReplaceAll(k.def, "{APP}", env["APP"])
		env[k.key] = strings.ReplaceAll(def, "{IMAGE}", env["IMAGE"])
	}

	a := &app{env: env, App: env["APP"], Namespace: env["NAMESPACE"], State: env["STATE"], Prefix: env["JOB_PREFIX"],
		Image: env["IMAGE"], BuildImage: env["BUILD_IMAGE"], PullPolicy: env["IMAGE_PULL_POLICY"], TZ: env["TZ"],
		SlotA: env["SLOT_A"], SlotB: env["SLOT_B"], ReleasesPVC: env["RELEASES_PVC"], ReleasesSize: env["RELEASES_SIZE"],
		AccessMode: env["RELEASES_ACCESS"], StorageClass: env["STORAGE_CLASS"],
		BuildMount: env["BUILD_MOUNT"], BuildCPU: env["BUILD_CPU"], BuildMemory: env["BUILD_MEMORY"],
		Kinds: cronJobs}
	for _, k := range []string{"APP", "NAMESPACE"} {
		if !dnsLabel.MatchString(env[k]) {
			return nil, fmt.Errorf("%s %q is not a DNS label", k, env[k])
		}
	}
	for _, k := range []string{"STATE", "SLOT_A", "SLOT_B", "RELEASES_PVC"} {
		if !dnsName.MatchString(env[k]) {
			return nil, fmt.Errorf("%s %q is not a valid name", k, env[k])
		}
	}
	if a.SlotA == a.SlotB {
		return nil, fmt.Errorf("SLOT_A and SLOT_B are both %s", a.SlotA)
	}
	if a.Prefix != "" && !regexp.MustCompile(`^[a-z0-9][-a-z0-9]*-$`).MatchString(a.Prefix) {
		return nil, fmt.Errorf("JOB_PREFIX %q: lowercase letters, digits and dashes, ending in a dash", a.Prefix)
	}
	var ok bool
	a.JobLabelKey, a.JobLabelValue, ok = strings.Cut(env["JOB_LABEL"], "=")
	if !ok || a.JobLabelKey == "" || a.JobLabelValue == "" || strings.ContainsAny(env["JOB_LABEL"], ", ") ||
		a.JobLabelKey == "app.kubernetes.io/part-of" || a.JobLabelKey == "app.kubernetes.io/component" {
		return nil, fmt.Errorf("JOB_LABEL %q: one key=value, not part-of or component (those go on every Job)", env["JOB_LABEL"])
	}
	if !slices.Contains([]string{"Always", "IfNotPresent", "Never"}, a.PullPolicy) {
		return nil, fmt.Errorf("IMAGE_PULL_POLICY %q", a.PullPolicy)
	}
	if !slices.Contains([]string{"ReadWriteOnce", "ReadWriteMany", "ReadWriteOncePod"}, a.AccessMode) {
		return nil, fmt.Errorf("RELEASES_ACCESS %q", a.AccessMode)
	}
	uid, gid, _ := strings.Cut(env["RUN_AS"], ":")
	if gid == "" {
		gid = uid
	}
	var err1, err2 error
	a.UID, err1 = strconv.Atoi(uid)
	a.GID, err2 = strconv.Atoi(gid)
	if err1 != nil || err2 != nil || a.UID < 1 || a.GID < 1 {
		return nil, fmt.Errorf("RUN_AS %q: uid or uid:gid, not root", env["RUN_AS"])
	}
	for _, k := range []string{"PORT", "REPLICAS", "PAUSE", "DRAIN", "KEEP"} {
		if n, err := strconv.Atoi(env[k]); err != nil || n < 0 || (k != "DRAIN" && n == 0) {
			return nil, fmt.Errorf("%s %q is not a positive number", k, env[k])
		}
	}
	if n, _ := strconv.Atoi(env["KEEP"]); n < 2 {
		return nil, fmt.Errorf("KEEP %s: keep at least 2, or nothing is left to roll back to", env["KEEP"])
	}
	last := 0
	for _, s := range strings.Fields(env["STEPS"]) {
		n, err := strconv.Atoi(s)
		if err != nil || n <= last || n >= 100 {
			return nil, fmt.Errorf("STEPS %q: percentages between 1 and 99, rising", env["STEPS"])
		}
		last = n
	}
	for _, k := range []string{"HEALTH"} {
		if !strings.HasPrefix(env[k], "/") {
			return nil, fmt.Errorf("%s %q must start with /", k, env[k])
		}
	}
	for _, p := range strings.Fields(env["PAGES"]) {
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("PAGES: %q must start with /", p)
		}
	}
	if env["RELEASE_ENV"] != "" && !envName.MatchString(env["RELEASE_ENV"]) {
		return nil, fmt.Errorf("RELEASE_ENV %q is no variable name", env["RELEASE_ENV"])
	}

	a.Deployments = []string{a.SlotA, a.SlotB}
	for _, f := range strings.Fields(env["FOLLOWERS"]) {
		kind, name, _ := strings.Cut(f, "/")
		if !dnsName.MatchString(name) {
			return nil, fmt.Errorf("FOLLOWERS: %q is not cronjob/<name> or deployment/<name>", f)
		}
		switch kind {
		case "cronjob":
			a.CronJobFollowers = append(a.CronJobFollowers, name)
			a.Followers = append(a.Followers, "cronjob/"+name)
		case "deployment", "deploy":
			if name == a.SlotA || name == a.SlotB {
				return nil, fmt.Errorf("FOLLOWERS: %s is a slot", name)
			}
			a.Deployments = append(a.Deployments, name)
			a.Followers = append(a.Followers, "deploy/"+name)
		default:
			return nil, fmt.Errorf("FOLLOWERS: %q is not cronjob/<name> or deployment/<name>", f)
		}
	}
	env["FOLLOWERS"] = strings.Join(a.Followers, " ")

	switch env["ROUTER"] {
	case "caddy":
		ns, name, ok := strings.Cut(env["CADDY_CONFIGMAP"], "/")
		if !ok || !dnsLabel.MatchString(ns) || !dnsName.MatchString(name) {
			return nil, fmt.Errorf("CADDY_CONFIGMAP %q: namespace/name of the ConfigMap with the Caddyfile", env["CADDY_CONFIGMAP"])
		}
		if !strings.HasPrefix(env["CADDY_ADMIN"], "http") {
			return nil, fmt.Errorf("CADDY_ADMIN %q: Caddy's admin API, e.g. http://caddy-admin.edge:2019", env["CADDY_ADMIN"])
		}
		a.RouterNamespace, a.RouterConfigMap = ns, name
	default:
		return nil, fmt.Errorf("ROUTER %q: only caddy so far", env["ROUTER"])
	}

	for _, f := range strings.Fields(env["BUILD_ENV_FROM"]) {
		r, err := parseRef(f, false)
		if err != nil {
			return nil, fmt.Errorf("BUILD_ENV_FROM: %w", err)
		}
		a.BuildEnvFrom = append(a.BuildEnvFrom, r)
	}
	for _, f := range strings.Fields(env["BUILD_MOUNTS"]) {
		r, err := parseRef(f, true)
		if err != nil {
			return nil, fmt.Errorf("BUILD_MOUNTS: %w", err)
		}
		a.BuildMounts = append(a.BuildMounts, r)
	}
	for k, v := range env {
		if name, ok := strings.CutPrefix(k, "BUILD_ENV_"); ok && k != "BUILD_ENV_FROM" {
			a.BuildEnv = append(a.BuildEnv, nameValue{name, v})
		}
	}
	sort.Slice(a.BuildEnv, func(i, j int) bool { return a.BuildEnv[i].Name < a.BuildEnv[j].Name })
	if !strings.HasPrefix(a.BuildMount, "/") || a.BuildMount == "/" || a.BuildMount == "/releases" {
		return nil, fmt.Errorf("BUILD_MOUNT %q: an absolute path, not / or /releases", a.BuildMount)
	}

	for _, kind := range cronJobs {
		a.CronJobs = append(a.CronJobs, a.Prefix+kind)
	}
	a.JobAccount = a.App + "-switchyard"
	a.DashboardAccount = a.App + "-dashboard"
	a.Scripts = a.App + "-switchyard"
	a.RouterRole = "switchyard-" + a.Namespace + "-" + a.App
	a.Policy = "switchyard-" + a.Namespace + "-" + a.App
	return a, nil
}

// parseRef reads configmap/<name> or secret/<name>, with :<path> for a mount.
func parseRef(s string, mount bool) (objRef, error) {
	ref, path, hasPath := strings.Cut(s, ":")
	kind, name, _ := strings.Cut(ref, "/")
	r := objRef{Name: name, Path: path}
	switch kind {
	case "configmap":
		r.Kind = "configMap"
	case "secret":
		r.Kind = "secret"
	}
	if r.Kind == "" || !dnsName.MatchString(name) || hasPath != mount || (mount && !strings.HasPrefix(path, "/")) {
		if mount {
			return r, fmt.Errorf("%q is not configmap/<name>:/path or secret/<name>:/path", s)
		}
		return r, fmt.Errorf("%q is not configmap/<name> or secret/<name>", s)
	}
	return r, nil
}

// shellQuote: 'value', safe to source.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// envFile is switchyard.sh's config: every key but the build Job's, quoted for bash.
func (a *app) envFile() []byte {
	var b bytes.Buffer
	b.WriteString("# rendered by switchyard manifests from the app's config\n")
	var keys []string
	for k := range a.env {
		if !strings.HasPrefix(k, "BUILD_ENV_") && k != "HOOKS" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, shellQuote(a.env[k]))
	}
	return b.Bytes()
}

func quote(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

var funcs = template.FuncMap{
	"q": quote,
	"list": func(l []string) string {
		var q []string
		for _, s := range l {
			q = append(q, quote(s))
		}
		return strings.Join(q, ", ")
	},
	"cel": func(l []string) string {
		var q []string
		for _, s := range l {
			q = append(q, "'"+s+"'")
		}
		return strings.Join(q, ", ")
	},
	"join": strings.Join,
}

func (a *app) render(name string) ([]byte, error) {
	src, err := kit.ReadFile("kit/" + name)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, a); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// yamlBlock writes a file's text as a YAML block scalar, indented.
func yamlBlock(b *bytes.Buffer, text, indent string) {
	chomp := ""
	switch {
	case !strings.HasSuffix(text, "\n"):
		chomp = "-"
	case strings.HasSuffix(text, "\n\n"):
		chomp = "+"
	}
	ind := ""
	if strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\n") {
		ind = strconv.Itoa(len(indent) - 2) // relative to the key, two spaces in
	}
	b.WriteString("|" + ind + chomp + "\n")
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
		} else {
			b.WriteString(indent + line + "\n")
		}
	}
}

// scripts is the ConfigMap the Jobs mount at /switchyard: the kit, the rendered build Job and
// config, the app's hooks.
func (a *app) scripts() ([]byte, error) {
	files := map[string][]byte{"switchyard.env": a.envFile()}
	for _, name := range []string{"switchyard.sh", "check-sql.awk", "router/" + a.env["ROUTER"] + ".sh"} {
		b, err := kit.ReadFile("kit/" + name)
		if err != nil {
			return nil, err
		}
		files[strings.ReplaceAll(name, "/", "-")] = b
	}
	job, err := a.render("job.yaml")
	if err != nil {
		return nil, err
	}
	files["job.yaml"] = job
	for name, b := range a.hooks {
		files["hook-"+name] = b
	}
	var keys []string
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	fmt.Fprintf(&b, "---\n# switchyard.sh, its config, the build Job and the app's hooks, mounted at /switchyard\n")
	fmt.Fprintf(&b, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n  labels: {app.kubernetes.io/part-of: %s}\ndata:\n",
		quote(a.Scripts), quote(a.Namespace), quote(a.App))
	for _, k := range keys {
		b.WriteString("  " + k + ": ")
		yamlBlock(&b, string(files[k]), "    ")
	}
	return b.Bytes(), nil
}

// manifests renders everything the app needs in the cluster besides its own slots, for
// kubectl apply -f -.
func (a *app) manifests() ([]byte, error) {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# switchyard %s for %s in namespace %s, rendered by `switchyard manifests`\n", version, a.App, a.Namespace)
	for _, name := range []string{"volume.yaml", "scripts", "rbac.yaml", "cronjobs.yaml", "policy.yaml"} {
		var b []byte
		var err error
		if name == "scripts" {
			b, err = a.scripts()
		} else {
			b, err = a.render(name)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

func manifestsCmd(args []string, w io.Writer) error {
	config := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			config = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			config = strings.TrimPrefix(args[i], "--config=")
		default:
			return fmt.Errorf("usage: switchyard manifests --config app.env")
		}
	}
	if config == "" {
		return fmt.Errorf("usage: switchyard manifests --config app.env")
	}
	a, err := loadApp(config)
	if err != nil {
		return err
	}
	b, err := a.manifests()
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
