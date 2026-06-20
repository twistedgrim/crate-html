// Package cliclient is the HTTP client used by cmd/crate to talk to a running crated.
package cliclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

// Client talks to crated.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New returns a Client configured from cfg.
func New(cfg config.Config) *Client {
	return &Client{
		base:  strings.TrimRight(cfg.BaseURL, "/"),
		token: cfg.Token,
		hc:    &http.Client{},
	}
}

func (c *Client) newReq(ctx context.Context, method, p string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+p, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(wire.HeaderAuth, "Bearer "+c.token)
	return req, nil
}

func decodeErr(resp *http.Response) error {
	var e wire.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error == "" {
		return fmt.Errorf("crated: %s", resp.Status)
	}
	return fmt.Errorf("crated: %s: %s", resp.Status, e.Error)
}

// Status calls GET /api/status.
func (c *Client) Status(ctx context.Context) (wire.StatusResponse, error) {
	req, err := c.newReq(ctx, http.MethodGet, wire.PathAPIStatus, nil)
	if err != nil {
		return wire.StatusResponse{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.StatusResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wire.StatusResponse{}, decodeErr(resp)
	}
	var out wire.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return wire.StatusResponse{}, err
	}
	return out, nil
}

// List calls GET /api/sites.
func (c *Client) List(ctx context.Context) ([]wire.Site, error) {
	req, err := c.newReq(ctx, http.MethodGet, wire.PathAPISites, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out wire.ListSitesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Sites, nil
}

// Delete calls DELETE /api/sites/{name}.
func (c *Client) Delete(ctx context.Context, name string) error {
	req, err := c.newReq(ctx, http.MethodDelete, wire.PathAPISites+"/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// Push tars srcDir and PUTs it as site `name`.
func (c *Client) Push(ctx context.Context, name, srcDir string) (wire.PutSiteResponse, error) {
	pr, pw := io.Pipe()
	go func() {
		err := storage.WriteDirAsTar(srcDir, pw)
		_ = pw.CloseWithError(err)
	}()

	req, err := c.newReq(ctx, http.MethodPut, wire.PathAPISites+"/"+name, pr)
	if err != nil {
		return wire.PutSiteResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.PutSiteResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wire.PutSiteResponse{}, decodeErr(resp)
	}
	var out wire.PutSiteResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return wire.PutSiteResponse{}, err
	}
	return out, nil
}
