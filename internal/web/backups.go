package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xuedi/switchyard/internal/backup"
	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/kube"
)

// BackupView is what the backups page of a namespace shows.
type BackupView struct {
	Version   string
	Cluster   string
	Time      string
	Online    bool
	Error     string
	Namespace string
	Set       config.Backup
	Archives  []Archive    // newest first
	Runs      []backup.Run // without their logs
	Run       *backup.Run  // the running one, or the newest, with its log
	Busy      bool
	IndexAt   string
	Size      int64
}

// Archive is one run's files in the target.
type Archive struct {
	Run      string
	Files    []backup.File
	Items    []string // the set's items it has
	Size     int64
	Complete bool
	Manifest bool
	Kind     string // from the manifest: manual, scheduled, pre-restore
}

var runPrefix = regexp.MustCompile(`^(\d{4}-\d\d-\d\d_\d\d-\d\d)_(.+)$`)

func archives(set config.Backup, st backup.State) ([]Archive, int64) {
	byRun := map[string]*Archive{}
	var total int64
	for _, f := range st.Files {
		total += f.Size
		m := runPrefix.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		a := byRun[m[1]]
		if a == nil {
			a = &Archive{Run: m[1], Items: []string{}}
			byRun[m[1]] = a
		}
		a.Files = append(a.Files, f)
		a.Size += f.Size
		if m[2] == "manifest.json" {
			a.Manifest = true
			var man struct{ Kind string }
			json.Unmarshal(st.Manifests[f.Name], &man)
			a.Kind = man.Kind
		}
		for _, it := range set.Items {
			if m[2] == it.Name+it.Ext() {
				a.Items = append(a.Items, it.Name)
			}
		}
	}
	var list []Archive
	for _, a := range byRun {
		a.Complete = a.Manifest || len(a.Items) == len(set.Items)
		list = append(list, *a)
	}
	slices.SortFunc(list, func(a, b Archive) int { return strings.Compare(b.Run, a.Run) })
	return list, total
}

func (s *Server) backupView(ns string) any {
	cfg := s.store.Get()
	v := BackupView{Version: s.version, Namespace: ns, Time: time.Now().Format("15:04:05")}
	if cfg == nil {
		return v
	}
	v.Cluster = cfg.Cluster
	set, ok := s.store.Backup(ns)
	if !ok {
		v.Error = "no backups set up in " + ns
		return v
	}
	v.Set = set
	run := s.bk().Runner(set)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	syncErr := run.Sync(ctx, false)
	st, err := run.ReadState(ctx)
	if kube.IsOffline(err) {
		return v
	}
	v.Online = true
	if err != nil {
		v.Error = err.Error()
		return v
	}
	if syncErr != nil {
		v.Error = syncErr.Error()
	}
	v.Archives, v.Size = archives(set, st)
	v.IndexAt = st.IndexAt
	for _, r := range st.Runs {
		if r.Status == "running" && r.Kind == "scheduled" {
			v.Busy = true
		}
		r.Log = nil
		v.Runs = append(v.Runs, r)
	}
	if cur := run.Current(); cur != nil {
		v.Run, v.Busy = cur, true
	} else if len(st.Runs) > 0 {
		v.Run = &st.Runs[0]
	}
	return v
}

func (s *Server) bsnap(ns string) *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "backup:" + ns
	sn := s.snaps[key]
	if sn == nil {
		sn = newSnapshot(func() any { return s.backupView(ns) }, 2*time.Second, 15*time.Second)
		s.snaps[key] = sn
		go sn.loop()
	}
	return sn
}

func (s *Server) backupSet(r *http.Request) (config.Backup, error) {
	set, ok := s.store.Backup(r.URL.Query().Get("n"))
	if !ok {
		return set, badRequest("no backups in that namespace")
	}
	return set, nil
}

func (s *Server) backupState(w http.ResponseWriter, r *http.Request) {
	set, err := s.backupSet(r)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.bsnap(set.Namespace).get())
}

// backupClick starts a backup now or a listing.
func (s *Server) backupClick(w http.ResponseWriter, r *http.Request, kind string) {
	set, err := s.backupSet(r)
	if err == nil {
		run := s.bk().Runner(set)
		if kind == "backup" {
			err = run.Start(s.by(r))
		} else {
			err = run.Refresh(s.by(r))
		}
	}
	s.audit(r, s.store.Get().User, kind, set.Namespace, "", err)
	if err != nil {
		fail(w, err)
		return
	}
	s.bsnap(set.Namespace).refresh()
	w.WriteHeader(http.StatusNoContent)
}

