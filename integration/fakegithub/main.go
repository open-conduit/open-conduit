// Command fakegithub is the Docker tests' stand-in for GitHub, run in a
// container on a gateway's egress network as github.com: the API of
// githubtest (with the test's GitHub App) for api.github.com, and git's
// smart HTTP for github.com, both over TLS with the test's certificate.
// Its bare repositories <repo>.git and every -extra one start empty.
// It is test support, never part of a release.
package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

func main() {
	repo := flag.String("repo", "", "the App's repository, owner/name")
	extra := flag.String("extra", "", "more repositories, comma-separated, the App isn't installed on")
	app := flag.String("app", "", "the GitHub App's ID")
	key := flag.String("app-key", "", "the App's public key (PEM)")
	cert := flag.String("cert", "", "the TLS certificate (PEM)")
	certKey := flag.String("cert-key", "", "the TLS key (PEM)")
	root := flag.String("root", "/tmp/git", "where the bare repositories go")
	listen := flag.String("listen", ":443", "the address to serve on")
	flag.Parse()
	raw, err := os.ReadFile(*key)
	if err != nil {
		log.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		log.Fatal("no PEM in the App key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		log.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		log.Fatal(err)
	}
	repos := []string{*repo}
	if *extra != "" {
		repos = append(repos, strings.Split(*extra, ",")...)
	}
	for _, r := range repos {
		dir := filepath.Join(*root, filepath.FromSlash(r)+".git")
		if out, err := exec.Command(git, "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
			log.Fatalf("git init %s: %v %s", dir, err, out)
		}
	}
	fake := githubtest.New(*repo)
	fake.AddApp(*app, pub.(*rsa.PublicKey))
	api, gitHTTP := fake.Handler(), fake.GitHandler(*root, git)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "api.") {
			api.ServeHTTP(w, r)
			return
		}
		gitHTTP.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServeTLS(*listen, *cert, *certKey, handler))
}
