// switchyard: canary deploys and rollbacks of image tags, for apps on a small Kubernetes cluster.
// One binary, run as a user service next to kubectl: `switchyard setup` (or the first page
// view) sets it up, then each box is one Deployment whose image switchyard moves from tag to tag.
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

const version = "0.6.0"

//go:embed assets/switchyard.svg
var logo []byte

func usage() {
	fmt.Fprintf(os.Stderr, `switchyard %s: canary deploys and rollbacks of image tags

  switchyard setup [-listen addr]   the installer, on http://127.0.0.1:8080 by default
  switchyard serve [-listen addr]   the pages (the installer too, while there is no config)
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
	listen := fs.String("listen", "", "address to listen on, e.g. 127.0.0.1:8083 (default: the config's, or 127.0.0.1:8080)")
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "setup":
		fs.Parse(os.Args[2:])
		if _, err := os.Stat(config.Path()); err == nil {
			log.Fatalf("switchyard is set up already: %s. Remove it to start over.", config.Path())
		}
		log.Fatal(web.Serve(version, logo, *listen))
	case "serve":
		fs.Parse(os.Args[2:])
		log.Fatal(web.Serve(version, logo, *listen))
	default:
		usage()
		os.Exit(2)
	}
}