type restoreForm struct {
	Run      string
	Items    []string      // a run of the set: the items to put back
	Import   []backup.Pick // or archives of another run, picked per item
	NoSafety bool
	Confirm  string // the namespace the page showed
}

func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
	var f restoreForm
	set, err := s.backupSet(r)
	if err == nil {
		err = json.NewDecoder(r.Body).Decode(&f)
	}
	if err == nil {
		err = s.restore(r, set, f)
	}
	what := "restore"
	if f.NoSafety {
		what = "restore-without-safety-backup"
	}
	s.audit(r, s.store.Get().User, what, set.Namespace, f.Run, err)
	if err != nil {
		fail(w, err)
		return
	}
	s.bsnap(set.Namespace).refresh()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restore(r *http.Request, set config.Backup, f restoreForm) error {
	if f.Confirm != set.Namespace {
		return badRequest("the page shows another namespace: reload it")
	}
	if !runPrefix.MatchString(f.Run + "_x") {
		return badRequest("no such run")
	}
	if busy := s.boxBusy(set.Namespace); busy != "" {
		return backup.Refused(busy + ": the restore waits for it")
	}
	run := s.bk().Runner(set)
	if len(f.Import) > 0 {
		return run.Restore(f.Run, f.Import, true, f.NoSafety, s.by(r))
	}
	var picks []backup.Pick
	for _, it := range f.Items {
		picks = append(picks, backup.Pick{Item: it})
	}
	return run.Restore(f.Run, picks, false, f.NoSafety, s.by(r))
}

// boxBusy names a box of the namespace that has a run.
func (s *Server) boxBusy(ns string) string {
	for _, b := range s.store.Get().Boxes {
		if b.Namespace != ns {
			continue
		}
		if cur, _ := s.eng().Runner(b).Current(); cur != nil {
			return "a " + cur.Kind + " of " + b.ID() + " is running"
		}
	}
	return ""
}

// --- the form ---

// hostZone is the time zone of the machine switchyard runs on.
func hostZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

type suggestion struct {
	Set       *config.Backup // the saved one, or the mirror a former switchyard left
	PVCs      []string
	Items     []config.Item // the databases found, and the PVCs that are not theirs
	Workloads []string      // for the hooks
	TimeZone  string
}

type envVar struct {
	Name      string
	Value     string
	ValueFrom *struct {
		SecretKeyRef *struct{ Name, Key string } `json:"secretKeyRef"`
	} `json:"valueFrom"`
}

// adminBackup: what the form starts with for a namespace.
func (s *Server) adminBackup(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("n")
	if err := checkName("namespace", ns, true); err != nil {
		fail(w, err)
		return
	}
	k, ctx, cancel, ok := s.adminCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	res := suggestion{TimeZone: hostZone()}
	if set, ok := s.store.Backup(ns); ok {
		res.Set = &set
	} else {
		var cm struct {
			Data map[string]string `json:"data"`
		}
		if k.Get(ctx, &cm, "--namespace="+ns, "get", "--", "configmap", backup.StateName) == nil && cm.Data["set"] != "" {
			var set config.Backup
			if json.Unmarshal([]byte(cm.Data["set"]), &set) == nil {
				set.Namespace = ns
				res.Set = &set
			}
		}
	}
	var pvcs struct {
		Items []struct{ Metadata kube.Meta }
	}
	if err := k.Get(ctx, &pvcs, "--namespace="+ns, "get", "persistentvolumeclaims"); err != nil {
		fail(w, err)
		return
	}
	res.PVCs = names(pvcs.Items)
	var sts struct {
		Items []struct {
			Metadata kube.Meta
			Spec     struct {
				ServiceName          string `json:"serviceName"`
				VolumeClaimTemplates []struct{ Metadata kube.Meta }
				Template             struct {
					Spec struct {
						Containers []struct {
							Image string
							Env   []envVar
						}
						Volumes []struct {
							PersistentVolumeClaim *struct{ ClaimName string } `json:"persistentVolumeClaim"`
						}
					}
				}
			}
		}
	}
	dbClaims := map[string]bool{}
	if k.Get(ctx, &sts, "--namespace="+ns, "get", "statefulsets") == nil {
		var dbs []config.Item
		for _, st := range sts.Items {
			res.Workloads = append(res.Workloads, "statefulset/"+st.Metadata.Name)
			if len(st.Spec.Template.Spec.Containers) == 0 {
				continue
			}
			c := st.Spec.Template.Spec.Containers[0]
			it, ok := dbItem(c.Image, c.Env)
			if !ok {
				continue
			}
			it.StatefulSet, it.Service = st.Metadata.Name, st.Spec.ServiceName
			if it.Service == "" {
				it.Service = st.Metadata.Name
			}
			dbs = append(dbs, it)
			for _, v := range st.Spec.Template.Spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					dbClaims[v.PersistentVolumeClaim.ClaimName] = true
				}
			}
			for _, t := range st.Spec.VolumeClaimTemplates {
				for _, p := range res.PVCs {
					if strings.HasPrefix(p, t.Metadata.Name+"-"+st.Metadata.Name+"-") {
						dbClaims[p] = true
					}
				}
			}
		}
		for i := range dbs {
			dbs[i].Name = "database"
			if len(dbs) > 1 {
				dbs[i].Name = strings.ToLower(dbs[i].Database)
			}
		}
		res.Items = dbs
	}
	for _, p := range res.PVCs {
		if !dbClaims[p] {
			res.Items = append(res.Items, config.Item{Name: p, Kind: "volume", PVC: p})
		}
	}
	var deps struct {
		Items []struct{ Metadata kube.Meta }
	}
	if k.Get(ctx, &deps, "--namespace="+ns, "get", "deployments") == nil {
		for _, d := range deps.Items {
			if !strings.HasSuffix(d.Metadata.Name, "-canary") {
				res.Workloads = append(res.Workloads, "deployment/"+d.Metadata.Name)
			}
		}
	}
	slices.Sort(res.Workloads)
	writeJSON(w, res)
}

