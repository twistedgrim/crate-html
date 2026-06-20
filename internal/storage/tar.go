package storage

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WriteDirAsTar walks src and writes every regular file (and directory entry)
// into w as a tar stream. Symbolic links are skipped. Paths in the archive are
// stored relative to src, with forward slashes.
func WriteDirAsTar(src string, w io.Writer) error {
	src = filepath.Clean(src)
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Use forward slashes in the archive regardless of host OS.
		archName := filepath.ToSlash(rel)

		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode()
		// Skip symlinks and special files.
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !d.IsDir()) {
			return nil
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = archName
		if d.IsDir() {
			hdr.Name = strings.TrimRight(archName, "/") + "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
}
