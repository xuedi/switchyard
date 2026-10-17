// switchyard: canary deploys and rollbacks of image tags, for apps on a small Kubernetes cluster.
// One binary, run as a user service next to kubectl: `switchyard setup` sets it up (the
// installer behind a one-time URL), then each box is one Deployment whose image switchyard moves
// from tag to tag.
// Standard library only.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	_ "time/tzdata"

	"github.com/xuedi/switchyard/internal/config"
	"github.com/xuedi/switchyard/internal/web"
)

const version = "0.8.0"

//go:embed assets/switchyard.svg
var logo []byte

func usage() {
	fmt.Fprintf(os.Stderr, `switchyard %s: canary deploys and rollbacks of image tags

  switchyard setup [-listen addr] [-kubeconfig file]
                                    the installer, on 127.0.0.1:8083 by default, with the
                                    cluster's admin kubeconfig (default ~/.kube/config); it
                                    prints the installer's one-time URL
  switchyard serve [-listen addr]   the pages
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
	default:
		usage()
		os.Exit(2)
	}
}
