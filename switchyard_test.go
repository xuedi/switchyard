package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// r4 is live in slot a, r3 sits in slot b, r1 and r2 are kept on the volume only. r3 and older
// carry their commit under the keys written before switchyard (r<n>_core, r<n>_multisite).
const release = `{"data":{
	"releases":"r4 r3 r2 r1","next":"5","keep":"5","live":"a","a_release":"r4","b_release":"r3","weight_a":"100","weight_b":"0",
	"r4_meta_commit":"1574487","r4_meta_theme":"6708b3b","r4_subject":"Merge #61","r4_built":"2026-10-12T21:18:29+02:00","r4_by":"canary",
	"r3_core":"0000003","r3_multisite":"1000003","r2_core":"0000002","r1_core":"0000001",
	"status":"ok","step":"done","from":"r3","to":"r4",
	"started":"2026-10-12T21:18:01+02:00","finished":"2026-10-12T21:25:14+02:00","message":"release r4 live",
	"quick_status":"failed","quick_step":"start","quick_from":"r3","quick_to":"r4","quick_message":"failed in step start",
	"rollback_status":"ok","rollback_step":"done","rollback_to":"r3"}}`

const cronDeploy = `{"metadata":{"name":"deploy","uid":"uid-deploy"},"spec":{"jobTemplate":{
	"metadata":{"labels":{"app.kubernetes.io/name":"switchyard"}},
	"spec":{"backoffLimit":0,"template":{"spec":{"serviceAccountName":"shop-switchyard","containers":[{"name":"switchyard","command":["bash","/switchyard/switchyard.sh","deploy"]}]}}}}}}`

func jobJSON(name, owner, created string, done string) string {
	cond := ""
	if done != "" {
		cond = `,"conditions":[{"type":"` + done + `","status":"True"}]`
	}
	return `{"metadata":{"name":"` + name + `","creationTimestamp":"` + created + `","ownerReferences":[{"kind":"CronJob","name":"` + owner + `"}]},
		"status":{"startTime":"` + created + `"` + cond + `}}`
}

// fakeAPI serves canned answers for the paths switchyard reads, and records created Jobs.
type fakeAPI struct {
	*httptest.Server
	mu      sync.Mutex
	release string
	jobs    []string
	created []map[string]any
	dryRuns int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{release: release, jobs: []string{
		jobJSON("deploy-100", "deploy", "2026-10-12T19:00:00Z", "Complete"),
		jobJSON("rollback-200", "rollback", "2026-10-12T20:00:00Z", "Failed"),
	}}
	const ns = "/api/v1/namespaces/shop/"
	const batch = "/apis/batch/v1/namespaces/shop/"
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"message":"no token"}`, http.StatusUnauthorized)
			return
		}
		switch p, q := r.URL.Path, r.URL.Query(); {
		case r.Method == "POST" && p == batch+"jobs":
			if q.Get("dryRun") != "" {
				f.dryRuns++
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"metadata":{"name":"deploy-1"}}`))
				return
			}
			var j map[string]any
			json.NewDecoder(r.Body).Decode(&j)
			f.created = append(f.created, j)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(j)
		case p == ns+"configmaps/shop-release":
			w.Write([]byte(f.release))
		case p == batch+"jobs" && q.Get("labelSelector") == "app.kubernetes.io/name=switchyard":
			w.Write([]byte(`{"items":[` + strings.Join(f.jobs, ",") + `]}`))
		case p == batch+"cronjobs/deploy":
			w.Write([]byte(cronDeploy))
		case p == batch+"cronjobs/quick", p == batch+"cronjobs/rollback":
			name := strings.TrimPrefix(p, batch+"cronjobs/")
			w.Write([]byte(strings.NewReplacer(`"deploy","uid"`, `"`+name+`","uid"`, `switchyard.sh","deploy"`, `switchyard.sh","`+name+`"`).Replace(cronDeploy)))
		case p == ns+"pods" && q.Get("labelSelector") == "job-name=rollback-200":
			w.Write([]byte(`{"items":[{"metadata":{"name":"rollback-200-old","creationTimestamp":"2026-10-12T20:00:00Z"}},
				{"metadata":{"name":"rollback-200-new","creationTimestamp":"2026-10-12T20:00:05Z"}}]}`))
		case p == ns+"pods/rollback-200-new/log" && q.Get("tailLines") == "40":
			w.Write([]byte("21:00:00 rollback: slot b\n21:00:40 done\n"))
		default:
			http.Error(w, `{"message":"not here"}`, http.StatusNotFound)
		}
	}))
	return f
}

