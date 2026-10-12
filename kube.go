package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// kube is a minimal client for the Kubernetes API: one server, one bearer token, one namespace,
// and the names of the objects switchyard reads there.
type kube struct {
	api, token, ns string
	state          string // the state ConfigMap
	prefix         string // in front of the CronJob names
	label          string // selects the deploy, quick and rollback Jobs
	http           *http.Client
}

func newKube(cfg config) (*kube, error) {
	token, err := os.ReadFile(cfg.token)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	ca, err := os.ReadFile(cfg.ca)
	if err != nil {
		return nil, fmt.Errorf("CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("CA: no PEM certificate in %s", cfg.ca)
	}
	return &kube{
		api:    strings.TrimRight(cfg.api, "/"),
		token:  strings.TrimSpace(string(token)),
		ns:     cfg.namespace,
		state:  cfg.state,
		prefix: cfg.prefix,
		label:  cfg.label,
		http: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

// apiError is an answer from the API server, as opposed to no answer at all (offline).
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// do sends one request. With out == nil the body is discarded; a *[]byte gets it raw (logs),
// anything else is decoded from JSON.
func (k *kube) do(method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, k.api+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var st struct{ Message string }
		json.NewDecoder(resp.Body).Decode(&st)
		return &apiError{resp.StatusCode, fmt.Sprintf("%s %s: %s %s", method, path, resp.Status, st.Message)}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o, err = io.ReadAll(resp.Body)
		return err
	default:
		return json.NewDecoder(resp.Body).Decode(out)
	}
}

func (k *kube) get(path string, out any) error { return k.do("GET", path, nil, out) }

// path of a namespaced object or collection, e.g. k.path("batch/v1", "jobs") or
// k.path("v1", "configmaps/switchyard-release").
func (k *kube) path(groupVersion, rest string) string {
	if groupVersion == "v1" {
		return "/api/v1/namespaces/" + k.ns + "/" + rest
	}
	return "/apis/" + groupVersion + "/namespaces/" + k.ns + "/" + rest
}

func selector(s string) string { return "?" + url.Values{"labelSelector": {s}}.Encode() }

// --- the bits of the Kubernetes objects switchyard reads ---

type ownerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller,omitempty"`
}

type meta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []ownerRef        `json:"ownerReferences,omitempty"`
}

