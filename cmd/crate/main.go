// Command crate is the crate-html CLI. It talks to a running crated over HTTP.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Twistedgrim/crate-html/internal/cliclient"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/wire"
	"github.com/alecthomas/kong"
)

type pushCmd struct {
	Src     string `arg:"" name:"src" help:"Directory to upload, path to a pre-built .tar, or '-' to read a tar from stdin."`
	Name    string `arg:"" help:"Site name (lowercase, dot/hyphen/underscore allowed)."`
	Open    bool   `help:"Open the published URL in a browser after a successful push." short:"o"`
	Expires string `help:"Expiry duration (default 24h), or 'never' to retain indefinitely." default:"24h" placeholder:"DURATION|never"`
}

func (c *pushCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	ctx := context.Background()

	res, err := c.push(ctx, client)
	if err != nil {
		return err
	}
	fmt.Printf("pushed %s (%d files, %d bytes)\n", res.Site.Name, res.Site.FileCount, res.Site.SizeBytes)
	// Build the display URL from the base URL this client actually dialed,
	// not the server-reported one: behind Docker port-mapping or a reverse
	// proxy the daemon only knows its internal address, so res.URL would be
	// a dead link from the caller's side of the boundary.
	url := strings.TrimRight(g.cfg.BaseURL, "/") + "/" + res.Site.Name + "/"
	fmt.Println(url)
	if res.Site.ExpiresAt == nil {
		fmt.Println("expires never")
	} else {
		fmt.Println("expires", res.Site.ExpiresAt.Local().Format(time.RFC3339))
	}
	if c.Open {
		if err := openBrowser(url); err != nil {
			return fmt.Errorf("open browser: %w", err)
		}
	}
	return nil
}

func (c *pushCmd) push(ctx context.Context, client *cliclient.Client) (wire.PutSiteResponse, error) {
	// "-" → stdin (the canonical agent-on-Docker-host path:
	// `tar -C ./dir -cf - . | docker exec -i crated crate push - <name>`).
	if c.Src == "-" {
		return client.PushReaderWithExpiry(ctx, c.Name, os.Stdin, c.Expires)
	}

	info, err := os.Stat(c.Src)
	if err != nil {
		return wire.PutSiteResponse{}, err
	}
	if info.IsDir() {
		return client.PushWithExpiry(ctx, c.Name, c.Src, c.Expires)
	}
	if info.Mode().IsRegular() {
		// Treat any regular file as a pre-built tar archive.
		f, ferr := os.Open(c.Src)
		if ferr != nil {
			return wire.PutSiteResponse{}, ferr
		}
		defer f.Close()
		return client.PushReaderWithExpiry(ctx, c.Name, f, c.Expires)
	}
	return wire.PutSiteResponse{}, fmt.Errorf("source must be a directory, a regular file, or '-' (stdin); got %s", c.Src)
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
		expires := "never"
		if s.ExpiresAt != nil {
			expires = s.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-32s  %6d files  %10d bytes  updated %s  expires %s\n",
			s.Name, s.FileCount, s.SizeBytes, s.UpdatedAt.Format("2006-01-02 15:04"), expires)
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

type tokenCmd struct {
	Show   tokenShowCmd   `cmd:"" default:"1" hidden:"" help:"Print the root bearer token from the loaded config."`
	Create tokenCreateCmd `cmd:"" help:"Mint a named API token (requires the root token)."`
	Ls     tokenLsCmd     `cmd:"" help:"List minted API tokens."`
	Revoke tokenRevokeCmd `cmd:"" help:"Revoke a minted API token by id or name."`
}

type tokenShowCmd struct{}

func (c *tokenShowCmd) Run(g *globals) error {
	if g.cfg.Token == "" {
		return fmt.Errorf("no token set in config")
	}
	_, err := io.WriteString(os.Stdout, g.cfg.Token+"\n")
	return err
}

type tokenCreateCmd struct {
	Name    string `arg:"" help:"Token name (lowercase, dot/hyphen/underscore allowed), e.g. the client it belongs to."`
	Expires string `help:"Expiry duration (for example 720h), or 'never' (default)." default:"never" placeholder:"DURATION|never"`
}

func (c *tokenCreateCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	res, err := client.CreateToken(context.Background(), c.Name, c.Expires)
	if err != nil {
		return err
	}
	// Secret to stdout (pipe-friendly), guidance to stderr.
	fmt.Println(res.Token)
	fmt.Fprintf(os.Stderr, "token %q created (id %s). This is the only time the secret is shown — store it now.\n",
		res.Info.Name, res.Info.ID)
	if res.Info.ExpiresAt != nil {
		fmt.Fprintln(os.Stderr, "expires", res.Info.ExpiresAt.Local().Format(time.RFC3339))
	}
	return nil
}

type tokenLsCmd struct{}

func (c *tokenLsCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	tokens, err := client.ListTokens(context.Background())
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Println("(no tokens — mint one with `crate token create <name>`)")
		return nil
	}
	for _, tk := range tokens {
		expires, lastUsed := "never", "never"
		if tk.ExpiresAt != nil {
			expires = tk.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		if tk.LastUsedAt != nil {
			lastUsed = tk.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%s  %-24s  created %s  expires %s  last used %s\n",
			tk.ID, tk.Name, tk.CreatedAt.Local().Format("2006-01-02 15:04"), expires, lastUsed)
	}
	return nil
}

type tokenRevokeCmd struct {
	Token string `arg:"" name:"id-or-name" help:"Token id or name to revoke."`
}

func (c *tokenRevokeCmd) Run(g *globals) error {
	client := cliclient.New(g.cfg)
	if err := client.RevokeToken(context.Background(), c.Token); err != nil {
		return err
	}
	fmt.Println("revoked", c.Token)
	return nil
}

type cli struct {
	Config string `help:"Path to config.yaml. Overrides the XDG default." short:"c" type:"path" placeholder:"PATH"`

	Push   pushCmd   `cmd:"" help:"Upload a directory, tar file, or stdin tar as a site."`
	Ls     lsCmd     `cmd:"" help:"List deployed sites."`
	Rm     rmCmd     `cmd:"" help:"Remove a site."`
	Open   openCmd   `cmd:"" help:"Open a site in your browser."`
	Status statusCmd `cmd:"" help:"Show daemon status."`
	Token  tokenCmd  `cmd:"" help:"Print the root token, or manage named API tokens (create/ls/revoke)."`
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
	if root.Config != "" {
		paths.ConfigFile = root.Config
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
	// Honor BROWSER if set (POSIX convention; xdg-open already does this on
	// Linux, we extend it cross-platform). Setting BROWSER=/usr/bin/true (or
	// any no-op command) is the supported way for tests + headless scripts
	// to suppress the actual browser pop.
	if b := os.Getenv("BROWSER"); b != "" {
		return exec.Command(b, url).Start()
	}
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
