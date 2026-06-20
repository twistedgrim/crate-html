// Command crate is the crate-html CLI. It talks to a running crated over HTTP.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/Twistedgrim/crate-html/internal/cliclient"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/alecthomas/kong"
)

type pushCmd struct {
	Dir  string `arg:"" help:"Local directory to upload." type:"existingdir"`
	Name string `arg:"" help:"Site name (lowercase, dot/hyphen/underscore allowed)."`
}

func (c *pushCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	res, err := client.Push(context.Background(), c.Name, c.Dir)
	if err != nil {
		return err
	}
	fmt.Printf("pushed %s (%d files, %d bytes)\n", res.Site.Name, res.Site.FileCount, res.Site.SizeBytes)
	fmt.Println(res.URL)
	return nil
}

type lsCmd struct{}

func (c *lsCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	sites, err := client.List(context.Background())
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		fmt.Println("(no sites)")
		return nil
	}
	for _, s := range sites {
		fmt.Printf("%-32s  %6d files  %10d bytes  %s\n",
			s.Name, s.FileCount, s.SizeBytes, s.UpdatedAt.Format("2006-01-02 15:04"))
	}
	return nil
}

type rmCmd struct {
	Name string `arg:"" help:"Site name to remove."`
}

func (c *rmCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	if err := client.Delete(context.Background(), c.Name); err != nil {
		return err
	}
	fmt.Println("removed", c.Name)
	return nil
}

type openCmd struct {
	Name string `arg:"" help:"Site name to open in the browser."`
}

func (c *openCmd) Run(g *globals) error {
	url := g.cfg.BaseURL + "/" + c.Name + "/"
	fmt.Println(url)
	return openBrowser(url)
}

type statusCmd struct{}

func (c *statusCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	st, err := client.Status(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("crated %s  base=%s  sites=%d\n", st.Version, g.cfg.BaseURL, st.SiteCount)
	return nil
}

type cli struct {
	Push   pushCmd   `cmd:"" help:"Upload a directory as a site."`
	Ls     lsCmd     `cmd:"" help:"List deployed sites."`
	Rm     rmCmd     `cmd:"" help:"Remove a site."`
	Open   openCmd   `cmd:"" help:"Open a site in your browser."`
	Status statusCmd `cmd:"" help:"Show daemon status."`
}

type globals struct {
	cfg config.Config
}

func main() {
	var root cli
	kctx := kong.Parse(&root,
		kong.Name("crate"),
		kong.Description("Publish HTML to a local crate-html daemon."),
		kong.UsageOnError(),
	)

	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crate:", err)
		os.Exit(1)
	}
	cfg, err := config.LoadOrInit(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crate:", err)
		os.Exit(1)
	}
	g := &globals{cfg: cfg}

	if err := kctx.Run(g); err != nil {
		fmt.Fprintln(os.Stderr, "crate:", err)
		os.Exit(1)
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return nil
	}
	return cmd.Start()
}
