//go:build !windows

package ghttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Failed exec must release every exported duplicate, while leaving the running
// parent's original listener usable. A subprocess confines descriptors leaked
// by the old implementation; disabling GC prevents a finalizer hiding the bug.
func TestLifecycleReloadExportRollback(t *testing.T) {
	if mode := os.Getenv("GF_TEST_RELOAD_EXPORT"); mode != "" {
		debug.SetGCPercent(-1)
		testLifecycleReloadExportChild(t, mode)
		return
	}
	for _, mode := range []string{"single-exec-failure", "repeated-exec-failure", "unsupported-listener", "partial-export-failure", "candidate-no-readiness"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleReloadExportRollback$", "-test.count=1")
			for _, variable := range os.Environ() {
				key, _, _ := strings.Cut(variable, "=")
				if strings.HasPrefix(key, "GF_SERVER_RELOAD") || key == "GF_SERVER_RESTART" || key == "GPROC_PPID" || key == "GF_TEST_RELOAD_EXPORT" {
					continue
				}
				child.Env = append(child.Env, variable)
			}
			child.Env = append(child.Env, "GF_TEST_RELOAD_EXPORT="+mode)
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			output, err := child.CombinedOutput()
			if child.Process != nil {
				_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			}
			if err != nil {
				t.Fatalf("reload export rollback failed: %v\n%s", err, output)
			}
		})
	}
}

type reloadListenerWithoutFile struct{ net.Listener }

func testLifecycleReloadExportChild(t *testing.T, mode string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	name := fmt.Sprintf("reload-export-%d", time.Now().UnixNano())
	s := GetServer(name)
	config := NewConfig()
	config.Name, config.SessionPath = name, t.TempDir()
	config.LogStdout, config.DumpRouterMap = false, false
	config.AccessLogEnabled, config.ErrorLogEnabled = false, false
	if err := s.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	var supplied net.Listener = listener
	if mode == "unsupported-listener" {
		supplied = &reloadListenerWithoutFile{Listener: listener}
	}
	listeners := []net.Listener{supplied}
	if mode == "partial-export-failure" {
		second, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		listeners = append(listeners, &reloadListenerWithoutFile{Listener: second})
	}
	if err := s.SetListener(listeners...); err != nil {
		t.Fatal(err)
	}
	s.BindHandler("/health", func(r *Request) { r.Response.Write("parent-alive") })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	checkParent := func() {
		t.Helper()
		for _, owned := range listeners {
			response, err := client.Get("http://" + owned.Addr().String() + "/health")
			if err != nil {
				t.Fatalf("parent unavailable after reload failure: %v", err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "parent-alive" {
				t.Fatalf("parent response changed: %q %v", body, err)
			}
		}
	}
	checkParent()
	unexecutable := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(unexecutable, []byte("not executable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var candidatePIDFile string
	candidateReaped := false
	if mode == "candidate-no-readiness" {
		candidatePIDFile = filepath.Join(t.TempDir(), "candidate.pid")
		// exec keeps the same PID: this candidate owns no untracked grandchild.
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$$\" > '%s'\nexec /bin/sleep 10\n", strings.ReplaceAll(candidatePIDFile, "'", "'\"'\"'"))
		if err := os.WriteFile(unexecutable, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unexecutable, 0700); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if candidateReaped {
				return
			}
			if pid := reloadExportCandidatePID(candidatePIDFile); pid > 0 {
				process, _ := os.FindProcess(pid)
				_ = process.Kill()
				_, _ = process.Wait()
			}
		}()
	}
	attempts := 1
	if mode == "repeated-exec-failure" {
		attempts = 4
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var reloadErr error
		var recovered any
		if mode == "candidate-no-readiness" {
			// Start scheduling is not part of the cancellation assertion.
			// Observe the candidate PID before canceling the waiting caller.
			reloadErr, recovered = reloadExportCancelAfterCandidateStarts(t, unexecutable, candidatePIDFile)
			if !errors.Is(reloadErr, context.Canceled) {
				t.Errorf("missing readiness must respect caller cancellation: %v", reloadErr)
			}
			if pid := reloadExportCandidatePID(candidatePIDFile); pid <= 0 {
				t.Error("reload candidate did not start")
			} else if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Errorf("canceled candidate %d not killed and reaped: %v", pid, err)
			} else {
				candidateReaped = true
			}
		} else {
			func() {
				defer func() { recovered = recover() }()
				reloadErr = forkReloadProcess(context.Background(), unexecutable)
			}()
		}

		if recovered != nil {
			t.Errorf("reload export panicked instead of returning error: %v", recovered)
		} else if reloadErr == nil {
			t.Error("unusable reload candidate unexpectedly succeeded")
		}
		checkParent()
	}
	client.CloseIdleConnections()
	if err := s.Shutdown(); err != nil {
		t.Fatal(err)
	}
	peer, err := net.DialTimeout("tcp", listener.Addr().String(), 200*time.Millisecond)
	if err == nil {
		_ = peer.Close()
		t.Errorf("exported descriptor kept parent port %s open after %d failed attempts and Shutdown", listener.Addr(), attempts)
	}
}

func reloadExportCandidatePID(path string) int {
	content, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(content)))
	return pid
}

// Cancellation is triggered only after the shell has published its PID, so a
// busy test host cannot kill the child before the assertion has an identity.
func reloadExportCancelAfterCandidateStarts(t *testing.T, executable, pidFile string) (error, any) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err        error
		panicValue any
	}
	done := make(chan result, 1)
	go func() {
		value := result{}
		defer func() {
			value.panicValue = recover()
			done <- value
		}()
		value.err = forkReloadProcess(ctx, executable)
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	startup := time.NewTimer(5 * time.Second)
	defer startup.Stop()
	for reloadExportCandidatePID(pidFile) == 0 {
		select {
		case value := <-done:
			t.Errorf("reload returned before candidate startup was observed: %v", value.err)
			return value.err, value.panicValue
		case <-startup.C:
			t.Error("candidate did not publish its PID within startup allowance")
			cancel()
			select {
			case value := <-done:
				return value.err, value.panicValue
			case <-time.After(2 * time.Second):
				t.Fatal("reload did not return after startup allowance cancellation")
			}
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case value := <-done:
		return value.err, value.panicValue
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not kill and reap candidate after caller cancellation")
		return nil, nil
	}
}
