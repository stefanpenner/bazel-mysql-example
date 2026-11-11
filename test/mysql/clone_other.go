//go:build !darwin && !linux

package mysql

func cloneTree(src, dst string) error {
	return errNoClone
}

func sharesClone(src, dst string) bool {
	return false
}

func diskClones(dir string) bool {
	return false
}
