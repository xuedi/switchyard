// Package web is switchyard's pages: the installer on the first run, the login, the boxes and
// one box's view, with the JSON they poll and the clicks they send. Every link is relative, so
// it works behind any reverse-proxy prefix.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xuedi/switchyard/internal/auth"
	"github.com/xuedi/switchyard/internal/backup"
	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/deploy"
	"github.com/xuedi/switchyard/internal/kube"
	"github.com/xuedi/switchyard/internal/stack"
)

//go:embed pages
var pages embed.FS

type Server struct {
	version    string
	logo       []byte
	store      *config.Store
	listen     string
	kubeconfig string // the installer's admin kubeconfig
	trusted    proxies
	limiter    auth.Limiter
	token      string // the installer's, printed by switchyard setup; "" keeps it closed

	mu      sync.Mutex
	engine  *deploy.Engine
	backups *backup.Engine
	stacks  *stack.Engine
	snaps   map[string]*snapshot
	usage   map[string]stack.Use // metrics-server's, read at most every 15 s
	usageAt time.Time
	power   powerRun
}

// Options: how switchyard was started.
type Options struct {
	Listen     string
	Kubeconfig string // the installer's admin kubeconfig, "" for the default
	Setup      bool   // switchyard setup: the installer is open
}

// Serve runs switchyard: the installer until there is a config, the boxes afterwards.
func Serve(version string, logo []byte, o Options) error {
	store, err := config.Load()
	if err != nil {
		return err
	}
	listen := o.Listen
	s := &Server{version: version, logo: logo, store: store, snaps: map[string]*snapshot{}, kubeconfig: o.Kubeconfig}
	if s.kubeconfig == "" {
		s.kubeconfig = defaultKubeconfig()
	}
	cfg := store.Get()
	if cfg != nil {
		if s.trusted, err = parseProxies(cfg.TrustedProxies); err != nil {
			return err
		}
		s.start(cfg)
		if listen == "" {
			listen = cfg.Listen
		}
	} else {
		s.trusted, _ = parseProxies(nil)
	}
	if listen == "" {
		listen = "127.0.0.1:8083"
	}
	s.listen = listen
	srv := &http.Server{Addr: listen, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 2 * time.Minute}
	log.Printf("switchyard %s listening on %s", version, listen)
	switch {
	case cfg != nil:
	case o.Setup:
		s.token = auth.NewSecret()[:32]
		log.Printf("to set switchyard up, open http://%s/?token=%s", listen, s.token)
	default:
		log.Printf("no %s yet: run switchyard setup", config.Path())
	}
	return srv.ListenAndServe()
}

// start brings up the engine on the scoped kubeconfig, and lets runs cut off by a restart go.
func (s *Server) start(cfg *config.Config) {
	k := kube.Kube{Bin: cfg.Kubectl, Kubeconfig: config.Kubeconfig()}
	e := deploy.NewEngine(k)
	bk := backup.NewEngine(k, func(ns string) string { return first(s.boxBusy(ns), s.stk().Running(ns)) })
	st := stack.NewEngine(k, func(ns string) string { return first(s.boxBusy(ns), s.bk().Busy(ns)) })
	s.mu.Lock()
	s.engine, s.backups, s.stacks = e, bk, st
	s.mu.Unlock()
	for _, b := range cfg.Boxes {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			e.Runner(b).Recover(ctx)
		}()
	}
	for _, set := range cfg.Backups {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			bk.Runner(set).Recover(ctx)
		}()
	}
	go s.syncBackups()
}

func (s *Server) bk() *backup.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backups
}

func (s *Server) eng() *deploy.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

func (s *Server) stk() *stack.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stacks
}

// first is the first of the strings that is not "".
func first(a ...string) string {
	for _, x := range a {
		if x != "" {
			return x
		}
	}
	return ""
}

