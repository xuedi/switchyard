// Package registry reads an image repository through the OCI distribution API: its tags, and per
// tag the digest, the creation time and the labels. Bearer tokens (ghcr.io, Docker Hub) and
// basic auth; the credentials are the namespace's pull secret.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Ref is an image reference, e.g. ghcr.io/acme/shop:1.2.
type Ref struct{ Registry, Repo, Tag string }

// Parse splits an image reference; docker.io and library/ are filled in as docker does.
func Parse(image string) Ref {
	name, _, _ := strings.Cut(image, "@")
	var r Ref
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, r.Tag = name[:i], name[i+1:]
	}
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repo = first, rest
	} else {
		r.Registry, r.Repo = "docker.io", name
	}
	if r.Registry == "docker.io" && !strings.Contains(r.Repo, "/") {
		r.Repo = "library/" + r.Repo
	}
	return r
}

// WithTag is the image as written in the pod spec, with another tag.
func WithTag(image, tag string) string {
	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	return name + ":" + tag
}

// Digest is the sha256:... an image reference or a pod's imageID pins, "" when it pins none.
func Digest(image string) string {
	i := strings.LastIndexByte(image, '@')
	if i < 0 || !strings.HasPrefix(image[i+1:], "sha256:") || len(image)-i-1 != 71 {
		return ""
	}
	return image[i+1:]
}

func (r Ref) host() string {
	if r.Registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return r.Registry
}

// Creds finds the registry's user and password in a .dockerconfigjson.
func Creds(dockerconfig []byte, registry string) (user, pass string, ok bool) {
	var cfg struct {
		Auths map[string]struct {
			Username, Password, Auth string
		}
	}
	if json.Unmarshal(dockerconfig, &cfg) != nil {
		return "", "", false
	}
	for key, a := range cfg.Auths {
		host := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
		host, _, _ = strings.Cut(host, "/")
		if host != registry && !(registry == "docker.io" && (host == "index.docker.io" || host == "registry-1.docker.io")) {
			continue
		}
		if a.Username != "" {
			return a.Username, a.Password, true
		}
		if b, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
			if u, p, ok := strings.Cut(string(b), ":"); ok {
				return u, p, true
			}
		}
	}
	return "", "", false
}

type Image struct {
	Tag     string
	Digest  string
	Created time.Time
	Labels  map[string]string
}

// Client reads one repository. Tags are immutable by contract, so what was read of a tag is
// kept; moving tags (latest) are left out.
type Client struct {
	ref        Ref
	user, pass string
	http       *http.Client

	mu     sync.Mutex
	token  string
	images map[string]Image
}

func New(ref Ref, user, pass string) *Client {
	return &Client{ref: ref, user: user, pass: pass, http: &http.Client{Timeout: 20 * time.Second}, images: map[string]Image{}}
}

func (c *Client) Ref() Ref { return c.ref }

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// login answers a 401's challenge: a bearer token from the realm, or basic auth from now on.
func (c *Client) login(ctx context.Context, challenge string) error {
	scheme, params, _ := strings.Cut(challenge, " ")
	if strings.EqualFold(scheme, "Basic") {
		if c.user == "" {
			return errors.New("the registry wants a login and the namespace has no pull secret for it")
		}
		c.token = ""
		return nil
	}
	p := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(params, -1) {
		p[m[1]] = m[2]
	}
	if p["realm"] == "" {
		return fmt.Errorf("registry: unknown challenge %q", challenge)
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + c.ref.Repo + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, "GET", p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("registry login at %s: %s", p["realm"], resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	c.token = t.Token
	if c.token == "" {
		c.token = t.AccessToken
	}
	return nil
}

// get fetches a path below https://<registry>/v2/<repo>/, logging in once on a 401.
func (c *Client) get(ctx context.Context, path string, accept ...string) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://"+c.ref.host()+"/v2/"+c.ref.Repo+"/"+path, nil)
		if err != nil {
			return nil, err
		}
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		c.mu.Lock()
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		} else if c.user != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		c.mu.Unlock()
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			resp.Body.Close()
			c.mu.Lock()
			err := c.login(ctx, resp.Header.Get("WWW-Authenticate"))
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, fmt.Errorf("registry %s/%s: %s: %s", c.ref.Registry, c.ref.Repo, path, resp.Status)
		}
		return resp, nil
	}
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Tags lists every tag of the repository.
func (c *Client) Tags(ctx context.Context) ([]string, error) {
	var all []string
	path := "tags/list?n=1000"
	for path != "" {
		resp, err := c.get(ctx, path)
		if err != nil {
			return nil, err
		}
		var list struct{ Tags []string }
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, list.Tags...)
		path = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			if _, after, ok := strings.Cut(m[1], "/"+c.ref.Repo+"/"); ok {
				path = after
			}
		}
	}
	return all, nil
}

