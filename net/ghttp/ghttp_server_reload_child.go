// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

package ghttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const adminActionReloadReadyFDEnvKey = "GF_SERVER_RELOAD_READY_FD"

// One private inherited pipe belongs to one reload attempt. GPROC_PPID alone
// does not authorize replacing a parent, and the ordinary gproc message bus is
// deliberately not involved in readiness or shutdown authorization.
var reloadChildState struct {
	once  sync.Once
	child *reloadChildHandshake
	err   error
}

type reloadChildHandshake struct {
	mu       sync.Mutex
	file     *os.File
	expected map[string]listenerFdMap
	ready    map[string]*Server
	finished chan struct{}
	complete bool
	invalid  error
	timer    *time.Timer
	delay    time.Duration
}

func initReloadChild() error {
	reloadChildState.once.Do(func() {
		rawFD := os.Getenv(adminActionReloadReadyFDEnvKey)
		if rawFD == "" {
			return
		}
		fd, err := strconv.Atoi(rawFD)
		if err != nil || fd < 3 {
			reloadChildState.err = fmt.Errorf("invalid HTTP reload readiness descriptor %q", rawFD)
			return
		}
		child := &reloadChildHandshake{
			file:     os.NewFile(uintptr(fd), "http-reload-ready"),
			ready:    make(map[string]*Server),
			finished: make(chan struct{}),
		}
		reloadChildState.child = child
		if err := json.Unmarshal([]byte(os.Getenv(adminActionReloadEnvKey)), &child.expected); err != nil {
			reloadChildState.err = fmt.Errorf("decode HTTP reload manifest: %w", err)
			return
		}
		if len(child.expected) == 0 {
			reloadChildState.err = errors.New("HTTP reload manifest contains no servers")
			return
		}
		// ExtraFiles intentionally clears close-on-exec for this process. Restore
		// it for the entire inherited set before plugin initialization can launch
		// another process. FileListener duplicates each descriptor and consumes
		// the original, so this does not affect the current child.
		if err := markReloadDescriptorCloseOnExec(fd); err != nil {
			reloadChildState.err = fmt.Errorf("protect HTTP reload readiness descriptor: %w", err)
			return
		}
		for instance, listeners := range child.expected {
			for protocol, entries := range listeners {
				for _, item := range strings.Split(entries, ",") {
					item = strings.TrimSpace(item)
					if item == "" {
						continue
					}
					_, rawFD, ok := strings.Cut(item, "#")
					inheritedFD, err := strconv.Atoi(strings.TrimSpace(rawFD))
					if !ok || err != nil || inheritedFD < 3 {
						reloadChildState.err = fmt.Errorf("invalid HTTP reload %s listener descriptor for %q", protocol, instance)
						return
					}
					if err := markReloadDescriptorCloseOnExec(inheritedFD); err != nil {
						reloadChildState.err = fmt.Errorf("protect HTTP reload %s listener descriptor %d: %w", protocol, inheritedFD, err)
						return
					}
				}
			}
		}
	})
	return reloadChildState.err
}

// reloadChildStarted runs only after this Server's full Start succeeds. It
// waits for every inherited listener to enter Serve's Accept path, then joins
// the process-wide barrier. Configuration is immutable after Start.
func reloadChildStarted(s *Server) {
	if initReloadChild() != nil || reloadChildState.child == nil {
		return
	}
	child := reloadChildState.child
	key := s.instance
	expected, ok := child.expected[key]
	if !ok {
		// Older manifests used the configured name. Prefer the instance identity;
		// duplicate configured-name claims are rejected below rather than counting
		// one server twice for another instance.
		key = s.config.Name
		expected, ok = child.expected[key]
	}
	if !ok {
		return
	}
	if err := validateReloadListenerCount(s, expected); err != nil {
		reloadChildFailed(err)
		return
	}
	go func() {
		for _, listener := range s.servers {
			select {
			case <-listener.Ready():
			case <-s.Done():
				reloadChildFailed(fmt.Errorf("HTTP reload server %q stopped before listener readiness", key))
				return
			case <-child.finished:
				return
			}
		}
		child.mu.Lock()
		defer child.mu.Unlock()
		if child.complete || child.invalid != nil {
			return
		}
		if previous := child.ready[key]; previous != nil && previous != s {
			child.finishLocked(false, fmt.Errorf("HTTP reload manifest server %q claimed by multiple instances", key))
			return
		}
		if err := reloadServerReadyError(s); err != nil {
			child.finishLocked(false, err)
			return
		}
		child.ready[key] = s
		if delay := time.Duration(s.config.GracefulTimeout) * time.Second; delay > child.delay {
			child.delay = delay
		}
		if len(child.ready) == len(child.expected) && child.timer == nil {
			// Preserve the configured grace delay, starting it only after the whole
			// inherited server set is ready rather than after the first Server.Start.
			child.timer = time.AfterFunc(child.delay, child.commit)
		}
	}()
}