func page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := pages.ReadFile("pages/" + name)
		switch {
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(name, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Write(b)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// fail answers a click or a form: 409 for a refusal, 502 for anything the cluster said.
func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var ref deploy.Refused
	var bref backup.Refused
	var sref stack.Refused
	var bad badRequest
	switch {
	case errors.As(err, &ref) || errors.As(err, &bref) || errors.As(err, &sref):
		code = http.StatusConflict
	case errors.As(err, &bad):
		code = http.StatusBadRequest
	}
	http.Error(w, err.Error(), code)
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	open := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	// before the setup only the installer, with its token, after it only with a session
	setup := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if s.store.Get() != nil {
				http.Error(w, "switchyard is set up already", http.StatusConflict)
				return
			}
			if !s.setupToken(r.Header.Get("X-Setup-Token")) {
				http.Error(w, "the installer needs the URL that switchyard setup printed", http.StatusForbidden)
				return
			}
			h(w, r)
		})
	}
	private := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.session(w, r) {
				http.Error(w, "log in first", http.StatusUnauthorized)
				return
			}
			h(w, r)
		})
	}
	privatePage := func(pattern, name string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.session(w, r) {
				redirect(w, "./")
				return
			}
			page(name)(w, r)
		})
	}

	open("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.store.Get() == nil && s.setupToken(r.URL.Query().Get("token")):
			page("setup.html")(w, r)
		case s.store.Get() == nil:
			http.Error(w, "switchyard is not set up: run switchyard setup and open the URL it prints", http.StatusForbidden)
		case !s.session(w, r):
			redirect(w, "login")
		default:
			page("boxes.html")(w, r)
		}
	})
	privatePage("GET /box", "box.html")
	privatePage("GET /password", "password.html")
	privatePage("GET /backups", "backups.html")
	privatePage("GET /running", "running.html")
	open("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if cfg := s.store.Get(); cfg != nil && cfg.NoLogin {
			redirect(w, "./")
			return
		}
		page("login.html")(w, r)
	})
	open("POST /login", s.login)
	open("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if s.session(w, r) {
			s.audit(r, s.store.Get().User, "logout", "", "", nil)
		}
		auth.ClearSession(w, s.trusted.https(r))
		redirect(w, "login")
	})
	private("POST /logout-all", s.logoutAll)
	private("POST /password", s.password)
	private("POST /login-off", func(w http.ResponseWriter, r *http.Request) { s.loginSwitch(w, r, true) })
	private("POST /login-on", func(w http.ResponseWriter, r *http.Request) { s.loginSwitch(w, r, false) })
	private("GET /login-state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"Login": !s.store.Get().NoLogin})
	})
	for _, f := range []string{"style.css", "login.js", "setup.js", "boxes.js", "box.js", "password.js", "backups.js", "running.js", "power.js"} {
		open("GET /"+f, page(f))
	}
	open("GET /logo.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(s.logo)
	})
	open("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok " + s.version + "\n"))
	})

	setup("GET /setup/check", s.setupCheck)
	setup("POST /setup/test", s.setupTest)
	setup("POST /setup", s.setupRun)

	private("GET /boxes", s.boxes)
	private("GET /state", s.state)
	for _, kind := range []string{deploy.Canary, deploy.Quick, deploy.Rollback} {
		private("POST /"+kind, func(w http.ResponseWriter, r *http.Request) { s.click(w, r, kind) })
	}
	private("POST /abort", func(w http.ResponseWriter, r *http.Request) { s.click(w, r, "abort") })
	private("GET /admin/namespaces", s.adminNamespaces)
	private("GET /admin/deployments", s.adminDeployments)
	private("GET /admin/details", s.adminDetails)
	private("GET /admin/box", s.adminBox)
	private("POST /admin/box", s.adminSave)
	private("POST /admin/remove", s.adminRemove)
	private("GET /backup/state", s.backupState)
	private("POST /backup/run", func(w http.ResponseWriter, r *http.Request) { s.backupClick(w, r, "backup") })
	private("POST /backup/refresh", func(w http.ResponseWriter, r *http.Request) { s.backupClick(w, r, "refresh") })
	private("POST /backup/resume", func(w http.ResponseWriter, r *http.Request) { s.backupClick(w, r, "resume") })
	private("POST /backup/restore", s.backupRestore)
	private("GET /admin/backup", s.adminBackup)
	private("POST /admin/backup", s.adminBackupSave)
	private("POST /admin/backup/remove", s.adminBackupRemove)
	private("GET /running/state", s.runningState)
	private("POST /running/start", func(w http.ResponseWriter, r *http.Request) { s.runningClick(w, r, "start") })
	private("POST /running/stop", func(w http.ResponseWriter, r *http.Request) { s.runningClick(w, r, "stop") })
	private("POST /running/enable", s.runningEnable)
	private("GET /power", s.powerState)
	private("POST /power", s.powerSave)
	private("POST /power/on", s.powerOn)

	// A click is a real deploy: no other site may trigger one through the browser.
	return headers(http.NewCrossOriginProtection().Handler(mux))
}

