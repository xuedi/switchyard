package web

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/xuedi/switchyard/internal/kube"
)

// RunView is one run with its log, for the run page: a box's deploy, a backup set's run or
// Running's last Start or Stop in a namespace. Without r it is the running one, else the newest.
type RunView struct {
	Version   string
	Cluster   string
	Online    bool
	Error     string
	Of        string // the box or the namespace
	Back      string // the page it belongs to
	Run       any
	Running   bool
	Abortable bool
}

func (s *Server) runState(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cfg := s.store.Get()
	v := RunView{Version: s.version, Cluster: cfg.Cluster, Online: true}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id := q.Get("r")
	switch {
	case q.Has("b"):
		b, ok := s.store.Box(q.Get("b"))
		if !ok {
			fail(w, badRequest("no such box"))
			return
		}
		v.Of, v.Back = b.ID(), "box?b="+url.QueryEscape(b.ID())
		run := s.eng().Runner(b)
		if cur, abortable := run.Current(); cur != nil && (id == "" || id == cur.Started) {
			v.Run, v.Running, v.Abortable = cur, true, abortable
			break
		}
		st, err := run.ReadState(ctx)
		if err != nil {
			v.Online, v.Error = !kube.IsOffline(err), err.Error()
			break
		}
		for _, x := range st.All() {
			if id == "" || x.Started == id {
				v.Run = x
				break
			}
		}
	case q.Has("n"):
		set, ok := s.store.Backup(q.Get("n"))
		if !ok {
			fail(w, badRequest("no backups in that namespace"))
			return
		}
		v.Of, v.Back = set.Namespace, "backups?n="+url.QueryEscape(set.Namespace)
		run := s.bk().Runner(set)
		if cur := run.Current(); cur != nil && (id == "" || id == cur.Started) {
			v.Run, v.Running = cur, true
			break
		}
		st, err := run.ReadState(ctx)
		if err != nil {
			v.Online, v.Error = !kube.IsOffline(err), err.Error()
			break
		}
		for _, x := range st.Runs {
			if id == "" || x.Started == id {
				v.Run, v.Running = x, x.Status == "running"
				break
			}
		}
	case q.Has("s"):
		v.Of, v.Back = q.Get("s"), "running"
		if last := s.stk().Last(v.Of); last != nil {
			v.Run, v.Running = last, last.Status == "running"
		}
	default:
		fail(w, badRequest("which run?"))
		return
	}
	if v.Run == nil && v.Error == "" {
		v.Error = "no such run (only the last ones are kept)"
	}
	writeJSON(w, v)
}
