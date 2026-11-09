// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

//go:build windows

package graceful

import (
	"errors"
	"net"
	"os"
)

func duplicateListenerFile(net.Listener) (*os.File, error) {
	return nil, errors.New("inherited HTTP listener reload is not supported on Windows")
}
