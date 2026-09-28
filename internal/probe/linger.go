package probe

import (
	"errors"
	"os"
	"path/filepath"
)

// lingerEnabledIn 回傳 linger 是否啟用，以及有沒有量到。
func lingerEnabledIn(root, unixUser string) (enabled, measured bool) {
	if root == "" || unixUser == "" {
		return false, false
	}
	_, err := os.Stat(filepath.Join(root, unixUser))
	if err == nil {
		return true, true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	return false, false
}
