package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const example = "examples/symfony-doctrine/app.env"

func TestParseEnv(t *testing.T) {
	m, err := parseEnv(strings.NewReader(`
# a comment
APP=shop
PAGES="/ /a"
URL=mysql://u:$(PW)@db/x?a=1&b=2
NOTE=keep   # this goes
QUOTED='a # b'
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"APP": "shop", "PAGES": "/ /a", "URL": "mysql://u:$(PW)@db/x?a=1&b=2", "NOTE": "keep", "QUOTED": "a # b"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, bad := range []string{"app=x", "APP x", "APP=a\nAPP=b"} {
		if _, err := parseEnv(strings.NewReader(bad)); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func minimal() map[string]string {
	return map[string]string{"APP": "shop", "NAMESPACE": "web", "IMAGE": "img:1",
		"CADDY_ADMIN": "http://caddy:2019", "CADDY_CONFIGMAP": "edge/caddyfile"}
}

func TestResolveDefaults(t *testing.T) {
	a, err := resolveApp(minimal())
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "shop-release" || a.SlotA != "shop-a" || a.ReleasesPVC != "shop-releases" || a.BuildImage != "img:1" ||
		a.env["CADDY_BLOCK"] != "shop" || a.JobLabelKey != "app.kubernetes.io/name" || a.JobLabelValue != "switchyard" ||
		a.UID != 1000 || a.GID != 1000 || strings.Join(a.CronJobs, " ") != "deploy quick rollback" {
		t.Errorf("defaults: %+v", a)
	}
}

func TestResolveRefuses(t *testing.T) {
	for key, val := range map[string]string{
		"TYPO":            "1",
		"APP":             "Shop",
		"JOB_PREFIX":      "shop",
		"JOB_LABEL":       "app.kubernetes.io/part-of=x",
		"SLOT_B":          "shop-a",
		"KEEP":            "1",
		"STEPS":           "50 10",
		"PAGES":           "/ products",
		"RUN_AS":          "0",
		"FOLLOWERS":       "job/x",
		"ROUTER":          "nginx",
		"CADDY_CONFIGMAP": "caddyfile",
		"BUILD_MOUNTS":    "secret/key",
		"BUILD_ENV_FROM":  "configmap/x:/y",
		"BUILD_MOUNT":     "/releases",
	} {
		m := minimal()
		m[key] = val
		if _, err := resolveApp(m); err == nil {
			t.Errorf("%s=%s must be refused", key, val)
		}
	}
	m := minimal()
	delete(m, "IMAGE")
	if _, err := resolveApp(m); err == nil || !strings.Contains(err.Error(), "IMAGE is required") {
		t.Errorf("IMAGE missing: %v", err)
	}
}

func render(t *testing.T, m map[string]string) string {
	t.Helper()
	a, err := resolveApp(m)
	if err != nil {
		t.Fatal(err)
	}
	a.hooks = map[string][]byte{"build": []byte("#!/bin/bash\necho BUILT commit=abc\n")}
	b, err := a.manifests()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestManifestsNamesFromConfig(t *testing.T) {
	m := minimal()
	m["JOB_PREFIX"] = "shop-"
	m["FOLLOWERS"] = "cronjob/shop-cron deployment/shop-worker"
	out := render(t, m)
	for _, want := range []string{
		// the policy: the dashboard's account, the three prefixed CronJobs, one binding each
		`expression: "request.userInfo.username == 'system:serviceaccount:web:shop-dashboard'"`,
		`expression: "variables.owners.size() == 1 && variables.owners[0] in ['shop-deploy', 'shop-quick', 'shop-rollback']"`,
		`name: "switchyard-web-shop-shop-rollback"`,
		`paramRef: {name: "shop-quick", namespace: "web", parameterNotFoundAction: Deny}`,
		// RBAC: the dashboard reads those CronJobs, switchyard.sh patches slots and followers
		`resourceNames: ["shop-deploy", "shop-quick", "shop-rollback"]`,
		`resourceNames: ["shop-a", "shop-b", "shop-worker"]`,
		`resourceNames: ["shop-cron"]`,
		`resourceNames: ["caddyfile"]`,
		"namespace: \"edge\"",
		// the CronJobs run the kit from the scripts ConfigMap, with the label the dashboard selects
		`name: "shop-deploy"`,
		`command: [bash, /switchyard/switchyard.sh, rollback]`,
		`"app.kubernetes.io/name": "switchyard"`,
		`configMap: {name: "shop-switchyard"}`,
		`claimName: "shop-releases"`,
		"    JOB_PREFIX='shop-'\n",
		"    FOLLOWERS='cronjob/shop-cron deploy/shop-worker'\n",
		"  hook-build: |\n    #!/bin/bash\n    echo BUILT commit=abc\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manifests lack %s", want)
		}
	}
	if n := strings.Count(out, "kind: ValidatingAdmissionPolicyBinding"); n != 3 {
		t.Errorf("%d policy bindings", n)
	}
	if strings.Count(out, "<no value>") != 1 { // the one in switchyard.sh's state()
		t.Errorf("a template field was not filled")
	}
}

func TestManifestsWithoutCronJobFollowers(t *testing.T) {
	out := render(t, minimal())
	if strings.Contains(out, "resources: [cronjobs]\n    resourceNames: []") || strings.Contains(out, "resourceNames: []") {
		t.Errorf("an empty resourceNames would allow every object")
	}
}

func TestExampleRenders(t *testing.T) {
	var out bytes.Buffer
	if err := manifestsCmd([]string{"--config", example}, &out); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"prepare", "pending-sql", "build", "after-rollback"} {
		if !strings.Contains(out.String(), "  hook-"+hook+": |") {
			t.Errorf("hook %s missing", hook)
		}
	}
	script, _ := os.ReadFile("kit/switchyard.sh")
	for _, line := range strings.Split(strings.TrimSpace(string(script)), "\n") {
		if line != "" && !strings.Contains(out.String(), "    "+line+"\n") {
			t.Fatalf("switchyard.sh not embedded line by line: %q", line)
		}
	}
}

func TestYAMLBlock(t *testing.T) {
	for text, head := range map[string]string{
		"a\nb\n":     "|\n",
		"a\nb":       "|-\n",
		"a\n\n":      "|+\n",
		"  a\n":      "|2\n",
		"a\n\tb\n":   "|\n",
		"\na\n":      "|2\n",
		"a\n\n\nb\n": "|\n",
	} {
		var b bytes.Buffer
		yamlBlock(&b, text, "    ")
		if !strings.HasPrefix(b.String(), head) {
			t.Errorf("%q: %q", text, b.String())
		}
	}
}

func bash(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command("bash", "-c", "set -euo pipefail; source kit/switchyard.sh; "+script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

func TestJobNameOptions(t *testing.T) {
	for name, want := range map[string]string{
		"rollback-r7-1760000000":      "to=r7 allow= reapply=",
		"rollback-7-1760000000":       "to= allow= reapply=",
		"deploy-drops-1760000000":     "to= allow=1 reapply=",
		"quick-drops-1760000000":      "to= allow=1 reapply=",
		"rollback-drops-1760000000":   "to= allow= reapply=",
		"deploy-1760000000":           "to= allow= reapply=",
		"deploy-reapply-1760000000":   "to= allow= reapply=1",
		"shop-rollback-r3-1760000000": "to= allow= reapply=",
	} {
		got := bash(t, `JOB_PREFIX=""; job_options "`+name+`"; echo "to=$TO allow=$ALLOW reapply=$REAPPLY"`)
		if strings.TrimSpace(got) != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	got := bash(t, `JOB_PREFIX="shop-"; job_options shop-rollback-r3-1760000000; echo "$TO"; job_options shop-quick-drops-1; echo "$ALLOW"`)
	if got != "r3\n1\n" {
		t.Errorf("with a prefix: %q", got)
	}
}

func TestJSONString(t *testing.T) {
	text := "a \"quoted\" \\ back\nslash\ttab\r"
	out, err := exec.Command("bash", "-c", "source kit/switchyard.sh; json_str \"$1\"", "-", text).Output()
	if err != nil {
		t.Fatal(err)
	}
	var back string
	if err := json.Unmarshal(out, &back); err != nil || back != text {
		t.Errorf("json_str %q -> %s -> %q, %v", text, out, back, err)
	}
}

func checkSQL(t *testing.T, file string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	out, err := exec.Command("awk", "-f", "kit/check-sql.awk", file).Output()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestCheckSQL(t *testing.T) {
	out, code := checkSQL(t, "kit/testdata/pending.sql")
	want, _ := os.ReadFile("kit/testdata/pending.out")
	if out != string(want) || code != 1 {
		t.Errorf("exit %d, output:\n%s\nwant:\n%s", code, out, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !regexp.MustCompile(`^(BLOCKED [^:]+: [^:]+: .+|pending: \d+ statements in \d+ migrations, \d+ destructive)$`).MatchString(line) {
			t.Errorf("line %q: switchyard.sh and the page read BLOCKED lines", line)
		}
	}
	if out, code := checkSQL(t, "kit/testdata/additive.sql"); code != 0 || !strings.Contains(out, "none destructive") {
		t.Errorf("only additions must pass: exit %d %s", code, out)
	}
	if out, code := checkSQL(t, os.DevNull); code != 0 || out != "no pending migrations\n" {
		t.Errorf("nothing pending: exit %d %q", code, out)
	}
}

func TestRouterBlock(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	dir := t.TempDir()
	caddyfile := "shop.example.org {\n\treverse_proxy {\n\t\t# BEGIN shop\n\t\tto old:8080\n\t\t# END shop\n\t\theader_up X-Forwarded-Proto https\n\t}\n}\n"
	// a fake kubectl and curl: the Caddyfile out of the ConfigMap, the patched one recorded
	os.WriteFile(dir+"/kubectl", []byte("#!/bin/bash\ncase \"$*\" in *get*) printf '%s' \"$CADDYFILE\" ;; *patch*) printf '%s ' \"$@\" > \""+dir+"/patch\" ;; esac\n"), 0o755)
	os.WriteFile(dir+"/curl", []byte("#!/bin/bash\n"), 0o755)
	cmd := exec.Command("bash", "-c", `source kit/switchyard.sh; source kit/router/caddy.sh
		NAMESPACE=web PORT=8080 CADDY_CONFIGMAP=edge/caddyfile CADDY_KEY=Caddyfile CADDY_BLOCK=shop CADDY_COOKIE=shop_slot CADDY_ADMIN=http://caddy:2019
		router_set shop-a 90 shop-b 10 && cat "$TMPDIR/Caddyfile"`)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CADDYFILE="+caddyfile, "TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "shop.example.org {\n\treverse_proxy {\n\t\t# BEGIN shop\n\t\tto shop-a.web.svc.cluster.local:8080 shop-b.web.svc.cluster.local:8080\n" +
		"\t\tlb_policy cookie shop_slot {\n\t\t\tfallback weighted_round_robin 90 10\n\t\t}\n\t\t# END shop\n\t\theader_up X-Forwarded-Proto https\n\t}\n}\n"
	if string(out) != want {
		t.Errorf("Caddyfile:\n%s\nwant:\n%s", out, want)
	}
	patch, _ := os.ReadFile(dir + "/patch")
	if !strings.Contains(string(patch), `-n edge patch configmap caddyfile --type merge -p {"data":{"Caddyfile":"shop.example.org {\n\treverse_proxy {\n\t\t# BEGIN shop\n`) {
		t.Errorf("stored back: %s", patch)
	}
}
