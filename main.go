// switchyard: one page to deploy and roll back an app that is built in place on a volume, in a
// small Kubernetes cluster, without an image per release.
//
// It shows the releases that the kit's switchyard.sh keeps in the state ConfigMap (the last few,
// the live one, the traffic, what each was built from, the last runs), the latest deploy, quick or
// rollback Job with the last lines of its log, and starts a new Job from the suspended CronJob
// deploy (canary: traffic moves over in steps), quick (all at once) or rollback (to a kept
// release, named in the Job), as `kubectl create job --from=cronjob/...` does. It runs outside
// the cluster, so it can say "offline" when the cluster is switched off. It talks to the API with
// a token of its own (`switchyard manifests` renders its RBAC and admission policy).
// Standard library only. All links are relative, so it works behind any reverse-proxy prefix.
package main

import (
	_ "embed"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the image has no zoneinfo; TZ still works
)

const version = "0.5.0"

// cronJobs are the kinds of Job a click starts, each one also a POST route. The CronJobs they
// are made from carry the same names, behind the optional prefix.
var cronJobs = []string{"deploy", "quick", "rollback"}

//go:embed web/page.html
var page []byte

type config struct {
	api, token, ca, listen string
	namespace              string // where the app lives
	state                  string // the ConfigMap switchyard.sh keeps the releases in
	prefix                 string // in front of the CronJob names
	label                  string // selects the deploy, quick and rollback Jobs
	title                  string // the page's heading
	cluster                string // shown on the offline page
	links                  []Link // in the header
}

type Link struct{ Label, URL string }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseLinks reads "label=url label=url ...".
func parseLinks(s string) ([]Link, error) {
	var links []Link
	for _, f := range strings.Fields(s) {
		label, url, ok := strings.Cut(f, "=")
		if !ok || label == "" || url == "" {
			return nil, fmt.Errorf("link %q is not label=url", f)
		}
		links = append(links, Link{label, url})
	}
	return links, nil
}

func loadConfig() config {
	c := config{
		api:       env("SWITCHYARD_API", ""),
		token:     env("SWITCHYARD_TOKEN", "/data/token"),
		ca:        env("SWITCHYARD_CA", "/data/ca.crt"),
		listen:    env("SWITCHYARD_LISTEN", ":8080"),
		namespace: env("SWITCHYARD_NAMESPACE", ""),
		state:     env("SWITCHYARD_STATE", "switchyard-release"),
		prefix:    env("SWITCHYARD_JOB_PREFIX", ""),
		label:     env("SWITCHYARD_JOB_LABEL", "app.kubernetes.io/name=switchyard"),
		cluster:   env("SWITCHYARD_CLUSTER", "The cluster"),
	}
	c.title = env("SWITCHYARD_TITLE", c.namespace)
	if c.api == "" || c.namespace == "" {
		log.Fatal("SWITCHYARD_API and SWITCHYARD_NAMESPACE must be set, e.g. https://192.168.1.10:6443 and shop")
	}
	links, err := parseLinks(env("SWITCHYARD_LINKS", ""))
	if err != nil {
		log.Fatal("SWITCHYARD_LINKS: ", err)
	}
	c.links = links
	return c
}

// starter creates a Job from one of the CronJobs, one at a time, and never while another
// deploy or rollback is still running. switchyard.sh refuses that too, but only after its Job
// started, which leaves a failed Job behind.
type starter struct {
	mu      sync.Mutex
	running func() (bool, error)                      // is a deploy, quick or rollback Job still running?
	canRoll func(release string) error                // nil when the release can be rolled back to
	create  func(cronjob, tag string) (string, error) // returns the new Job's name
}

type refused string

func (r refused) Error() string { return string(r) }

// start a Job from the CronJob. tag: the release a rollback goes to; "drops" for a deploy
// that may run destructive migrations; "" otherwise.
func (s *starter) start(cronjob, tag string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	busy, err := s.running()
	if err != nil {
		return "", err
	}
	if busy {
		return "", refused("a deploy or rollback is still running")
	}
	if cronjob == "rollback" {
		if err := s.canRoll(tag); err != nil {
			return "", err
		}
	} else if tag != "" && tag != "drops" {
		return "", refused("unknown option " + tag)
	}
	return s.create(cronjob, tag)
}

// routes wires the endpoints. The POSTs are called by the page with fetch(): 204 on success,
// an error text otherwise. POST rollback?release=r<n> names the release to go back to,
// POST deploy?allow=drops (or quick) lets destructive migrations through.
func routes(cfg config, snap *snapshot, st *starter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(snap.get())
	})
	for _, cronjob := range cronJobs {
		mux.HandleFunc("POST /"+cronjob, func(w http.ResponseWriter, r *http.Request) {
			tag := r.URL.Query().Get("allow")
			if cronjob == "rollback" {
				tag = r.URL.Query().Get("release")
			}
			job, err := st.start(cronjob, tag)
			if err != nil {
				log.Printf("%s: %v", cronjob, err)
				code := http.StatusBadGateway
				if _, ok := err.(refused); ok {
					code = http.StatusConflict
				}
				http.Error(w, err.Error(), code)
				return
			}
			log.Printf("%s: started job %s", cronjob, job)
			snap.refresh() // the next poll already shows the new Job
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok " + version + "\n"))
	})
	// A click is a real deploy: no other site may trigger one through the browser.
	return http.NewCrossOriginProtection().Handler(mux)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" { // for container HEALTHCHECK, no curl in the image
		_, port, _ := net.SplitHostPort(env("SWITCHYARD_LISTEN", ":8080"))
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "manifests" { // the cluster side of an app, for kubectl apply -f -
		if err := manifestsCmd(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	cfg := loadConfig()
	k, err := newKube(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "check" { // after a change to the CronJobs or the policy
		failed := false
		for _, kind := range cronJobs {
			name, err := k.newJob(kind, "", true)
			if err != nil {
				failed = true
				log.Printf("%s: refused: %v", kind, err)
			} else {
				log.Printf("%s: the API server would create job %s (dry run, nothing started)", kind, name)
			}
		}
		if failed {
			os.Exit(1)
		}
		return
	}
	snap := newSnapshot(func() View { return k.view(cfg) }, time.Second, 15*time.Second)
	go snap.loop()
	st := &starter{running: k.jobRunning, canRoll: k.canRollBack, create: k.createJob}

	log.Printf("switchyard %s listening on %s, cluster %s, namespace %s", version, cfg.listen, cfg.api, cfg.namespace)
	log.Fatal(http.ListenAndServe(cfg.listen, routes(cfg, snap, st)))
}