// dbItem reads a database StatefulSet's image and environment, the official images' variables.
func dbItem(image string, env []envVar) (config.Item, bool) {
	val := func(names ...string) (string, string, string) {
		for _, e := range env {
			if slices.Contains(names, e.Name) {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					return "", e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key
				}
				return e.Value, "", ""
			}
		}
		return "", "", ""
	}
	low := strings.ToLower(image)
	switch {
	case strings.Contains(low, "mariadb") || strings.Contains(low, "mysql"):
		it := config.Item{Kind: "mariadb"}
		it.Database, _, _ = val("MARIADB_DATABASE", "MYSQL_DATABASE")
		it.User, _, _ = val("MARIADB_USER", "MYSQL_USER")
		_, it.Secret, it.PasswordKey = val("MARIADB_PASSWORD", "MYSQL_PASSWORD")
		_, _, it.RootKey = val("MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD")
		return it, true
	case strings.Contains(low, "postgres"):
		it := config.Item{Kind: "postgres"}
		it.User, _, _ = val("POSTGRES_USER")
		if it.User == "" {
			it.User = "postgres"
		}
		it.Database, _, _ = val("POSTGRES_DB")
		if it.Database == "" {
			it.Database = it.User
		}
		_, it.Secret, it.PasswordKey = val("POSTGRES_PASSWORD")
		it.RootUser, it.RootKey = it.User, it.PasswordKey
		return it, true
	}
	return config.Item{}, false
}

