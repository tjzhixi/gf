//go:build windows

package ghttp

import "errors"

func markReloadDescriptorCloseOnExec(int) error {
	return errors.New("close-on-exec is unavailable for inherited HTTP reload descriptors on Windows")
}
