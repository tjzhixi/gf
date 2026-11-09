// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

package ghttp

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// ClaimReloadDescriptors owns HTTP inheritance before application initialization
// can fail. Start and rollback use the same os.File objects, so cleanup never
// rewraps a consumed descriptor number. This does not start any listener.
func ClaimReloadDescriptors() error {
	manifest := bufferToServerFdMap([]byte(os.Getenv(adminActionReloadEnvKey)))
	for instance, listeners := range manifest {
		s := GetServer(instance)
		s.lifecycleMu.Lock()
		for _, entries := range listeners {
			for _, item := range strings.Split(entries, ",") {
				if item == "" {
					continue
				}
				_, raw, ok := strings.Cut(item, "#")
				fd, err := strconv.Atoi(raw)
				if !ok || err != nil || fd < 3 {
					s.lifecycleMu.Unlock()
					return errors.New("invalid HTTP inherited descriptor")
				}
				s.claimInheritedDescriptor(fd)
			}
		}
		s.lifecycleMu.Unlock()
	}
	return nil
}

// CloseUnstartedReloadDescriptors releases only descriptors, leaving normal
// started Server shutdown to its application owner.
func CloseUnstartedReloadDescriptors() {
	for _, s := range serverMapping.Values() {
		s.lifecycleMu.Lock()
		if !s.lifecycleStarted {
			for _, file := range s.lifecycleInherited {
				_ = file.Close()
			}
			s.lifecycleInherited = nil
		}
		s.lifecycleMu.Unlock()
	}
}

// SetReloadHandler delegates native reload to the application that owns the
// complete command. Configure before Start; ordinary servers retain native
// HTTP-only reload. The handler must own candidate cleanup and parent shutdown.
func (s *Server) SetReloadHandler(handler func(context.Context, string) error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.lifecycleStarted || s.lifecycleStopped {
		panic("configure HTTP reload handler before Start")
	}
	s.applicationReload = handler
}

// SetAcceptGate prevents a prepared application candidate from taking requests
// from shared listeners. closed signals shutdown, including before activation.
func (s *Server) SetAcceptGate(gate func(context.Context, <-chan struct{}) error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.lifecycleStarted || s.lifecycleStopped {
		panic("configure HTTP accept gate before Start")
	}
	s.applicationAccept = gate
}

// ReloadSnapshot owns exported copies, never the original listeners.
type ReloadSnapshot struct{ export *reloadExport }

func ExportReloadSnapshot() (*ReloadSnapshot, error) {
	export, err := exportReloadListeners()
	if err != nil {
		return nil, err
	}
	return &ReloadSnapshot{export: export}, nil
}

func (snapshot *ReloadSnapshot) Files() []*os.File { return snapshot.export.files }
func (snapshot *ReloadSnapshot) Close()            { snapshot.export.closeFiles() }

func (snapshot *ReloadSnapshot) GraceDelay() time.Duration { return snapshot.export.graceDelay }

// Environment carries HTTP inheritance without enabling the HTTP-only child
// readiness protocol. The application supplies its own whole-command barrier.
func (snapshot *ReloadSnapshot) Environment() ([]string, error) {
	manifest, err := marshalReloadManifest(snapshot.export.manifest)
	if err != nil {
		return nil, err
	}
	env := reloadEnvironment(manifest, -1)
	return env[:len(env)-1], nil
}

// Commit orders handoff with concurrent Shutdown across the complete snapshot.
// publish must be bounded and must not reenter a Server lifecycle method.
func (snapshot *ReloadSnapshot) Commit(ctx context.Context, publish func() error) error {
	for _, server := range snapshot.export.servers {
		server.lifecycleMu.Lock()
	}
	defer func() {
		for index := len(snapshot.export.servers) - 1; index >= 0; index-- {
			snapshot.export.servers[index].lifecycleMu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, server := range snapshot.export.servers {
		if server.lifecycleStopped || !server.lifecycleCounted || server.Err() != nil {
			return errors.New("HTTP parent stopped before application handoff")
		}
	}
	return publish()
}

// WaitReady observes all net/http accept loops without a status polling race.
func (s *Server) WaitReady(ctx context.Context) error {
	s.lifecycleMu.Lock()
	servers := append(s.servers[:0:0], s.servers...)
	s.lifecycleMu.Unlock()
	for _, server := range servers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.Done():
			return errors.New("HTTP server stopped before readiness")
		case <-server.Ready():
		}
	}
	return s.Err()
}

// ValidateReloadReadiness verifies the entire inherited HTTP manifest before an
// application-wide prepared report. It includes protocol and actual addresses.
func ValidateReloadReadiness(ctx context.Context) error {
	manifest := bufferToServerFdMap([]byte(os.Getenv(adminActionReloadEnvKey)))
	for instance, expected := range manifest {
		server := serverMapping.Get(instance)
		if server == nil {
			return errors.New("inherited HTTP server was not started")
		}
		server.lifecycleMu.Lock()
		started, stopped := server.lifecycleCounted, server.lifecycleStopped
		if !started || stopped {
			server.lifecycleMu.Unlock()
			return errors.New("inherited HTTP server was not started")
		}
		err := validateReloadListenerCount(server, expected)
		server.lifecycleMu.Unlock()
		if err != nil {
			return err
		}
		if err = server.WaitReady(ctx); err != nil {
			return err
		}
	}
	return nil
}
