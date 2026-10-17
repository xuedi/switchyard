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

	Kubectl         string `json:"kubectl"`
	AdminKubeconfig string `json:"adminKubeconfig,omitempty"` // "" is kubectl's default
	AdminContext    string `json:"adminContext"`              // used only to add a box in a new namespace
	Cluster         string `json:"cluster"`                   // its name on the page

	Boxes []Box `json:"boxes"`
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
	return &c
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
