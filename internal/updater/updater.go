// Package updater provides checksum-verified, rollback-safe updates for the
// crate client binary from GitHub Releases.
package updater

import (
	"context"
	"fmt"
	"regexp"
	"runtime"

	"github.com/Masterminds/semver/v3"
	selfupdate "github.com/creativeprojects/go-selfupdate"
)

// Repository is the GitHub repository used for release discovery. Release
// builds stamp it from github.repository so forks and rehearsal repositories
// update from the same place that produced their binary.
var Repository = "Twistedgrim/crate-html"

// Result describes the outcome of an update attempt.
type Result struct {
	From         string
	To           string
	Updated      bool
	ReleaseURL   string
	ReleaseNotes string
}

// Options supplies dependencies and platform overrides. The zero value uses
// GitHub Releases, the running platform, and the current executable.
type Options struct {
	Source         selfupdate.Source
	OS             string
	Arch           string
	ExecutablePath func() (string, error)
}

// Client detects and applies crate client releases.
type Client struct {
	updater        *selfupdate.Updater
	repository     selfupdate.Repository
	executablePath func() (string, error)
	goos           string
	goarch         string
}

// New constructs a client that selects only the crate archive for its target
// platform and validates it against the release's checksums.txt asset.
func New(options Options) (*Client, error) {
	goos := options.OS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := options.Arch
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	executablePath := options.ExecutablePath
	if executablePath == nil {
		executablePath = selfupdate.ExecutablePath
	}

	impl, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    options.Source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
		Filters:   []string{assetPattern(goos, goarch)},
		OS:        goos,
		Arch:      goarch,
	})
	if err != nil {
		return nil, fmt.Errorf("configure updater: %w", err)
	}

	return &Client{
		updater:        impl,
		repository:     selfupdate.ParseSlug(Repository),
		executablePath: executablePath,
		goos:           goos,
		goarch:         goarch,
	}, nil
}

// Update replaces the current executable when a newer stable release exists.
// Development and prerelease builds are deliberately not self-updatable.
func (c *Client) Update(ctx context.Context, current string) (Result, error) {
	currentVersion, err := semver.NewVersion(current)
	if err != nil {
		return Result{}, fmt.Errorf("invalid current version %q: %w", current, err)
	}
	if currentVersion.Prerelease() != "" {
		return Result{}, fmt.Errorf("self-update is unavailable for development or prerelease build %q", current)
	}

	latest, found, err := c.updater.DetectLatest(ctx, c.repository)
	if err != nil {
		return Result{}, fmt.Errorf("check GitHub releases: %w", err)
	}
	if !found {
		return Result{}, fmt.Errorf("no compatible crate release found for %s/%s", c.goos, c.goarch)
	}

	result := Result{
		From:         currentVersion.String(),
		To:           latest.Version(),
		ReleaseURL:   latest.URL,
		ReleaseNotes: latest.ReleaseNotes,
	}
	if !latest.GreaterThan(currentVersion.String()) {
		return result, nil
	}

	executable, err := c.executablePath()
	if err != nil {
		return Result{}, fmt.Errorf("locate crate executable: %w", err)
	}
	if err := c.updater.UpdateTo(ctx, latest, executable); err != nil {
		return Result{}, fmt.Errorf("replace %s: %w", executable, err)
	}
	result.Updated = true
	return result, nil
}

func assetPattern(goos, goarch string) string {
	return fmt.Sprintf(
		`^crate_\d+\.\d+\.\d+_%s_%s\.tar\.gz$`,
		regexp.QuoteMeta(goos),
		regexp.QuoteMeta(goarch),
	)
}
