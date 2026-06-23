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
	"syscall"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
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
	}
	cfg, err := config.LoadOrInit(paths)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "crated ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("config: %s", paths.ConfigFile)
	logger.Printf("sites:  %s", paths.SitesDir)
	logger.Printf("listen: %s", cfg.BaseURL)

	store := storage.New(paths.SitesDir)
	srv := server.New(cfg, store, builtin.Sites(), logger)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
