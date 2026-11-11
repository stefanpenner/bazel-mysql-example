//go:build darwin

package mysql

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	sysClonefileat        = 462
	sysGetattrlist        = 220
	fsoptAttrCmnExtended  = 0x20
	attrCMNExtPrivateSize = 0x00000008
	attrCMNExtCloneID     = 0x00000100
)

func cloneTree(src, dst string) error {
	err := clonefileat(src, dst)
	if err == nil {
		return nil
	}
	if isNoClone(err) {
		return errNoClone
	}
	return err
}

func clonefileat(src, dst string) error {
	sp, err := syscall.BytePtrFromString(src)
	if err != nil {
		return err
	}
	dp, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	at := uintptr(^uint(1)) // AT_FDCWD is -2.
	_, _, errno := syscall.Syscall6(sysClonefileat, at, uintptr(unsafe.Pointer(sp)), at, uintptr(unsafe.Pointer(dp)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func sharesClone(src, dst string) bool {
	idA, _, okA := cloneMeta(src)
	idB, privateB, okB := cloneMeta(dst)
	if !okA || !okB || idA == 0 || idA != idB {
		return false
	}
	info, err := os.Stat(dst)
	if err != nil {
		return false
	}
	return privateB < uint64(info.Size())
}

func diskClones(dir string) bool {
	src := filepath.Join(dir, ".clone-probe-src")
	dst := filepath.Join(dir, ".clone-probe-dst")
	defer os.Remove(src)
	defer os.Remove(dst)
	if err := os.WriteFile(src, []byte("clone-probe-file"), 0o644); err != nil {
		return false
	}
	return clonefileat(src, dst) == nil
}

func cloneMeta(path string) (id, private uint64, ok bool) {
	type attrlist struct {
		bitmapcount uint16
		reserved    uint16
		commonattr  uint32
		volattr     uint32
		dirattr     uint32
		fileattr    uint32
		forkattr    uint32
	}
	al := attrlist{bitmapcount: 5, forkattr: attrCMNExtPrivateSize | attrCMNExtCloneID}
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var buf [32]byte
	_, _, errno := syscall.Syscall6(
		sysGetattrlist,
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		fsoptAttrCmnExtended,
		0,
	)
	if errno != 0 {
		return 0, 0, false
	}
	private = binary.LittleEndian.Uint64(buf[4:12])
	id = binary.LittleEndian.Uint64(buf[12:20])
	return id, private, true
}

func isNoClone(err error) bool {
	return err == syscall.EXDEV || err == syscall.ENOTSUP || err == syscall.EOPNOTSUPP || err == syscall.ENOSYS
}