type job struct {
	Metadata meta `json:"metadata"`
	Status   struct {
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// phase is "running" until the Job has its final condition, "complete" or "failed" then.
func (j job) phase() string {
	for _, c := range j.Status.Conditions {
		if c.Status == "True" && (c.Type == "Complete" || c.Type == "Failed") {
			return strings.ToLower(c.Type)
		}
	}
	return "running"
}

// kind is the CronJob the Job was made from, without the prefix: deploy, quick or rollback.
func (j job) kind(prefix string) string {
	for _, o := range j.Metadata.OwnerReferences {
		if o.Kind == "CronJob" {
			return strings.TrimPrefix(o.Name, prefix)
		}
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(j.Metadata.Name, prefix), "-")
	return name
}

type cronJob struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		JobTemplate struct {
			Metadata struct {
				Labels      map[string]string `json:"labels,omitempty"`
				Annotations map[string]string `json:"annotations,omitempty"`
			} `json:"metadata"`
			Spec json.RawMessage `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

// --- what the page shows ---

// Release is one build of the app, r<n>: a kept one (it went live), or the one a deploy is
// moving the traffic to. The page lists them newest first.
type Release struct {
	ID, Subject, Built, By string
	Meta                   []Meta // what the build hook reported, e.g. the commit, sorted by key
	Slot                   string // a or b while a slot runs it
	Weight                 int    // its share of the traffic
	Live, Kept             bool
	Drops                  bool // it ran destructive migrations on purpose: older releases may break
	Back                   int  // kept releases between it and the live one, negative when newer
}

type Meta struct{ Key, Value string }

// Run is the last run as switchyard.sh recorded it: deploy or rollback, or quick under its own keys.
type Run struct {
	Status, Step, Message, Started, Finished, From, To string
	Blocked                                            []string // destructive migrations that stopped the build
}

// Job is the latest deploy or rollback Job, running or not.
type Job struct {
	Name, Kind, Phase, Started, Finished string
	Log                                  []string
}

type View struct {
	Version  string
	Title    string
	Cluster  string // its name, for the offline page
	Links    []Link
	Online   bool
	Error    string
	Time     string
	Keep     int // how many releases switchyard.sh keeps, 0 when it has not said
	Releases []Release
	Run      Run // the last canary deploy
	Quick    Run // the last quick deploy
	Rollback Run // the last rollback
	Job      *Job
	Busy     bool // a deploy, quick or rollback Job is running: every button refuses
}

const logLines = 40

// releaseMeta is what the build reported for a release: the keys r<n>_meta_<key>, or, as written before
// switchyard, r<n>_core and r<n>_multisite.
func releaseMeta(data map[string]string, id string) []Meta {
	var list []Meta
	for key, val := range data {
		if k, ok := strings.CutPrefix(key, id+"_meta_"); ok {
			list = append(list, Meta{k, val})
		}
	}
	if len(list) == 0 {
		for _, k := range []string{"core", "multisite"} {
			if val := data[id+"_"+k]; val != "" {
				list = append(list, Meta{k, val})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	return list
}

// releases lists the kept releases newest first, and before them one a deploy is still
// bringing in. Releases are written by switchyard.sh (releases, r<n>_*, <slot>_release).
func releases(data map[string]string) []Release {
	kept := strings.Fields(data["releases"])
	live := data[data["live"]+"_release"]
	at := map[string]int{}
	for i, id := range kept {
		at[id] = i
	}
	one := func(id string) Release {
		r := Release{ID: id, Meta: releaseMeta(data, id), Subject: data[id+"_subject"],
			Built: data[id+"_built"], By: data[id+"_by"], Drops: data[id+"_drops"] == "1", Live: id == live}
		for _, s := range []string{"a", "b"} {
			if data[s+"_release"] == id {
				r.Slot = s
				r.Weight, _ = strconv.Atoi(data["weight_"+s])
			}
		}
		if i, ok := at[id]; ok {
			r.Kept = true
			if j, ok := at[live]; ok {
				r.Back = i - j
			}
		}
		return r
	}
	var list []Release
	for _, s := range []string{"a", "b"} {
		id := data[s+"_release"]
		if _, ok := at[id]; id != "" && !ok && (id == live || data["weight_"+s] != "0") {
			list = append(list, one(id))
		}
	}
	for _, id := range kept {
		list = append(list, one(id))
	}
	return list
}

func run(data map[string]string, prefix string) Run {
	r := Run{Status: data[prefix+"status"], Step: data[prefix+"step"], Message: data[prefix+"message"],
		Started: data[prefix+"started"], Finished: data[prefix+"finished"], From: data[prefix+"from"], To: data[prefix+"to"]}
	if b := strings.TrimSpace(data[prefix+"blocked"]); b != "" {
		r.Blocked = strings.Split(b, "\n")
	}
	return r
}

func (k *kube) release() (map[string]string, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	err := k.get(k.path("v1", "configmaps/"+k.state), &cm)
	return cm.Data, err
}

// jobs returns the deploy, quick and rollback Jobs (build Jobs do not carry the label), the newest first.
func (k *kube) jobs() ([]job, error) {
	var list struct {
		Items []job `json:"items"`
	}
	if err := k.get(k.path("batch/v1", "jobs"+selector(k.label)), &list); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Metadata.CreationTimestamp.After(list.Items[j].Metadata.CreationTimestamp)
	})
	return list.Items, nil
}

// logTail returns the last lines of the Job's newest pod, nil while there is none to read.
func (k *kube) logTail(jobName string) []string {
	var pods struct {
		Items []struct {
			Metadata meta `json:"metadata"`
		} `json:"items"`
	}
	if err := k.get(k.path("v1", "pods"+selector("job-name="+jobName)), &pods); err != nil || len(pods.Items) == 0 {
		return nil
	}
	newest := pods.Items[0].Metadata
	for _, p := range pods.Items[1:] {
		if p.Metadata.CreationTimestamp.After(newest.CreationTimestamp) {
			newest = p.Metadata
		}
	}
	var raw []byte
	if err := k.get(k.path("v1", "pods/"+newest.Name+"/log?tailLines="+strconv.Itoa(logLines)), &raw); err != nil {
		return nil // still being created, or already gone
	}
	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// view reads the cluster once. No answer from the API counts as "offline"; an error answer
// (a missing permission, say) is shown in Error with whatever was read.
func (k *kube) view(cfg config) View {
	v := View{Version: version, Title: cfg.title, Cluster: cfg.cluster, Links: cfg.links, Time: time.Now().Format("15:04:05")}
	data, err := k.release()
	var ae *apiError
	if err != nil && !errors.As(err, &ae) {
		log.Printf("offline: %v", err)
		return v
	}
	v.Online = true
	var errs []string
	if err != nil {
		errs = append(errs, err.Error())
	} else {
		v.Releases = releases(data)
		v.Keep, _ = strconv.Atoi(data["keep"])
		v.Run, v.Quick, v.Rollback = run(data, ""), run(data, "quick_"), run(data, "rollback_")
	}

	jobs, err := k.jobs()
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, j := range jobs {
		if j.phase() == "running" {
			v.Busy = true
		}
	}
	if len(jobs) > 0 {
		j := jobs[0]
		v.Job = &Job{Name: j.Metadata.Name, Kind: j.kind(k.prefix), Phase: j.phase(),
			Started: stamp(j.Status.StartTime), Finished: stamp(j.Status.CompletionTime), Log: k.logTail(j.Metadata.Name)}
	}
	v.Error = strings.Join(errs, "; ")
	return v
}

// --- the only write: a Job from a CronJob ---

func (k *kube) jobRunning() (bool, error) {
	jobs, err := k.jobs()
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.phase() == "running" {
			return true, nil
		}
	}
	return false, nil
}

var releaseID = regexp.MustCompile(`^r[0-9]{1,9}$`)

// canRollBack: id is a kept release, and not the live one.
func (k *kube) canRollBack(id string) error {
	if !releaseID.MatchString(id) {
		return refused("no release given")
	}
	data, err := k.release()
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(data["releases"]), id) {
		return refused("release " + id + " is not kept any more")
	}
	if data[data["live"]+"_release"] == id {
		return refused("release " + id + " is already live")
	}
	return nil
}

// createJob does what `kubectl create job --from=cronjob/<name>` does: the CronJob's job
// template, unchanged, as a Job named <cronjob>-<unix time>, owned by the CronJob. The
// admission policy lets this token create nothing else. The name is the one part left free, so
// it carries what switchyard.sh needs to know: rollback-r<n>-<unix time> the release to go back
// to, deploy-drops-<unix time> (or quick-) that destructive migrations may run.
func (k *kube) createJob(kind, tag string) (string, error) { return k.newJob(kind, tag, false) }

// newJob with dryRun: the API server checks the Job (RBAC, the admission policy) but keeps
// nothing. `switchyard check` uses it.
func (k *kube) newJob(kind, tag string, dryRun bool) (string, error) {
	name := k.prefix + kind
	var cj cronJob
	if err := k.get(k.path("batch/v1", "cronjobs/"+name), &cj); err != nil {
		return "", err
	}
	tmpl := cj.Spec.JobTemplate
	ann := map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	for key, val := range tmpl.Metadata.Annotations {
		ann[key] = val
	}
	controller := true
	j := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":            jobName(name, tag),
			"namespace":       k.ns,
			"labels":          tmpl.Metadata.Labels,
			"annotations":     ann,
			"ownerReferences": []ownerRef{{APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Metadata.Name, UID: cj.Metadata.UID, Controller: &controller}},
		},
		"spec": tmpl.Spec,
	}
	body, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	var created job
	path := k.path("batch/v1", "jobs")
	if dryRun {
		path += "?dryRun=All"
	}
	if err := k.do("POST", path, body, &created); err != nil {
		return "", err
	}
	return created.Metadata.Name, nil
}

func jobName(cronjob, tag string) string {
	if tag != "" {
		return fmt.Sprintf("%s-%s-%d", cronjob, tag, time.Now().Unix())
	}
	return fmt.Sprintf("%s-%d", cronjob, time.Now().Unix())
}