var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

type manifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

func (c *Client) manifest(ctx context.Context, ref string) (manifest, string, error) {
	resp, err := c.get(ctx, "manifests/"+ref, manifestTypes...)
	if err != nil {
		return manifest{}, "", err
	}
	defer resp.Body.Close()
	var m manifest
	err = json.NewDecoder(resp.Body).Decode(&m)
	return m, resp.Header.Get("Docker-Content-Digest"), err
}

// Image reads a tag's digest, creation time and labels. For a multi-platform image the
// linux/amd64 one is read, or the first real platform.
func (c *Client) Image(ctx context.Context, tag string) (Image, error) {
	c.mu.Lock()
	img, ok := c.images[tag]
	c.mu.Unlock()
	if ok {
		return img, nil
	}
	m, digest, err := c.manifest(ctx, tag)
	if err != nil {
		return Image{}, err
	}
	if len(m.Manifests) > 0 {
		pick := ""
		for _, e := range m.Manifests {
			if e.Platform.OS == "linux" && e.Platform.Architecture == "amd64" {
				pick = e.Digest
				break
			}
			if pick == "" && e.Platform.OS != "unknown" {
				pick = e.Digest
			}
		}
		if pick == "" {
			return Image{}, fmt.Errorf("%s: no platform in the index", tag)
		}
		if m, _, err = c.manifest(ctx, pick); err != nil {
			return Image{}, err
		}
	}
	if m.Config.Digest == "" {
		return Image{}, fmt.Errorf("%s: the manifest names no config", tag)
	}
	resp, err := c.get(ctx, "blobs/"+m.Config.Digest)
	if err != nil {
		return Image{}, err
	}
	defer resp.Body.Close()
	var cfg struct {
		Created time.Time `json:"created"`
		Config  struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&cfg); err != nil {
		return Image{}, err
	}
	img = Image{Tag: tag, Digest: digest, Created: cfg.Created, Labels: cfg.Config.Labels}
	if t, err := time.Parse(time.RFC3339, img.Labels["org.opencontainers.image.created"]); err == nil {
		img.Created = t
	}
	c.mu.Lock()
	c.images[tag] = img
	c.mu.Unlock()
	return img, nil
}

// Moving says a tag is not a release: latest and the like.
func Moving(tag string) bool {
	return slices.Contains([]string{"latest", "main", "master", "edge", "nightly", "dev", "stable"}, tag)
}

// Images returns the newest max tags by creation time. The registry lists tags in no useful
// order, so each is read once; tags already read cost nothing.
func (c *Client) Images(ctx context.Context, max int) ([]Image, error) {
	tags, err := c.Tags(ctx)
	if err != nil {
		return nil, err
	}
	var (
		mu   sync.Mutex
		list []Image
		errs []string
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 4)
	)
	for _, t := range tags {
		if Moving(t) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			img, err := c.Image(ctx, t)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err.Error())
				return
			}
			list = append(list, img)
		}()
	}
	wg.Wait()
	slices.SortFunc(list, func(a, b Image) int { return b.Created.Compare(a.Created) })
	if len(list) > max {
		list = list[:max]
	}
	if len(list) == 0 && len(errs) > 0 {
		return nil, errors.New(errs[0])
	}
	return list, nil
}
