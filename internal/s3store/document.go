package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/minio/minio-go/v7"
)

// ErrConflict reports that a document changed in the bucket since this process
// last read it, so the write was refused rather than silently discarding the
// other writer's update.
var ErrConflict = errors.New("s3: document changed concurrently")

// Document is a single small object holding daemon state that is not a site —
// today, the token set. Sites are content-addressed and replaced wholesale; a
// document is read-modify-write, which needs different handling.
//
// Writes are compare-and-swap: each Save is conditioned on the object version
// this process last observed, using S3 conditional writes. A concurrent writer
// therefore loses with ErrConflict instead of clobbering, which matters because
// the token document is a whole set — a blind overwrite would delete tokens
// another replica had just minted.
//
// Not every S3-compatible server enforces conditional headers. Where they are
// ignored the behavior degrades to last-write-wins, which is what crate did
// before this existed.
type Document struct {
	client *minio.Client
	bucket string
	key    string

	mu   sync.Mutex
	etag string // version last observed; empty means "believed absent"
	seen bool   // whether etag reflects a real observation
}

// Document returns a handle to a named state object in the bucket.
func (s *Store) Document(name string) *Document {
	return &Document{
		client: s.client,
		bucket: s.bucket,
		key:    s.prefix + "state/" + name,
	}
}

// Load implements token.Persistence, returning nil when the object is absent.
func (d *Document) Load() ([]byte, error) {
	ctx := context.Background()
	obj, err := d.client.GetObject(ctx, d.bucket, d.key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			d.observe("", true)
			return nil, nil
		}
		return nil, err
	}
	defer obj.Close()

	// GetObject is lazy, so the object may not actually exist until it is read.
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			d.observe("", true)
			return nil, nil
		}
		return nil, fmt.Errorf("s3: read %s: %w", d.key, err)
	}
	info, err := obj.Stat()
	if err != nil {
		if isNotFound(err) {
			d.observe("", true)
			return nil, nil
		}
		return nil, err
	}
	d.observe(info.ETag, true)
	return data, nil
}

// Save implements token.Persistence. The write is conditioned on the version
// observed by the last Load or Save; see the type comment.
func (d *Document) Save(data []byte) error {
	d.mu.Lock()
	etag, seen := d.etag, d.seen
	d.mu.Unlock()

	opts := minio.PutObjectOptions{ContentType: "application/yaml"}
	switch {
	case seen && etag != "":
		// Replace exactly the version we read.
		opts.SetMatchETag(etag)
	case seen && etag == "":
		// We believe nothing is there; refuse if someone created it first.
		opts.SetMatchETagExcept("*")
	}

	info, err := d.client.PutObject(context.Background(), d.bucket, d.key,
		bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if isPreconditionFailed(err) {
			// Our cached version is stale; the next Load re-syncs.
			d.observe("", false)
			return fmt.Errorf("%w: %s", ErrConflict, d.key)
		}
		return fmt.Errorf("s3: write %s: %w", d.key, err)
	}
	d.observe(info.ETag, true)
	return nil
}

func (d *Document) observe(etag string, seen bool) {
	d.mu.Lock()
	d.etag, d.seen = etag, seen
	d.mu.Unlock()
}

// isPreconditionFailed reports whether err is a rejected conditional write.
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusPreconditionFailed ||
		resp.StatusCode == http.StatusConflict ||
		resp.Code == "PreconditionFailed"
}