// headers: no script but the pages' own, no framing by other sites, small bodies.
func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h.ServeHTTP(w, r)
	})
}

// audit logs one line per action: who, from where, what, on which box, the result.
func (s *Server) audit(r *http.Request, user, action, box, tag string, err error) {
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	log.Printf("audit user=%q client=%s action=%s box=%q tag=%q result=%q", user, s.trusted.client(r), action, box, tag, result)
}

// by is the user and the client, for a run's log.
func (s *Server) by(r *http.Request) string {
	return s.store.Get().User + " from " + s.trusted.client(r)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	if cfg == nil || cfg.NoLogin {
		redirect(w, "./")
		return
	}
	ok, locked := s.limiter.Try(s.trusted.client(r), func() bool {
		user := auth.Same(r.FormValue("user"), cfg.User)
		return auth.Check(cfg.Hash, r.FormValue("password")) && user
	})
	if !ok {
		why, msg := "1", "wrong user or password"
		if locked {
			why, msg = "locked", "locked out"
		}
		s.audit(r, r.FormValue("user"), "login", "", "", errors.New(msg))
		redirect(w, "login?failed="+why)
		return
	}
	s.audit(r, cfg.User, "login", "", "", nil)
	s.newSession(w, r, cfg)
	redirect(w, "./")
}

// redirect sends the relative Location as it is: http.Redirect makes it absolute, which leaves
// a reverse proxy's prefix.
func redirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) setupToken(t string) bool { return s.token != "" && auth.Same(t, s.token) }

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	now := time.Now()
	auth.SetSession(w, cfg.Secret, auth.Session{User: cfg.User, Generation: cfg.Generation, Start: now, Seen: now}, s.trusted.https(r))
}

// session: the request has a live session, or the login is off. A request in the top directory
// renews it when its last request is a while ago, so it ends after auth.Idle without one; a
// request below drops a cookie of its own directory that is stale or beside another.
func (s *Server) session(w http.ResponseWriter, r *http.Request) bool {
	cfg := s.store.Get()
	if cfg == nil {
		return false
	}
	if cfg.NoLogin {
		return true
	}
	list, n := auth.ReadSessions(r, cfg.Secret)
	now := time.Now()
	var ses auth.Session
	live := false
	for _, x := range list {
		if x.Live(cfg.User, cfg.Generation, now) {
			ses, live = x, true
			break
		}
	}
	top := strings.Count(r.URL.Path, "/") == 1
	if !top && (n > 1 || n == 1 && !live) {
		auth.ClearSession(w, s.trusted.https(r))
	}
	if !live {
		return false
	}
	if top && now.Sub(ses.Seen) > 5*time.Minute {
		ses.Seen = now
		auth.SetSession(w, cfg.Secret, ses, s.trusted.https(r))
	}
	return true
}

// loginSwitch turns the login off, with the password as for a change, or on again, keeping this
// browser logged in.
func (s *Server) loginSwitch(w http.ResponseWriter, r *http.Request, off bool) {
	cfg := s.store.Get()
	action := "login-on"
	if off {
		action = "login-off"
		if !s.checkOld(w, r, action) {
			return
		}
	}
	err := s.store.Update(func(c *config.Config) error { c.NoLogin = off; return nil })
	s.audit(r, cfg.User, action, "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	if !off {
		s.newSession(w, r, s.store.Get())
	}
	redirect(w, "password?"+action+"=1")
}