func (f *fakeAPI) kube() *kube {
	return testKube(f.URL, f.Client())
}

func testKube(api string, client *http.Client) *kube {
	return &kube{api: api, token: "test-token", ns: "shop", state: "shop-release", label: "app.kubernetes.io/name=switchyard", http: client}
}

func testConfig() config {
	return config{namespace: "shop", title: "Shop staging", cluster: "box", links: []Link{{"site", "https://site"}, {"mail", "https://mail"}}}
}

func TestView(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	v := f.kube().view(testConfig())
	if !v.Online || v.Error != "" {
		t.Fatalf("online=%v error=%q", v.Online, v.Error)
	}
	var ids []string
	for _, r := range v.Releases {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, " ") != "r4 r3 r2 r1" {
		t.Fatalf("releases newest first: %v", ids)
	}
	r4, r1 := v.Releases[0], v.Releases[3]
	if !r4.Live || !r4.Kept || r4.Slot != "a" || r4.Weight != 100 || r4.Subject != "Merge #61" || r4.By != "canary" || r4.Back != 0 {
		t.Errorf("r4 = %+v", r4)
	}
	if m := v.Releases[0].Meta; len(m) != 2 || m[0] != (Meta{"commit", "1574487"}) || m[1] != (Meta{"theme", "6708b3b"}) {
		t.Errorf("r4 meta, sorted by key: %+v", m)
	}
	if m := v.Releases[1].Meta; len(m) != 2 || m[0] != (Meta{"core", "0000003"}) || m[1] != (Meta{"multisite", "1000003"}) {
		t.Errorf("r3 meta from the keys written before switchyard: %+v", m)
	}
	if v.Keep != 5 || v.Title != "Shop staging" || len(v.Links) != 2 || v.Links[1].URL != "https://mail" {
		t.Errorf("keep %d, title %q, links %+v", v.Keep, v.Title, v.Links)
	}
	if r1.Live || r1.Slot != "" || r1.Back != 3 || v.Releases[1].Slot != "b" || v.Releases[1].Back != 1 {
		t.Errorf("r1 = %+v, r3 = %+v", r1, v.Releases[1])
	}
	if v.Rollback.Status != "ok" || v.Rollback.To != "r3" {
		t.Errorf("rollback = %+v", v.Rollback)
	}
	if v.Run.Status != "ok" || v.Run.Step != "done" || v.Run.From != "r3" || v.Run.Finished == "" {
		t.Errorf("run = %+v", v.Run)
	}
	if v.Quick.Status != "failed" || v.Quick.Step != "start" || v.Quick.To != "r4" || v.Quick.Message == "" {
		t.Errorf("quick = %+v", v.Quick)
	}
	if v.Busy {
		t.Errorf("busy")
	}
	j := v.Job
	if j == nil || j.Name != "rollback-200" || j.Kind != "rollback" || j.Phase != "failed" {
		t.Fatalf("the newest Job should be shown: %+v", j)
	}
	if len(j.Log) != 2 || j.Log[1] != "21:00:40 done" {
		t.Errorf("log of the newest pod: %q", j.Log)
	}
}

func TestOffline(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	client := srv.Client()
	srv.Close() // nothing listens any more
	k := testKube(srv.URL, client)
	if v := k.view(testConfig()); v.Online || v.Cluster != "box" {
		t.Errorf("unreachable API must read as offline: %+v", v)
	}
}

func TestErrorAnswerIsOnline(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	k := f.kube()
	k.token = "wrong"
	v := k.view(testConfig())
	if !v.Online || !strings.Contains(v.Error, "401") {
		t.Errorf("an API that answers is online, its error shown: online=%v error=%q", v.Online, v.Error)
	}
}

func TestPhaseAndKind(t *testing.T) {
	var j job
	json.Unmarshal([]byte(jobJSON("deploy-1-second", "deploy", "2026-10-12T19:00:00Z", "")), &j)
	if j.phase() != "running" || j.kind("") != "deploy" {
		t.Errorf("phase %q kind %q", j.phase(), j.kind(""))
	}
	j = job{}
	json.Unmarshal([]byte(`{"metadata":{"name":"rollback-5"},"status":{"conditions":[{"type":"Failed","status":"False"},{"type":"Complete","status":"True"}]}}`), &j)
	if j.phase() != "complete" || j.kind("") != "rollback" {
		t.Errorf("phase %q kind %q (no owner: from the name)", j.phase(), j.kind(""))
	}
	json.Unmarshal([]byte(jobJSON("shop-quick-1", "shop-quick", "2026-10-12T19:00:00Z", "")), &j)
	if j.kind("shop-") != "quick" {
		t.Errorf("kind %q: the prefix belongs to the CronJob's name, not the kind", j.kind("shop-"))
	}
	j = job{Metadata: meta{Name: "shop-rollback-r3-5"}}
	if j.kind("shop-") != "rollback" {
		t.Errorf("kind %q from a prefixed name", j.kind("shop-"))
	}
}

