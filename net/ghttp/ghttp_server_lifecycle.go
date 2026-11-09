// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

package ghttp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/gogf/gf/v2/os/genv"
	"github.com/gogf/gf/v2/os/gproc"
)

// Done closes after startup rollback or after every underlying Serve has ended
// and synchronous shutdown has completed. It does not install signal handlers.
func (s *Server) Done() <-chan struct{} { return s.lifecycleDone }

// Err returns startup, serving and cleanup errors. Read it after Done for the
// final result. Graceful cancellation does not add an error.
func (s *Server) Err() error {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.lifecycleErr
}

func (s *Server) recordLifecycleError(err error) {
	if err == nil {
		return
	}
	s.resultMu.Lock()
	s.lifecycleErr = errors.Join(s.lifecycleErr, err)
	s.resultMu.Unlock()
}

// shutdownLocked is shared by Start rollback, explicit Shutdown and the first
// Serve exit. Serve result publication uses a separate mutex and never waits on
// this lock, allowing every Serve to be joined here.
func (s *Server) shutdownLocked() error {
	if s.lifecycleStopped {
		return s.Err()
	}
	if !s.lifecycleStarted {
		s.claimInheritedDescriptors()
	}
	s.lifecycleStopped = true
	reloadStopCause := s.Err()
	if reloadStopCause == nil {
		reloadStopCause = fmt.Errorf("HTTP server %s stopped before reload handoff", s.instance)
	}
	reloadChildStopping(reloadStopCause)
	ctx := context.Background()
	cleanup := func(name string, fn func() error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.recordLifecycleError(fmt.Errorf("HTTP %s panic: %v", name, recovered))
			}
		}()
		s.recordLifecycleError(fn())
	}
	// Preserve existing plugin/deregistration-before-listener shutdown order.
	for _, plugin := range s.installedPlugins {
		cleanup("plugin removal", plugin.Remove)
	}
	s.installedPlugins = nil
	cleanup("service deregistration", func() error { s.doServiceDeregister(); return nil })
	for _, server := range s.servers {
		// One listener timing out must not skip the remaining listeners or
		// Serve joins. The final result is shared by all Shutdown callers.
		s.recordLifecycleError(server.Shutdown(ctx))
	}
	// Custom listeners already belong to the owner before Start. They may not
	// have a graceful.Server yet when session/plugin initialization fails.
	for _, listener := range s.config.Listeners {
		_ = listener.Close()
	}
	for _, file := range s.lifecycleInherited {
		_ = file.Close()
	}
	s.lifecycleInherited = nil
	s.serveWG.Wait()
	serverMapping.LockFunc(func(mapping map[string]*Server) {
		if mapping[s.instance] == s {
			delete(mapping, s.instance)
		}
	})
	if s.lifecycleCounted {
		s.lifecycleCounted = false
		if serverRunning.Add(-1) == 0 {
			select {
			case allShutdownChan <- struct{}{}:
			default:
			}
		}
	}
	select {
	case s.closeChan <- struct{}{}:
	default:
	}
	close(s.lifecycleDone)
	reloadChildFailed(reloadStopCause)
	s.Logger().Infof(ctx, "pid[%d]: all servers shutdown", gproc.Pid())
	return s.Err()
}

// claimInheritedDescriptor shares one os.File object so repeated cleanup cannot
// accidentally close a reused descriptor number after FileListener duplicates it.
func (s *Server) claimInheritedDescriptor(fd int) *os.File {
	if fd <= 0 || runtime.GOOS == "windows" {
		return nil
	}
	if s.lifecycleInherited == nil {
		s.lifecycleInherited = make(map[int]*os.File)
	}
	if file := s.lifecycleInherited[fd]; file != nil {
		return file
	}
	file := os.NewFile(uintptr(fd), "")
	s.lifecycleInherited[fd] = file
	return file
}

func (s *Server) claimInheritedDescriptors() {
	addresses := []string{s.config.Address, s.config.HTTPSAddr}
	if raw := genv.Get(adminActionReloadEnvKey).String(); raw != "" {
		manifest := bufferToServerFdMap([]byte(raw))
		inherited, ok := manifest[s.instance]
		if !ok {
			inherited, ok = manifest[s.config.Name]
		}
		if ok {
			addresses = append(addresses, inherited["http"], inherited["https"])
		}
	}
	for _, addresses := range addresses {
		for _, address := range strings.Split(addresses, ",") {
			parts := strings.Split(address, "#")
			if len(parts) > 1 {
				if fd, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
					s.claimInheritedDescriptor(fd)
				}
			}
		}
	}
}
