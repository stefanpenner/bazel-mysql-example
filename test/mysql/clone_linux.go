//go:build linux

package mysql

import (
	"os"
	"path/filepath"
	"syscall"
)

// FICLONE is _IOW(0x94, 9, int) on 64-bit Linux.
const ficlone = 0x40049409

func cloneTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out := filepath.Join(dst, rel)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			return os.MkdirAll(out, 0o750)
		}
		if d.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		return cloneFile(path, out, info.Mode().Perm()|0o600)
	})
	if err == nil {
		return nil
	}
	if isNoClone(err) {
		return errNoClone
	}
	return err
}

func cloneFile(src, dst string, perm os.FileMode) error {
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
	defer out.Close()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd())
	if errno != 0 {
		return errno
	}
	return os.Chmod(dst, perm)
}

func sharesClone(src, dst string) bool {
	return false
}

func diskClones(dir string) bool {
	return false
}

func isNoClone(err error) bool {
	return err == syscall.EXDEV || err == syscall.ENOTSUP || err == syscall.EOPNOTSUPP || err == syscall.ENOSYS || err == syscall.EINVAL
}