func handler(f *fakeAPI) http.Handler {
	k := f.kube()
	cfg := testConfig()
	snap := newSnapshot(func() View { return k.view(cfg) }, time.Hour, time.Hour)
	return routes(cfg, snap, &starter{running: k.jobRunning, canRoll: k.canRollBack, create: k.createJob})
}

func post(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDeployCreatesJobLikeKubectl(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	if w := post(handler(f), "/deploy"); w.Code != http.StatusNoContent {
		t.Fatalf("deploy: %d %s", w.Code, w.Body)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d jobs", len(f.created))
	}
	b, _ := json.Marshal(f.created[0])
	got := string(b)
	for _, want := range []string{
		`"kind":"Job"`,
		`"cronjob.kubernetes.io/instantiate":"manual"`,
		`"ownerReferences":[{"apiVersion":"batch/v1","controller":true,"kind":"CronJob","name":"deploy","uid":"uid-deploy"}]`,
		`"labels":{"app.kubernetes.io/name":"switchyard"}`,
		`"spec":{"backoffLimit":0,"template":{"spec":{"containers":[{"command":["bash","/switchyard/switchyard.sh","deploy"],"name":"switchyard"}],"serviceAccountName":"shop-switchyard"}}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("job lacks %s\n%s", want, got)
		}
	}
	if !regexp.MustCompile(`"name":"deploy-\d{10}"`).MatchString(got) {
		t.Errorf("name should be deploy-<unix time>: %s", got)
	}
}

func TestRefusedWhileRunning(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	f.jobs = append(f.jobs, jobJSON("deploy-300", "deploy", "2026-10-12T21:00:00Z", ""))
	h := handler(f)
	for _, path := range []string{"/deploy", "/quick", "/rollback?release=r3"} {
		if w := post(h, path); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still running") {
			t.Errorf("%s while a job runs: %d %s", path, w.Code, w.Body)
		}
	}
	if len(f.created) != 0 {
		t.Errorf("refused clicks must not create jobs: %v", f.created)
	}
	if v := f.kube().view(testConfig()); !v.Busy || v.Job.Name != "deploy-300" || v.Job.Phase != "running" {
		t.Errorf("busy=%v job=%+v", v.Busy, v.Job)
	}
}

func TestRollbackToAKeptRelease(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	h := handler(f)
	for path, want := range map[string]string{
		"/rollback":            "no release given",
		"/rollback?release=x":  "no release given",
		"/rollback?release=r9": "release r9 is not kept",
		"/rollback?release=r4": "release r4 is already live",
	} {
		if w := post(h, path); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if len(f.created) != 0 {
		t.Fatalf("refused rollbacks created %v", f.created)
	}
	if w := post(h, "/rollback?release=r1"); w.Code != http.StatusNoContent {
		t.Fatalf("rollback to r1: %d %s", w.Code, w.Body)
	}
	b, _ := json.Marshal(f.created)
	if !regexp.MustCompile(`"name":"rollback-r1-\d{10}"`).Match(b) || !strings.Contains(string(b), `"kind":"CronJob","name":"rollback"`) {
		t.Errorf("rollback job: %s", b)
	}
}

func TestDeployingReleaseListedFirst(t *testing.T) {
	data := map[string]string{"releases": "r4 r3", "live": "a", "a_release": "r4", "b_release": "r5",
		"weight_a": "90", "weight_b": "10", "r5_meta_commit": "abc"}
	rs := releases(data)
	if len(rs) != 3 || rs[0].ID != "r5" || rs[0].Kept || rs[0].Weight != 10 || rs[0].Meta[0].Value != "abc" || !rs[1].Live || rs[1].Weight != 90 {
		t.Errorf("a release taking traffic before it is kept: %+v", rs)
	}
	data["weight_a"], data["weight_b"] = "100", "0"
	if rs := releases(data); len(rs) != 2 {
		t.Errorf("a failed one without traffic is not listed: %+v", rs)
	}
}

func TestOneAtATime(t *testing.T) {
	var mu sync.Mutex
	running := false
	created := 0
	st := &starter{
		running: func() (bool, error) { mu.Lock(); defer mu.Unlock(); return running, nil },
		canRoll: func(string) error { return nil },
		create: func(string, string) (string, error) {
			time.Sleep(10 * time.Millisecond) // the API is slow; a second click comes in meanwhile
			mu.Lock()
			defer mu.Unlock()
			running = true
			created++
			return "deploy-1", nil
		},
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); st.start("deploy", "") }()
	}
	wg.Wait()
	if created != 1 {
		t.Errorf("five quick clicks created %d jobs", created)
	}
}

func TestCrossSiteRefused(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	r := httptest.NewRequest("POST", "/deploy", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	handler(f).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(f.created) != 0 {
		t.Errorf("cross-site POST: %d, created %d", w.Code, len(f.created))
	}
}

func TestPageAndState(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	h := handler(f)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `fetch("state"`) {
		t.Errorf("page: %d", w.Code)
	}
	if m := regexp.MustCompile(`(href|src|action)="/|fetch\("/|fetch\(.\/`).FindString(body); m != "" {
		t.Errorf("page must use relative URLs only, it runs behind proxy prefixes: %s", m)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/state", nil))
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || !v.Online || len(v.Releases) != 4 || v.Version != version {
		t.Errorf("state: %v %+v", err, v)
	}
}

func TestDryRunCreatesNothing(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	if _, err := f.kube().newJob("deploy", "", true); err != nil {
		t.Fatal(err)
	}
	if f.dryRuns != 1 || len(f.created) != 0 {
		t.Errorf("dry runs %d, created %v", f.dryRuns, f.created)
	}
}

func TestQuickCreatesJobFromItsCronJob(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	if w := post(handler(f), "/quick"); w.Code != http.StatusNoContent {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d jobs", len(f.created))
	}
	b, _ := json.Marshal(f.created[0])
	got := string(b)
	if !strings.Contains(got, `"kind":"CronJob","name":"quick"`) || !strings.Contains(got, `"/switchyard/switchyard.sh","quick"`) ||
		!regexp.MustCompile(`"name":"quick-\d{10}"`).MatchString(got) {
		t.Errorf("quick job: %s", got)
	}
}

func TestDeployAnywayAllowsDrops(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	h := handler(f)
	if w := post(h, "/quick?allow=everything"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "unknown option") {
		t.Errorf("unknown option: %d %s", w.Code, w.Body)
	}
	if w := post(h, "/deploy?allow=drops"); w.Code != http.StatusNoContent {
		t.Fatalf("deploy anyway: %d %s", w.Code, w.Body)
	}
	b, _ := json.Marshal(f.created)
	if len(f.created) != 1 || !regexp.MustCompile(`"name":"deploy-drops-\d{10}"`).Match(b) {
		t.Errorf("the permission travels in the name: %s", b)
	}
}

func TestBlockedAndDrops(t *testing.T) {
	data := map[string]string{"releases": "r2 r1", "live": "a", "a_release": "r2", "r2_drops": "1",
		"quick_status": "failed", "quick_blocked": "V1: drops column x: ALTER TABLE t DROP x\nV1: drops a table: DROP TABLE y\n"}
	if q := run(data, "quick_"); len(q.Blocked) != 2 || !strings.HasPrefix(q.Blocked[1], "V1: drops a table") {
		t.Errorf("blocked = %q", q.Blocked)
	}
	if r := run(data, ""); r.Blocked != nil {
		t.Errorf("no blocked statements: %q", r.Blocked)
	}
	if rs := releases(data); !rs[0].Drops || rs[1].Drops {
		t.Errorf("drops = %+v", rs)
	}
}

func TestPrefixedCronJobs(t *testing.T) {
	f := newFakeAPI(t)
	defer f.Close()
	k := f.kube()
	k.prefix = "shop-"
	if _, err := k.createJob("deploy", ""); err == nil || !strings.Contains(err.Error(), "cronjobs/shop-deploy") {
		t.Errorf("the CronJob's name has the prefix: %v", err)
	}
}

func TestParseLinks(t *testing.T) {
	links, err := parseLinks(" site=https://shop.example  mail=https://mail.example/?a=b ")
	if err != nil || len(links) != 2 || links[0] != (Link{"site", "https://shop.example"}) || links[1].URL != "https://mail.example/?a=b" {
		t.Errorf("links %+v, %v", links, err)
	}
	if _, err := parseLinks("site"); err == nil {
		t.Errorf("a link without a URL must be refused")
	}
}
