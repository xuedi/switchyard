package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xuedi/switchyard/internal/config"
)

// powerRun is the power-on command's last run, kept in memory.
type powerRun struct {
	mu      sync.Mutex
	running bool
	since   time.Time // started fine, until the cluster answers
	out     string
	err     string
}

type PowerView struct {
	Set, Online, Running bool
	Since, Out, Error    string
	Command, User        string
	PasswordSet          bool
}

func (s *Server) powerState(w http.ResponseWriter, r *http.Request) {
	p := s.store.Get().Power
	online := s.eng().K.Online(r.Context())
	pr := &s.power
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if online {
		pr.since = time.Time{}
	}
	v := PowerView{Set: p.Command != "", Online: online, Running: pr.running, Out: pr.out, Error: pr.err,
		Command: p.Command, User: p.User, PasswordSet: p.Password != ""}
	if !pr.since.IsZero() {
		v.Since = pr.since.Format("15:04")
	}
	writeJSON(w, v)
}

// powerSave sets the command, with the current password: it runs on this machine. An empty
// command removes it with its user and password; an empty password keeps the one set.
func (s *Server) powerSave(w http.ResponseWriter, r *http.Request) {
	if !s.checkOld(w, r, "power-set") {
		return
	}
	p := config.Power{Command: strings.TrimSpace(r.FormValue("command")), User: strings.TrimSpace(r.FormValue("puser")), Password: r.FormValue("ppassword")}
	err := s.store.Update(func(c *config.Config) error {
		switch {
		case p.Command == "":
			p = config.Power{}
		case p.Password == "":
			p.Password = c.Power.Password
		}
		c.Power = p
		return nil
	})
	s.audit(r, s.store.Get().User, "power-set", "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	redirect(w, "password?power=1")
}

// powerOn runs the command once, while the cluster does not answer.
func (s *Server) powerOn(w http.ResponseWriter, r *http.Request) {
	p := s.store.Get().Power
	pr := &s.power
	err := func() error {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		switch {
		case p.Command == "":
			return badRequest("no power-on command is set")
		case pr.running:
			return badRequest("the power-on command is running already")
		case s.eng().K.Online(r.Context()):
			return badRequest("the cluster answers already")
		}
		pr.running, pr.since, pr.out, pr.err = true, time.Time{}, "", ""
		return nil
	}()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, "sh", "-c", p.Command)
		cmd.Env = append(os.Environ(), "SY_POWER_USER="+p.User, "SY_POWER_PASSWORD="+p.Password)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		out, runErr := cmd.CombinedOutput()
		if ctx.Err() != nil {
			runErr = errors.New("it took longer than a minute")
		}
		cancel()
		text := strings.TrimSpace(string(out))
		if len(text) > 2000 {
			text = "..." + text[len(text)-2000:]
		}
		pr.mu.Lock()
		pr.running, pr.out = false, text
		if runErr != nil {
			pr.err = "power-on: " + runErr.Error()
		} else {
			pr.since = time.Now()
		}
		pr.mu.Unlock()
		err = runErr
	}
	s.audit(r, s.store.Get().User, "power-on", "", "", err)
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