var (
	secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
	cronExpr  = regexp.MustCompile(`^(@(yearly|annually|monthly|weekly|daily|midnight|hourly)|[0-9*/,-]+( [0-9*/,A-Za-z-]+){4})$`)
	subPath   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)
	imageRef  = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._/:@-]+$`)
)

func checkBackup(b *config.Backup) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, badRequest(fmt.Sprintf(format, a...))) }
	errs = append(errs, checkName("namespace", b.Namespace, true), checkName("target PVC", b.Target, false))
	if len(b.Items) == 0 {
		bad("pick at least one item")
	}
	seen := map[string]bool{}
	for _, it := range b.Items {
		if !backup.ItemName.MatchString(it.Name) || it.Name == "manifest" || seen[it.Name] {
			bad("an item is named with a-z, 0-9, _ . - and once only, not %q", it.Name)
		}
		seen[it.Name] = true
		switch it.Kind {
		case "mariadb", "postgres":
			errs = append(errs, checkName("service", it.Service, false))
			if it.StatefulSet != "" {
				errs = append(errs, checkName("StatefulSet", it.StatefulSet, false))
			}
			errs = append(errs, checkName("Secret", it.Secret, false))
			for _, x := range []string{it.Database, it.User} {
				if !backup.DBName.MatchString(x) {
					bad("%s: %q is not a database or user name switchyard takes (A-Z, a-z, 0-9, _ -)", it.Name, x)
				}
			}
			if it.RootUser != "" && !backup.DBName.MatchString(it.RootUser) {
				bad("%s: %q is not a user name switchyard takes", it.Name, it.RootUser)
			}
			if !secretKey.MatchString(it.PasswordKey) || !secretKey.MatchString(it.RootKey) {
				bad("%s needs the Secret's keys of the password and of the restore user's password", it.Name)
			}
		case "volume":
			errs = append(errs, checkName("PVC", it.PVC, false))
			if it.PVC == b.Target {
				bad("%s is the target itself", it.Name)
			}
			if it.SubPath != "" && (!subPath.MatchString(it.SubPath) || strings.Contains(it.SubPath, "..")) {
				bad("%s: the sub-path %q is a directory in the volume", it.Name, it.SubPath)
			}
			for _, e := range it.Exclude {
				if e == "" || strings.ContainsAny(e, "\n\r") {
					bad("%s: an empty exclude", it.Name)
				}
			}
		default:
			bad("%s: the kind is mariadb, postgres or volume", it.Name)
		}
	}
	for name, h := range map[string]*config.Hook{"before": b.Before, "after": b.After} {
		if h == nil {
			continue
		}
		kind, wname, _ := strings.Cut(h.Workload, "/")
		if kind != "deployment" && kind != "statefulset" || !kube.ValidName(wname) || strings.TrimSpace(h.Command) == "" {
			bad("the %s hook is a workload (deployment/<name> or statefulset/<name>) and a command", name)
		}
	}
	b.Schedule = strings.Join(strings.Fields(b.Schedule), " ")
	if b.Schedule != "" && !cronExpr.MatchString(b.Schedule) {
		bad("the schedule is a cron expression, e.g. 10 5 * * *, not %q", b.Schedule)
	}
	if b.TimeZone != "" {
		if _, err := time.LoadLocation(b.TimeZone); err != nil {
			bad("no time zone %q", b.TimeZone)
		}
	}
	if b.Tools != "" && !imageRef.MatchString(b.Tools) {
		bad("the tools image %q is an image reference", b.Tools)
	}
	return errors.Join(errs...)
}

// adminBackupSave adds or changes a namespace's backup set: the config, the Role, the CronJob
// and the mirror in the namespace.
func (s *Server) adminBackupSave(w http.ResponseWriter, r *http.Request) {
	var b config.Backup
	err := json.NewDecoder(r.Body).Decode(&b)
	if err == nil {
		err = s.saveBackup(r, &b)
	}
	s.audit(r, s.store.Get().User, "backup-saved", b.Namespace, "", err)
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveBackup(r *http.Request, b *config.Backup) error {
	if err := checkBackup(b); err != nil {
		return err
	}
	if run := s.bk().Runner(*b).Current(); run != nil {
		return backup.Refused("a " + run.Kind + " is running")
	}
	k, err := s.admin()
	if err != nil {
		return badRequest(err.Error())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var pvc struct{ Metadata kube.Meta }
	if err := k.Get(ctx, &pvc, "--namespace="+b.Namespace, "get", "--", "persistentvolumeclaim", b.Target); err != nil {
		return err
	}
	err = s.store.Update(func(cfg *config.Config) error {
		cfg.Backups = slices.DeleteFunc(cfg.Backups, func(x config.Backup) bool { return x.Namespace == b.Namespace })
		cfg.Backups = append(cfg.Backups, *b)
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.writeRole(ctx, k, b.Namespace); err != nil {
		return err
	}
	run := s.bk().Runner(*b)
	if err := run.SaveSet(ctx); err != nil {
		return err
	}
	return run.Sync(ctx, true)
}

func (s *Server) adminBackupRemove(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("n")
	err := s.removeBackup(r, ns)
	s.audit(r, s.store.Get().User, "backup-removed", ns, "", err)
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeBackup(r *http.Request, ns string) error {
	set, ok := s.store.Backup(ns)
	if !ok {
		return badRequest("no backups in " + ns)
	}
	k, err := s.admin()
	if err != nil {
		return badRequest(err.Error())
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.bk().Runner(set).Remove(ctx); err != nil {
		return err
	}
	err = s.store.Update(func(cfg *config.Config) error {
		cfg.Backups = slices.DeleteFunc(cfg.Backups, func(x config.Backup) bool { return x.Namespace == ns })
		return nil
	})
	if err != nil {
		return err
	}
	return s.writeRole(ctx, k, ns)
}

// syncBackups keeps every set's CronJob current and reads its runs, once an hour.
func (s *Server) syncBackups() {
	for {
		time.Sleep(time.Hour)
		cfg := s.store.Get()
		for _, set := range cfg.Backups {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := s.bk().Runner(set).Sync(ctx, true); err != nil {
				log.Printf("%s backup: sync: %v", set.Namespace, err)
			}
			cancel()
		}
	}
}
