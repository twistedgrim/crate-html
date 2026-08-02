// Command crated is the crate-html HTTP daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/s3store"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/telemetry"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/alecthomas/kong"
)

type cli struct {
	Config      string `help:"Path to config.yaml. Overrides the XDG default." short:"c" type:"path" placeholder:"PATH"`
	Role        string `help:"Runtime role: all serves public sites and the broker API; web is public/read-only; broker owns the API and cleanup." default:"all" enum:"all,web,broker" env:"CRATE_ROLE"`
	LogFormat   string `help:"Daemon log format." default:"text" enum:"text,json" env:"CRATE_LOG_FORMAT" name:"log-format"`
	MetricsAddr string `help:"Prometheus metrics listen address when OTEL_METRICS_EXPORTER=prometheus. Metrics never share the public listener." default:"127.0.0.1:9464" env:"CRATE_METRICS_ADDR" name:"metrics-addr"`
}

func main() {
	var root cli
	kong.Parse(&root,
		kong.Name("crated"),
		kong.Description("crate-html HTTP daemon. Serves sites under $XDG_DATA_HOME/crate/sites/ and accepts uploads via /api/sites."),
		kong.UsageOnError(),
	)
	if err := run(root); err != nil {
		loggerForFormat(root.LogFormat).Error("crated failed", "err", err)
		os.Exit(1)
	}
}