// logoutAll ends every session, this one too.
func (s *Server) logoutAll(w http.ResponseWriter, r *http.Request) {
	err := s.store.Update(func(c *config.Config) error { c.Generation++; return nil })
	s.audit(r, s.store.Get().User, "logout-everywhere", "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	auth.ClearSession(w, s.trusted.https(r))
	redirect(w, "login")
}

// checkOld: the form's current password is right; otherwise the answer is sent already.
func (s *Server) checkOld(w http.ResponseWriter, r *http.Request, action string) bool {
	cfg := s.store.Get()
	ok, locked := s.limiter.Try(s.trusted.client(r), func() bool { return auth.Check(cfg.Hash, r.FormValue("old")) })
	if !ok {
		why, msg := "old", "wrong password"
		if locked {
			why, msg = "locked", "locked out"
		}
		s.audit(r, cfg.User, action, "", "", errors.New(msg))
		redirect(w, "password?failed="+why)
	}
	return ok
}

// password changes the password and ends every other session.
func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	if !s.checkOld(w, r, "password-change") {
		return
	}
	pw := r.FormValue("new")
	if len(pw) < 8 || pw != r.FormValue("again") {
		redirect(w, "password?failed=new")
		return
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		fail(w, err)
		return
	}
	err = s.store.Update(func(c *config.Config) error { c.Hash = hash; c.Generation++; return nil })
	s.audit(r, cfg.User, "password-change", "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	s.newSession(w, r, s.store.Get())
	redirect(w, "password?changed=1")
}

// click starts a run (or aborts one): 204, or the reason as text.
func (s *Server) click(w http.ResponseWriter, r *http.Request, kind string) {
	b, ok := s.store.Box(r.URL.Query().Get("b"))
	if !ok {
		fail(w, badRequest("no such box"))
		return
	}
	run := s.eng().Runner(b)
	tag := r.URL.Query().Get("tag")
	var err error
	switch {
	case kind == "abort":
		err = run.Abort(s.by(r))
	case s.bk().Busy(b.Namespace) != "":
		err = deploy.Refused(s.bk().Busy(b.Namespace) + " is running in " + b.Namespace + ": the " + kind + " waits for it")
	case s.stk().Running(b.Namespace) != "":
		err = deploy.Refused(s.stk().Running(b.Namespace) + " is running: the " + kind + " waits for it")
	case s.stopped(r.Context(), b):
		err = deploy.Refused(b.ID() + " is stopped: start it on Running first")
	default:
		err = run.Start(kind, tag, r.URL.Query().Get("allow") == "1", s.by(r))
	}
	s.audit(r, s.store.Get().User, kind, b.ID(), tag, err)
	if err != nil {
		fail(w, err)
		return
	}
	s.snap(b.ID()).refresh()
	w.WriteHeader(http.StatusNoContent)
}

// stopped: the box's Deployment is scaled to 0.
func (s *Server) stopped(ctx context.Context, b config.Box) bool {
	var d kube.Deployment
	err := s.eng().K.Get(ctx, &d, "--namespace="+b.Namespace, "get", "--", "deployment", b.Deployment)
	return err == nil && d.Replicas() == 0
}

// snap is the box's shared snapshot.
func (s *Server) snap(id string) *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := s.snaps[id]
	if sn == nil {
		sn = newSnapshot(func() any { return s.view(id) }, time.Second, 15*time.Second)
		s.snaps[id] = sn
		go sn.loop()
	}
	return sn
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("b")
	if _, ok := s.store.Box(id); !ok {
		fail(w, badRequest("no such box"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.snap(id).get())
}

// boxes is the list page's data: every box's view without the logs and the tags.
func (s *Server) boxes(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	list := []json.RawMessage{}
	for _, b := range cfg.Boxes {
		list = append(list, s.snap(b.ID()).get())
	}
	backups := []json.RawMessage{}
	for _, set := range cfg.Backups {
		backups = append(backups, s.bsnap(set.Namespace).get())
	}
	writeJSON(w, map[string]any{"Version": s.version, "Login": !cfg.NoLogin, "Cluster": cfg.Cluster, "Online": s.eng().K.Online(r.Context()),
		"Boxes": list, "Backups": backups})
}
