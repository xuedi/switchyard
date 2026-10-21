// switchyard: canary deploys and rollbacks of image tags, for apps on a small Kubernetes cluster.
// One binary, run as a user service next to kubectl: `switchyard setup` sets it up (the
// installer behind a one-time URL), then each box is one Deployment whose image switchyard moves
// from tag to tag.
// Standard library only.
package main

import (
	"bufio"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	_ "time/tzdata"

	"github.com/xuedi/switchyard/internal/auth"
	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/web"
)

const version = "0.15.2"

//go:embed assets/switchyard.svg
var logo []byte

func usage() {
	fmt.Fprintf(os.Stderr, `switchyard %s: canary deploys and rollbacks of image tags

  switchyard setup [-listen addr] [-kubeconfig file]
                                    the installer, on 127.0.0.1:8083 by default, with the
                                    cluster's admin kubeconfig (default ~/.kube/config); it
                                    prints the installer's one-time URL
  switchyard serve [-listen addr]   the pages
  switchyard password               a new password, every session ended; then restart the
                                    service
  switchyard version

The config is %s.
`, version, config.Path())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (default: the config's, or 127.0.0.1:8083)")
	var kubeconfig *string
	if os.Args[1] == "setup" {
		kubeconfig = fs.String("kubeconfig", "", "the cluster's admin kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "setup":
		fs.Parse(os.Args[2:])
		if _, err := os.Stat(config.Path()); err == nil {
			log.Fatalf("switchyard is set up already: %s. Remove it to start over.", config.Path())
		}
		log.Fatal(web.Serve(version, logo, web.Options{Listen: *listen, Kubeconfig: *kubeconfig, Setup: true}))
	case "serve":
		fs.Parse(os.Args[2:])
		log.Fatal(web.Serve(version, logo, web.Options{Listen: *listen}))
	case "password":
		fs.Parse(os.Args[2:])
		if err := password(); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

// password sets a new password in the config. The running service keeps its own copy, so it
// has to be restarted right after.
func password() error {
	s, err := config.Load()
	if err != nil {
		return err
	}
	if s.Get() == nil {
		return fmt.Errorf("switchyard is not set up yet: no %s", config.Path())
	}
	in := bufio.NewReader(os.Stdin)
	pw := readSecret(in, "New password: ")
	if len(pw) < 8 {
		return errors.New("the password needs at least 8 characters")
	}
	if readSecret(in, "Again: ") != pw {
		return errors.New("the passwords differ")
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		return err
	}
	if err := s.Update(func(c *config.Config) error { c.Hash = hash; c.Generation++; return nil }); err != nil {
		return err
	}
	fmt.Printf("The password of %s is set, every session ended. Now: systemctl --user restart switchyard\n", s.Get().User)
	return nil
}

// readSecret reads a line, without echo on a terminal.
func readSecret(in *bufio.Reader, prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if stty("-echo") == nil {
		defer func() { stty("echo"); fmt.Fprintln(os.Stderr) }()
	}
	line, _ := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func stty(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
