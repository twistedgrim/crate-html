package builtin_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/Twistedgrim/crate-html/internal/builtin"
)

// TestSitesContainsCratesplainer pins the only currently-shipped builtin.
// If this fails, we either renamed it (update the test) or accidentally
// stopped shipping it (regression).
func TestSitesContainsCratesplainer(t *testing.T) {
	sites := builtin.Sites()
	if len(sites) == 0 {
		t.Fatal("no builtin sites")
	}
	found := false
	for _, s := range sites {
		if s.Name == "cratesplainer" {
			found = true
		}
	}
	if !found {
		t.Errorf("cratesplainer not in builtins: %+v", sites)
	}
}

// TestSitesAllHaveIndex confirms every builtin has an index.html at the FS
// root, otherwise serving `/<name>/` would 404 even though the builtin
// "exists".
func TestSitesAllHaveIndex(t *testing.T) {
	for _, site := range builtin.Sites() {
		site := site
		t.Run(site.Name, func(t *testing.T) {
			b, err := fs.ReadFile(site.FS, "index.html")
			if err != nil {
				t.Errorf("%s/index.html: %v", site.Name, err)
				return
			}
			if len(b) == 0 {
				t.Errorf("%s/index.html is empty", site.Name)
			}
		})
	}
}

// TestCratesplainerStaticAssets pins the asset list at a minimum that the
// integration tests expect to be reachable. If we remove or rename one
// without updating the integration suite, we want to see it here first.
func TestCratesplainerStaticAssets(t *testing.T) {
	var cratesplainer fs.FS
	for _, s := range builtin.Sites() {
		if s.Name == "cratesplainer" {
			cratesplainer = s.FS
			break
		}
	}
	if cratesplainer == nil {
		t.Fatal("cratesplainer not found")
	}

	for _, path := range []string{"index.html", "commands.html", "gotchas.html", "style.css"} {
		path := path
		t.Run(path, func(t *testing.T) {
			info, err := fs.Stat(cratesplainer, path)
			if err != nil {
				t.Errorf("Stat %s: %v", path, err)
				return
			}
			if info.IsDir() {
				t.Errorf("%s is a dir, want regular file", path)
			}
		})
	}
}

// TestCratesplainerNoBrokenAside guards against the malformed HTML we fixed
// earlier (`<div class="aside>` instead of `<div class="aside">`). If a
// future edit reintroduces a missing-quote class attribute, this fails.
func TestCratesplainerNoBrokenAside(t *testing.T) {
	var fsys fs.FS
	for _, s := range builtin.Sites() {
		if s.Name == "cratesplainer" {
			fsys = s.FS
		}
	}
	if fsys == nil {
		t.Skip("no cratesplainer builtin")
	}
	for _, file := range []string{"index.html", "commands.html", "gotchas.html"} {
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			continue
		}
		if strings.Contains(string(body), `class="aside>`) {
			t.Errorf(`%s contains malformed class="aside> (missing quote)`, file)
		}
	}
}
