// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

//go:build !windows

package graceful

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// duplicateListenerFile deliberately avoids net.TCPListener.File. Its returned
// os.File is a kindSock: exec.Cmd obtains Fd(), which switches the shared open
// file description to blocking mode and can strand the parent's Accept during
// reload. NewFile on an already nonblocking dup preserves O_NONBLOCK in Fd().
func duplicateListenerFile(listener net.Listener) (*os.File, error) {
	connection, ok := listener.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("listener %T does not support nonblocking descriptor export", listener)
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, err
	}
	descriptor := -1
	var duplicateErr error
	err = raw.Control(func(fd uintptr) {
		// Synchronize the Dup + CLOEXEC pair with other execs. Only explicit
		// ExtraFiles may inherit this descriptor, including concurrent commands.
		syscall.ForkLock.RLock()
		descriptor, duplicateErr = syscall.Dup(int(fd))
		if duplicateErr == nil {
			syscall.CloseOnExec(descriptor)
		}
		syscall.ForkLock.RUnlock()
	})
	if err != nil {
		if descriptor >= 0 {
			_ = syscall.Close(descriptor)
		}
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	return os.NewFile(uintptr(descriptor), "http-reload-listener"), nil
}
