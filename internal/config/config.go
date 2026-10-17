// Package config is switchyard's one file, config.json in ~/.config/switchyard (0600), written
// only by switchyard itself: the admin login, how to reach the cluster, the boxes.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Config struct {
	Listen string `json:"listen"`
	User   string `json:"user"`
	Hash   string `json:"hash"`   // the admin password, PBKDF2
	Secret string `json:"secret"` // signs the session cookie

	Generation     int      `json:"generation,omitempty"`     // in every session: one more ends them all
	TrustedProxies []string `json:"trustedProxies,omitempty"` // whose X-Forwarded-* count, "" is the loopback

	Kubectl         string `json:"kubectl"`
	AdminKubeconfig string `json:"adminKubeconfig,omitempty"` // "" is kubectl's default
	AdminContext    string `json:"adminContext"`              // used only to add a box in a new namespace
	Cluster         string `json:"cluster"`                   // its name on the page

	Boxes   []Box    `json:"boxes"`
	Backups []Backup `json:"backups,omitempty"`
}

// Box is one app: a Deployment whose image switchyard moves from tag to tag.
type Box struct {
	Namespace  string   `json:"namespace"`
	Deployment string   `json:"deployment"`
	Container  string   `json:"container,omitempty"` // "" is the first one
	Followers  []string `json:"followers,omitempty"` // cronjob/<name> or deployment/<name>, moved along
	PendingSQL string   `json:"pendingSQL,omitempty"`
	Migrate    string   `json:"migrate,omitempty"`
	Cache      string   `json:"cache,omitempty"` // after every switch
	Site       string   `json:"site,omitempty"`  // e.g. https://shop.example.org: Host and X-Forwarded-Proto of the probes
	Pages      []string `json:"pages,omitempty"`
	Links      string   `json:"links,omitempty"` // "label=url ..." in the header
	Pause      int      `json:"pause,omitempty"` // seconds per canary step
	Keep       int      `json:"keep,omitempty"`
}

func (b Box) ID() string { return b.Namespace + "/" + b.Deployment }

// State is the ConfigMap that holds the box's history in the cluster.
func (b Box) State() string { return "switchyard-" + b.Deployment }

func (b Box) PauseOr() int {
	if b.Pause > 0 {
		return b.Pause
	}
	return 120
}

func (b Box) KeepOr() int {
	if b.Keep > 0 {
		return b.Keep
	}
	return 5
}

// Backup is a namespace's backup set: its databases and volumes, as dated archives on a PVC.
type Backup struct {
	Namespace   string `json:"namespace"`
	Target      string `json:"target"` // the PVC the archives go to
	Items       []Item `json:"items"`
	Before      *Hook  `json:"before,omitempty"`
	After       *Hook  `json:"after,omitempty"`
	Cold        bool   `json:"cold,omitempty"`     // runs switchyard starts scale the app to 0 first
	Schedule    string `json:"schedule,omitempty"` // cron, "" for by hand only; always hot
	TimeZone    string `json:"timeZone,omitempty"`
	Tools       string `json:"tools,omitempty"` // image with sh, tar, zstd, sha256sum; "" is the first database's
	KeepDaily   int    `json:"keepDaily,omitempty"`
	KeepMonthly int    `json:"keepMonthly,omitempty"`
	Production  bool   `json:"production,omitempty"` // a restore needs the namespace typed in
}

// Item is one thing a run saves: a database dump or a volume's tar.
type Item struct {
	Name string `json:"name"` // in the file names: <ts>_<name>.sql.zst or .tar.zst
	Kind string `json:"kind"` // mariadb, postgres or volume

	Service     string `json:"service,omitempty"`
	StatefulSet string `json:"statefulSet,omitempty"` // "" is the service's name; not scaled down
	Database    string `json:"database,omitempty"`
	User        string `json:"user,omitempty"`
	Secret      string `json:"secret,omitempty"`
	PasswordKey string `json:"passwordKey,omitempty"` // the user's password in the Secret
	RootUser    string `json:"rootUser,omitempty"`    // restores: drops and creates the database
	RootKey     string `json:"rootKey,omitempty"`

	PVC     string   `json:"pvc,omitempty"`
	SubPath string   `json:"subPath,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

func (i Item) Ext() string {
	if i.Kind == "volume" {
		return ".tar.zst"
	}
	return ".sql.zst"
}

func (i Item) RootUserOr() string {
	switch {
	case i.RootUser != "":
		return i.RootUser
	case i.Kind == "postgres":
		return "postgres"
	}
	return "root"
}

func (i Item) StatefulSetOr() string {
	if i.StatefulSet != "" {
		return i.StatefulSet
	}
	return i.Service
}

// Hook is a command run in a container made from a workload's pod spec.
type Hook struct {
	Workload string `json:"workload"` // deployment/<name> or statefulset/<name>
	Command  string `json:"command"`  // split at spaces, no shell
}

func (b Backup) KeepDailyOr() int {
	if b.KeepDaily > 0 {
		return b.KeepDaily
	}
	return 5
}

func (b Backup) KeepMonthlyOr() int {
	if b.KeepMonthly > 0 {
		return b.KeepMonthly
	}
	return 12
}

func Dir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "switchyard")
}

func Path() string       { return filepath.Join(Dir(), "config.json") }
func Kubeconfig() string { return filepath.Join(Dir(), "kubeconfig") }

// Store is the loaded config, safe for concurrent use.
type Store struct {
	mu sync.Mutex
	c  *Config
}

// Load returns a Store; its Get is nil while there is no config.json yet.
func Load() (*Store, error) {
	s := &Store{}
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	s.c = &c
	return s, nil
}

// Get returns a copy, nil before the setup.
func (s *Store) Get() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return nil
	}
	c := *s.c
	c.Boxes = append([]Box(nil), s.c.Boxes...)
	c.Backups = append([]Backup(nil), s.c.Backups...)
	return &c
}

func (s *Store) Backup(ns string) (Backup, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		for _, b := range s.c.Backups {
			if b.Namespace == ns {
				return b, true
			}
		}
	}
	return Backup{}, false
}

func (s *Store) Box(id string) (Box, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		for _, b := range s.c.Boxes {
			if b.ID() == id {
				return b, true
			}
		}
	}
	return Box{}, false
}

// Update changes the config and writes it.
func (s *Store) Update(f func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Config{}
	if s.c != nil {
		c = *s.c
		c.Boxes = append([]Box(nil), s.c.Boxes...)
		c.Backups = append([]Backup(nil), s.c.Backups...)
	}
	if err := f(&c); err != nil {
		return err
	}
	if err := WriteFile(Path(), c); err != nil {
		return err
	}
	s.c = &c
	return nil
}

// WriteFile writes v as JSON, 0600, through a temporary file.
func WriteFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type Link struct{ Label, URL string }

// ParseLinks reads "label=url label=url ...".
func ParseLinks(s string) ([]Link, error) {
	var links []Link
	for _, f := range strings.Fields(s) {
		label, url, ok := strings.Cut(f, "=")
		if !ok || label == "" || url == "" {
			return nil, errors.New("link " + f + " is not label=url")
		}
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return nil, errors.New("link " + f + ": the url starts with https:// or http://")
		}
		links = append(links, Link{label, url})
	}
	return links, nil
}
