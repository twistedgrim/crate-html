// Package builtin owns the sites that ship inside the crated binary.
//
// Today the only built-in is `cratesplainer` — a deliberately-overexplained
// guide to using crate that's there from the moment the daemon comes up.
// More can be added later by dropping the directory under internal/builtin/
// and registering it in Sites().
package builtin

import (
	"embed"
	"io/fs"
)

//go:embed all:cratesplainer
var cratesplainerFS embed.FS

// Site is a single embedded site.
type Site struct {
	// Name is the URL segment the site is served under (/<Name>/).
	Name string
	// FS is the site's content, rooted at the site directory.
	FS fs.FS
}

// Sites returns every embedded site keyed by name. Order is stable for index rendering.
func Sites() []Site {
	cp, _ := fs.Sub(cratesplainerFS, "cratesplainer")
	return []Site{
		{Name: "cratesplainer", FS: cp},
	}
}
