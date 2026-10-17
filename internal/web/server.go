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
	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/deploy"
	"github.com/xuedi/switchyard/internal/kube"
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

	mu     sync.Mutex
	engine *deploy.Engine
	snaps  map[string]*snapshot
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
	log.Printf("switchyard %s listening on %s", version, listen)
	switch {
	case cfg != nil:
	case o.Setup:
		s.token = auth.NewSecret()[:32]
		log.Printf("to set switchyard up, open http://%s/?token=%s", listen, s.token)
	default:
		log.Printf("no %s yet: run switchyard setup", config.Path())
	}
	return http.ListenAndServe(listen, s.routes())
}

// start brings up the engine on the scoped kubeconfig, and lets runs cut off by a restart go.
func (s *Server) start(cfg *config.Config) {
	e := deploy.NewEngine(kube.Kube{Bin: cfg.Kubectl, Kubeconfig: config.Kubeconfig()})
	s.mu.Lock()
	s.engine = e
	s.mu.Unlock()
	for _, b := range cfg.Boxes {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			e.Runner(b).Recover(ctx)
		}()
	}
}

func (s *Server) eng() *deploy.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

func page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := pages.ReadFile("pages/" + name)
		switch {
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
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
	var bad badRequest
	switch {
	case errors.As(err, &ref):
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
				http.Redirect(w, r, "./", http.StatusSeeOther)
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
			http.Redirect(w, r, "login", http.StatusSeeOther)
		default:
			page("boxes.html")(w, r)
		}
	})
	privatePage("GET /box", "box.html")
	privatePage("GET /password", "password.html")
	open("GET /login", page("login.html"))
	open("POST /login", s.login)
	open("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		auth.ClearSession(w, s.trusted.https(r))
		http.Redirect(w, r, "login", http.StatusSeeOther)
	})
	private("POST /logout/all", s.logoutAll)
	private("POST /password", s.password)
	open("GET /style.css", page("style.css"))
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

	// A click is a real deploy: no other site may trigger one through the browser.
	return http.NewCrossOriginProtection().Handler(mux)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	if cfg == nil {
		http.Redirect(w, r, "./", http.StatusSeeOther)
		return
	}
	ok, locked := s.limiter.Try(s.trusted.client(r), func() bool {
		user := auth.Same(r.FormValue("user"), cfg.User)
		return auth.Check(cfg.Hash, r.FormValue("password")) && user
	})
	if !ok {
		why := "1"
		if locked {
			why = "locked"
		}
		http.Redirect(w, r, "login?failed="+why, http.StatusSeeOther)
		return
	}
	s.newSession(w, r, cfg)
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

func (s *Server) setupToken(t string) bool { return s.token != "" && auth.Same(t, s.token) }

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	now := time.Now()
	auth.SetSession(w, cfg.Secret, auth.Session{User: cfg.User, Generation: cfg.Generation, Start: now, Seen: now}, s.trusted.https(r))
}

// session: the request has a live session; it is renewed when its last request is a while ago,
// so it ends after auth.Idle without one.
func (s *Server) session(w http.ResponseWriter, r *http.Request) bool {
	cfg := s.store.Get()
	if cfg == nil {
		return false
	}
	ses, ok := auth.ReadSession(r, cfg.Secret)
	now := time.Now()
	if !ok || !ses.Live(cfg.User, cfg.Generation, now) {
		return false
	}
	if now.Sub(ses.Seen) > 5*time.Minute {
		ses.Seen = now
		auth.SetSession(w, cfg.Secret, ses, s.trusted.https(r))
	}
	return true
}

// logoutAll ends every session, this one too.
func (s *Server) logoutAll(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Update(func(c *config.Config) error { c.Generation++; return nil }); err != nil {
		fail(w, err)
		return
	}
	auth.ClearSession(w, s.trusted.https(r))
	http.Redirect(w, r, "login", http.StatusSeeOther)
}

// password changes the password and ends every other session.
func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	ok, locked := s.limiter.Try(s.trusted.client(r), func() bool { return auth.Check(cfg.Hash, r.FormValue("old")) })
	if !ok {
		why := "old"
		if locked {
			why = "locked"
		}
		http.Redirect(w, r, "password?failed="+why, http.StatusSeeOther)
		return
	}
	pw := r.FormValue("new")
	if len(pw) < 8 || pw != r.FormValue("again") {
		http.Redirect(w, r, "password?failed=new", http.StatusSeeOther)
		return
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.store.Update(func(c *config.Config) error { c.Hash = hash; c.Generation++; return nil }); err != nil {
		fail(w, err)
		return
	}
	s.newSession(w, r, s.store.Get())
	http.Redirect(w, r, "password?changed=1", http.StatusSeeOther)
}

// click starts a run (or aborts one): 204, or the reason as text.
func (s *Server) click(w http.ResponseWriter, r *http.Request, kind string) {
	b, ok := s.store.Box(r.URL.Query().Get("b"))
	if !ok {
		fail(w, badRequest("no such box"))
		return
	}
	run := s.eng().Runner(b)
	var err error
	if kind == "abort" {
		err = run.Abort()
	} else {
		err = run.Start(kind, r.URL.Query().Get("tag"), r.URL.Query().Get("allow") == "1")
	}
	if err != nil {
		log.Printf("%s: %s: %v", b.ID(), kind, err)
		fail(w, err)
		return
	}
	log.Printf("%s: %s %s", b.ID(), kind, r.URL.Query().Get("tag"))
	s.snap(b.ID()).refresh()
	w.WriteHeader(http.StatusNoContent)
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
	writeJSON(w, map[string]any{"Version": s.version, "Cluster": cfg.Cluster, "Boxes": list})
}