func validateReloadListenerCount(s *Server, expected listenerFdMap) error {
	// The manifest records real bound addresses. Equal counts alone could
	// acknowledge a plugin that silently moved a listener to another port.
	expectedAddresses := make(map[string]int)
	for _, protocol := range []string{"http", "https"} {
		for _, item := range strings.Split(expected[protocol], ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			address, _, ok := strings.Cut(item, "#")
			if !ok {
				return fmt.Errorf("HTTP reload server %q has invalid %s listener entry", s.instance, protocol)
			}
			expectedAddresses[protocol+" "+address]++
		}
	}
	for _, listener := range s.servers {
		protocol := "http"
		if listener.IsHttps() {
			protocol = "https"
		}
		address := protocol + " " + listener.GetListenedAddress()
		if expectedAddresses[address] == 0 {
			return fmt.Errorf("HTTP reload server %q has unexpected listener %s", s.instance, address)
		}
		expectedAddresses[address]--
	}
	for address, count := range expectedAddresses {
		if count != 0 {
			return fmt.Errorf("HTTP reload server %q did not inherit listener %s", s.instance, address)
		}
	}
	return nil
}

func reloadServerReadyError(s *Server) error {
	select {
	case <-s.Done():
		return fmt.Errorf("HTTP reload server %q stopped before handoff", s.instance)
	default:
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("HTTP reload server %q failed before handoff: %w", s.instance, err)
	}
	for _, listener := range s.servers {
		if listener.Status() != ServerStatusRunning {
			return fmt.Errorf("HTTP reload server %q listener stopped before handoff", s.instance)
		}
	}
	return nil
}

func (child *reloadChildHandshake) commit() {
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.complete || child.invalid != nil {
		return
	}
	for _, server := range child.ready {
		if err := reloadServerReadyError(server); err != nil {
			child.finishLocked(false, err)
			return
		}
	}
	child.finishLocked(true, nil)
}

// reloadChildStopping prevents commit while the owner is still rolling back.
// The error frame is sent by reloadChildFailed only after that cleanup finishes.
func reloadChildStopping(err error) {
	if err == nil {
		return
	}
	_ = initReloadChild()
	child := reloadChildState.child
	if child == nil {
		return
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.complete {
		return
	}
	if child.invalid == nil {
		child.invalid = err
	}
	if child.timer != nil {
		child.timer.Stop()
	}
}

// reloadChildFailed aborts an uncommitted handoff. Start calls it after rollback;
// Shutdown also calls it after cleanup and before publishing its terminal state. It never
// acquires a Server lifecycle mutex, avoiding inversion with Start/Shutdown.
func reloadChildFailed(err error) {
	if err == nil {
		return
	}
	_ = initReloadChild()
	child := reloadChildState.child
	if child == nil {
		return
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.invalid != nil {
		err = child.invalid
	}
	child.finishLocked(false, err)
}

func (child *reloadChildHandshake) finishLocked(ready bool, err error) {
	if child.complete {
		return
	}
	child.complete = true
	if child.timer != nil {
		child.timer.Stop()
	}
	close(child.finished)
	message := struct {
		Ready bool   `json:"ready,omitempty"`
		Error string `json:"error,omitempty"`
	}{Ready: ready}
	if err != nil {
		message.Error = err.Error()
		// JSON escaping can expand one byte to six. Bound the entire frame
		// below the parent's 8 KiB decoder limit even for control characters.
		if len(message.Error) > 1024 {
			message.Error = message.Error[:1024]
		}
	}
	if ready {
		// The inherited descriptors have now been consumed by all expected
		// servers. Clear stale numbers before publishing successful handoff so
		// later same-process Start cannot mistake them for inherited listeners.
		_ = os.Unsetenv(adminActionReloadEnvKey)
		_ = os.Unsetenv(adminActionReloadReadyFDEnvKey)
	}
	// The parent treats a missing/partial frame or EOF as rollback. It owns the
	// candidate process and will reap it; this child cannot shut its parent down.
	_ = json.NewEncoder(child.file).Encode(message)
	_ = child.file.Close()
}
