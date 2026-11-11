// Package copy copies a MySQL data directory for a test.
// The copy is a new tree. The source stays unchanged.
package copy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var errNoClone = errors.New("datadir clone is not available")

// Run copies a data directory.
// args are the source path and the destination path.
func Run(args []string) error {
	if len(args) != 2 {
		return errors.New("copy-datadir needs a source path and a destination path")
	}
	return CopyDatadir(args[0], args[1])
}

// CopyDatadir copies the MySQL template into a private directory.
// A clone is used when the source and the destination are on one volume that supports it.
// Measured on this Mac on 11 Nov 2026. That template was 190 MB and the disk cache was warm.
// A full copy took 82 ms and wrote 190 MB of new disk.
// The template is now 81.6 MiB.
// An APFS clone took 25 ms and wrote 0.2 MB.
// After the server started and created one small table, the clone owned about 63 MB.
// The unread template blocks stayed shared. Resident memory stayed about 500 MB either way.
// A clone is a new file. A write in the copy leaves the template unchanged.
// Another volume, or a disk with no reflink, uses the full copy.
func CopyDatadir(src, dst string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	if err := cloneTree(resolved, dst); err != nil {
		os.RemoveAll(dst)
		if !errors.Is(err, errNoClone) {
			return err
		}
		return walkDatadir(resolved, dst)
	}
	if err := materializeSymlinks(resolved, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return makeWritable(dst)
}

func materializeSymlinks(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if info.IsDir() {
			return walkDatadir(resolved, out)
		}
		return copyFile(resolved, out, info.Mode().Perm()|0o600)
	})
}

func makeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()|0o200)
	})
}

func walkDatadir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(out, 0o750); err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return CopyDatadir(path, out)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("datadir file %s is not a regular file", path)
		}
		// Runfiles are read-only. The server must write ibdata1.
		return copyFile(path, out, info.Mode().Perm()|0o600)
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	// OpenFile applies the umask. Chmod keeps the owner write bit.
	return os.Chmod(dst, perm)
}
