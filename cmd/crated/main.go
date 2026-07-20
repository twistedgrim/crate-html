// Command crated is the crate-html HTTP daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/alecthomas/kong"
)

type cli struct {
	Config string `help:"Path to config.yaml. Overrides the XDG default." short:"c" type:"path" placeholder:"PATH"`
}

func main() {
	var root cli
	kong.Parse(&root,
		kong.Name("crated"),
		kong.Description("crate-html HTTP daemon. Serves sites under $XDG_DATA_HOME/crate/sites/ and accepts uploads via /api/sites."),
		kong.UsageOnError(),
	)
	if err := run(root); err != nil {
		fmt.Fprintln(os.Stderr, "crated:", err)
		os.Exit(1)
	}
}

func run(root cli) error {
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	if root.Config != "" {
		paths.ConfigFile = root.Config
		// tokens.yaml lives beside an explicitly chosen config file so a
		// --config deployment is fully self-contained.
		paths.TokensFile = filepath.Join(filepath.Dir(root.Config), "tokens.yaml")
	}
	cfg, err := config.LoadOrInit(paths)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "crated ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("config: %s", paths.ConfigFile)
	logger.Printf("sites:  %s", paths.SitesDir)
	logger.Printf("listen: %s", cfg.BaseURL)

	tokens, err := token.Load(paths.TokensFile)
	if err != nil {
		return err
	}

	store := storage.New(paths.SitesDir)
	// Cap logical extracted size, not just the HTTP body: a sparse tar can
	// expand far past its on-wire size.
	store.SetMaxSiteBytes(cfg.MaxUploadBytes)
	srv := server.New(cfg, store, tokens, builtin.Sites(), logger)
	if cfg.IndexTemplate != "" {
		if err := srv.UseIndexTemplateFile(cfg.IndexTemplate); err != nil {
			return err
		}
		logger.Printf("index:  %s (custom)", cfg.IndexTemplate)
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go watchExpiries(ctx, srv, logger, time.Minute)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Println("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func watchExpiries(ctx context.Context, srv *server.Server, logger *log.Logger, interval time.Duration) {
	remove := func() {
		deleted, err := srv.DeleteExpired(time.Now())
		if err != nil {
			logger.Printf("expiry cleanup: %v", err)
			return
		}
		for _, name := range deleted {
			logger.Printf("expired site %s", name)
		}
	}
	remove()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			remove()
		}
	}
}