// openStore builds the configured storage backend. Both backends cap the
// logical extracted size rather than only the HTTP body, because a sparse tar
// can expand far past its on-wire size.
//
// The S3 backend contacts the bucket here so an unreachable endpoint or a
// missing bucket stops the daemon at startup instead of failing the first push.
func openStore(cfg config.Config, paths config.Paths, logger *slog.Logger) (server.Backend, error) {
	if cfg.StorageBackend == config.BackendS3 {
		metaTTL, err := cfg.S3.MetaTTLDuration()
		if err != nil {
			return nil, err
		}
		store, err := s3store.New(context.Background(), s3store.Config{
			Endpoint:     cfg.S3.Endpoint,
			Bucket:       cfg.S3.Bucket,
			Region:       cfg.S3.Region,
			AccessKey:    cfg.S3.AccessKey,
			SecretKey:    cfg.S3.SecretKey,
			Prefix:       cfg.S3.Prefix,
			UseSSL:       true,
			MaxSiteBytes: cfg.MaxUploadBytes,
			CacheBytes:   cfg.S3.CacheBytes,
			MetaTTL:      metaTTL,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("storage ready",
			"backend", config.BackendS3,
			"bucket", cfg.S3.Bucket,
			"prefix", cfg.S3.Prefix,
			"endpoint", cfg.S3.Endpoint,
		)
		return store, nil
	}

	store := storage.New(paths.SitesDir)
	store.SetMaxSiteBytes(cfg.MaxUploadBytes)
	logger.Info("storage ready", "backend", config.BackendLocal, "path", paths.SitesDir)
	return store, nil
}

// tokenPersistence returns the durable named-token document for broker roles.
// The public web role never calls this, so it does not need token-bucket or
// local config access.
func tokenPersistence(cfg config.Config, paths config.Paths, store server.Backend) (token.Persistence, error) {
	if cfg.StorageBackend == config.BackendS3 {
		s3, ok := store.(*s3store.Store)
		if !ok {
			return nil, errors.New("s3 backend has unexpected implementation")
		}
		// Tokens go in the bucket too, otherwise every restart would mint a
		// new set and invalidate every client's credentials.
		return s3.Document("tokens.yaml"), nil
	}
	return token.FileStore{Path: paths.TokensFile}, nil
}

func run(root cli) (runErr error) {
	var (
		paths config.Paths
		err   error
	)
	if root.Role == "web" {
		paths, err = config.ResolvePathsReadOnly()
	} else {
		paths, err = config.ResolvePaths()
	}
	if err != nil {
		return err
	}
	if root.Config != "" {
		paths.ConfigFile = root.Config
		// tokens.yaml lives beside an explicitly chosen config file so a
		// --config deployment is fully self-contained.
		paths.TokensFile = filepath.Join(filepath.Dir(root.Config), "tokens.yaml")
	}
	var cfg config.Config
	if root.Role == "web" {
		cfg, err = config.LoadReadOnly(paths)
	} else {
		cfg, err = config.LoadOrInit(paths)
	}
	if err != nil {
		return err
	}

	if err := cfg.ValidateStorage(); err != nil {
		return err
	}

	logger := loggerForFormat(root.LogFormat)
	logger.Info("daemon starting",
		"version", server.Version,
		"role", root.Role,
		"config", paths.ConfigFile,
		"listen", cfg.ListenAddr,
	)
	if root.Role != "web" {
		logger.Info("broker endpoint", "url", cfg.EffectiveAPIURL())
	}
	if root.Role != "broker" {
		logger.Info("public endpoint", "url", cfg.EffectivePublicURL())
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var metricsProvider *telemetry.Provider
	if root.Role != "web" {
		metricsProvider, err = telemetry.Start(ctx)
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			runErr = errors.Join(runErr, metricsProvider.Shutdown(shutdownCtx))
		}()
		logger.Info("broker metrics configured", "exporter", metricsProvider.Mode())
	}

	store, err := openStore(cfg, paths, logger)
	if err != nil {
		return err
	}

	var tokens *token.Store
	if root.Role != "web" {
		tokenStore, terr := tokenPersistence(cfg, paths, store)
		if terr != nil {
			return terr
		}
		tokens, err = token.LoadFrom(tokenStore)
		if err != nil {
			return err
		}
	}
	var builtins []builtin.Site
	if root.Role != "broker" {
		builtins = builtin.Sites()
	}
	var srv *server.Server
	if root.Role == "web" {
		srv = server.NewReadOnly(cfg, store, builtins, logger)
	} else {
		srv = server.NewWithMetrics(cfg, store, tokens, builtins, logger, metricsProvider.Metrics())
		srv.RefreshMetricSiteCount()
	}
	if root.Role != "broker" && cfg.IndexTemplate != "" {
		if err := srv.UseIndexTemplateFile(cfg.IndexTemplate); err != nil {
			return err
		}
		logger.Info("custom index ready", "path", cfg.IndexTemplate)
	}

	var handler http.Handler
	switch root.Role {
	case "web":
		handler = srv.PublicHandler()
	case "broker":
		handler = srv.BrokerHandler()
	default:
		handler = srv.Handler()
	}
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	var metricsSrv *http.Server
	var metricsListener net.Listener
	if root.Role != "web" && metricsProvider.Handler() != nil {
		listener, lerr := net.Listen("tcp", root.MetricsAddr)
		if lerr != nil {
			return fmt.Errorf("listen for Prometheus metrics on %s: %w", root.MetricsAddr, lerr)
		}
		metricsListener = listener
		metricsSrv = &http.Server{
			Handler:           metricsProvider.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		logger.Info("Prometheus metrics listener", "listen", root.MetricsAddr)
	}
	if root.Role != "web" {
		go watchExpiries(ctx, srv, logger, time.Minute)
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()
	if metricsSrv != nil {
		go func() {
			errCh <- metricsSrv.Serve(metricsListener)
		}()
	}

	shutdown := func() error {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		var shutdownErr error
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				shutdownErr = errors.Join(shutdownErr, err)
			}
		}
		return shutdownErr
	}

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		return shutdown()
	case err := <-errCh:
		shutdownErr := shutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return shutdownErr
		}
		return errors.Join(err, shutdownErr)
	}
}

func watchExpiries(ctx context.Context, srv *server.Server, logger *slog.Logger, interval time.Duration) {
	remove := func() {
		deleted, err := srv.DeleteExpired(time.Now())
		if err != nil {
			logger.Error("expiry cleanup failed", "err", err)
			return
		}
		for _, name := range deleted {
			logger.Info("site expired", "site", name)
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

func loggerForFormat(format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
