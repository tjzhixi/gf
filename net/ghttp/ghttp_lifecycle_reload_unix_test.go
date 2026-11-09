//go:build !windows

package ghttp_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gsession"
)

// Real parent and replacement processes exercise the public reload API. The
// outer test process is never a GoFrame child and cannot receive an exit notice.
func TestLifecycleNativeReloadProcess(t *testing.T) {
	if directory := os.Getenv("GF_NATIVE_RELOAD_TEST_DIR"); directory != "" {
		nativeReloadProcessHelper(t, directory, os.Getenv("GF_NATIVE_RELOAD_TEST_MODE"))
		return
	}
	for _, mode := range []string{"success", "second-failure", "early-failure", "early-exit", "canceled", "retry-success", "second-generation", "http-restart"} {
		t.Run(mode, func(t *testing.T) { nativeReloadProcessScenario(t, mode) })
	}
}

type nativeReloadReady struct {
	PID       int      `json:"pid"`
	Endpoints []string `json:"endpoints"`
}
type nativeReloadResult struct {
	Error string `json:"error"`
}

func nativeReloadProcessScenario(t *testing.T, mode string) {
	directory := t.TempDir()
	logFile, err := os.Create(filepath.Join(directory, "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestLifecycleNativeReloadProcess$", "-test.v")
	command.Env = nativeReloadEnvironment(directory, mode)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	parentExited := false
	defer func() {
		_ = os.WriteFile(filepath.Join(directory, "stop"), nil, 0600)
		if !parentExited {
			select {
			case <-done:
				parentExited = true
			case <-time.After(2 * time.Second):
			}
		}
		// This group is created solely for this fixture, including all replacements.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !parentExited {
			<-done
		}
		if t.Failed() {
			content, _ := os.ReadFile(filepath.Join(directory, "process.log"))
			t.Logf("process evidence:\n%s", content)
		}
	}()
	var ready nativeReloadReady
	nativeReloadReadJSON(t, directory, "parent-ready", &ready)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true} // loopback fixture certificates only
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, endpoint := range ready.Endpoints {
		nativeReloadAssertHTTP(t, client, endpoint, ready.PID)
	}
	holdResult := make(chan string, 1)
	holdErr := make(chan error, 1)
	go func() {
		holdClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		request, _ := http.NewRequest(http.MethodGet, ready.Endpoints[2]+"/hold", nil)
		request.Header.Set("Authorization", "Bearer reload-fixture")
		response, err := holdClient.Do(request)
		if err != nil {
			holdErr <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			holdErr <- err
			return
		}
		holdResult <- string(body)
	}()
	nativeReloadWaitFile(t, directory, "parent-hold", 5*time.Second)
	adminResult := make(chan nativeReloadResult, 1)
	if mode == "http-restart" {
		nativeReloadWaitFile(t, directory, "admin-ready", 5*time.Second)
		go func() {
			adminClient := &http.Client{Transport: transport, Timeout: 12 * time.Second}
			request, _ := http.NewRequest(http.MethodGet, ready.Endpoints[0]+"/debug/admin/restart", nil)
			request.Header.Set("Authorization", "Bearer reload-fixture")
			response, err := adminClient.Do(request)
			result := nativeReloadResult{}
			if err != nil {
				result.Error = err.Error()
			} else {
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "server restarted" {
					result.Error = fmt.Sprintf("admin reload response: %d %q %v", response.StatusCode, body, readErr)
				}
			}
			adminResult <- result
		}()
	} else {
		nativeReloadWrite(t, directory, "reload", nil)
	}
	nativeReloadWaitFile(t, directory, "child-pid", 5*time.Second)
	if mode != "early-failure" && mode != "early-exit" {
		nativeReloadWaitFile(t, directory, "child-a-ready", 5*time.Second)
		// A has a one-second legacy timer. B's four-second budget leaves time to
		// prove that A alone must never authorize stopping the entire old process.
		until := time.Now().Add(2300 * time.Millisecond)
		for time.Now().Before(until) {
			if nativeReloadExists(directory, "parent-stopped") {
				t.Fatal("parent stopped before replacement B completed initialization")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if mode == "canceled" {
			nativeReloadWrite(t, directory, "cancel-reload", nil)
		} else {
			nativeReloadWrite(t, directory, "child-release", nil)
		}
	}
	var result nativeReloadResult
	if mode == "http-restart" {
		select {
		case result = <-adminResult:
		case <-time.After(8 * time.Second):
			t.Fatal("admin restart did not return before its own graceful shutdown")
		}
	} else {
		nativeReloadReadJSON(t, directory, "reload-result", &result)
	}
	if mode == "success" || mode == "second-generation" || mode == "http-restart" {
		if result.Error != "" {
			t.Fatalf("reload failed: %s", result.Error)
		}
		nativeReloadWaitFile(t, directory, "child-all-ready", 4*time.Second)
		// The replacement is ready, but the old process must still drain the
		// request accepted before reload instead of exiting immediately.
		if nativeReloadExists(directory, "parent-done") {
			t.Fatal("parent exited before accepted request completed")
		}
		nativeReloadWrite(t, directory, "hold-release", nil)
		select {
		case body := <-holdResult:
			if body != fmt.Sprintf("held:%d", ready.PID) {
				t.Fatalf("old request changed: %q", body)
			}
		case err := <-holdErr:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("parent request did not drain")
		}
		nativeReloadWaitFile(t, directory, "parent-done", 4*time.Second)
		select {
		case err := <-done:
			parentExited = true
			if err != nil {
				t.Fatalf("parent process failed: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("old parent did not exit after graceful handoff")
		}
		var childPID int
		nativeReloadReadJSON(t, directory, "child-pid", &childPID)
		for _, endpoint := range ready.Endpoints {
			nativeReloadAssertHTTP(t, client, endpoint, childPID)
		}
		if mode == "second-generation" {
			nativeReloadWrite(t, directory, "reload-generation-2", nil)
			var second nativeReloadResult
			nativeReloadReadJSON(t, directory, "generation-2-result", &second)
			if second.Error != "" {
				t.Fatalf("second-generation reload failed: %s", second.Error)
			}
			nativeReloadWaitFile(t, directory, "child-exited", 4*time.Second)
			var grandchildPID int
			nativeReloadReadJSON(t, directory, "grandchild-pid", &grandchildPID)
			for _, endpoint := range ready.Endpoints {
				nativeReloadAssertHTTP(t, client, endpoint, grandchildPID)
			}
			nativeReloadWrite(t, directory, "stop", nil)
			nativeReloadWaitFile(t, directory, "grandchild-exited", 3*time.Second)
		} else {
			nativeReloadWrite(t, directory, "stop", nil)
			nativeReloadWaitFile(t, directory, "child-exited", 3*time.Second)
		}
	} else {
		nativeReloadWrite(t, directory, "hold-release", nil)
		select {
		case <-holdResult:
		case err := <-holdErr:
			t.Error(err)
		case <-time.After(time.Second):
			t.Error("parent request did not drain after failed replacement")
		}
		if result.Error == "" {
			t.Error("failed replacement startup was reported as successful reload")
		}
		var failedPID int
		nativeReloadReadJSON(t, directory, "child-pid", &failedPID)
		// A failed/canceled replacement must be reaped before failure returns.
		if err := syscall.Kill(failedPID, 0); err != syscall.ESRCH {
			t.Errorf("failed child %d is still present after reload returned: %v", failedPID, err)
		}
		if nativeReloadExists(directory, "parent-stopped") {
			t.Error("failed child stopped the still-serving parent")
		}
		for _, endpoint := range ready.Endpoints {
			nativeReloadAssertHTTP(t, client, endpoint, ready.PID)
		}
		if mode == "retry-success" {
			nativeReloadWrite(t, directory, "retry-reload", nil)
			var retry nativeReloadResult
			nativeReloadReadJSON(t, directory, "retry-result", &retry)
			if retry.Error != "" {
				t.Fatalf("reload did not recover after failed child: %s", retry.Error)
			}
			nativeReloadWaitFile(t, directory, "parent-done", 4*time.Second)
			select {
			case err := <-done:
				parentExited = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retried reload did not retire parent")
			}
			var retryPID int
			nativeReloadReadJSON(t, directory, "retry-child-pid", &retryPID)
			for _, endpoint := range ready.Endpoints {
				nativeReloadAssertHTTP(t, client, endpoint, retryPID)
			}
			nativeReloadWrite(t, directory, "stop", nil)
			nativeReloadWaitFile(t, directory, "retry-child-exited", 3*time.Second)
		}
	}
}

func nativeReloadProcessHelper(t *testing.T, directory, mode string) {
	generation, _ := strconv.Atoi(os.Getenv("GF_NATIVE_RELOAD_TEST_GENERATION"))
	child := os.Getenv("GF_SERVER_RELOAD") != ""
	role := "parent"
	if child {
		role = "child"
	}
	if generation > 1 {
		role = "grandchild"
	}
	retry := os.Getenv("GF_NATIVE_RELOAD_TEST_ATTEMPT") == "2"
	if retry {
		role = "retry-child"
	}
	if child {
		nativeReloadWrite(t, directory, role+"-pid", os.Getpid())
		if mode == "early-exit" {
			os.Exit(23)
		}
		defer nativeReloadWrite(t, directory, role+"-exited", nil)
	}
	certificate, err := tls.LoadX509KeyPair("testdata/https/files/server.crt", "testdata/https/files/server.key")
	if err != nil {
		t.Fatal(err)
	}
	a := nativeReloadServer(t, "reload-a", 1, directory, role)
	a.SetTLSConfig(&tls.Config{Certificates: []tls.Certificate{certificate}})
	a.SetHTTPSAddr("127.0.0.1:0")
	if mode == "http-restart" {
		a.EnableAdmin("/debug/admin")
	}
	b := nativeReloadServer(t, "reload-b", 4, directory, role)
	if child {
		if mode == "early-failure" {
			a.Plugin(nativeReloadPlugin{install: func() error { return errors.New("fixture early startup failure") }})
		}
		b.Plugin(nativeReloadPlugin{install: func() error {
			deadline := time.Now().Add(15 * time.Second)
			for !nativeReloadExists(directory, "child-release") {
				if nativeReloadExists(directory, "stop") || time.Now().After(deadline) {
					return errors.New("fixture child startup canceled")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if mode == "second-failure" || (mode == "retry-success" && !retry) {
				return errors.New("fixture second startup failure")
			}
			return nil
		}})
	}
	if err := a.Start(); err != nil {
		if child && mode == "early-failure" {
			return
		}
		t.Fatal(err)
	}
	defer a.Shutdown()
	if child {
		nativeReloadWrite(t, directory, role+"-a-ready", nil)
	}
	if err := b.Start(); err != nil {
		if child && (mode == "second-failure" || mode == "retry-success") {
			return
		}
		t.Fatal(err)
	}
	defer b.Shutdown()
	if child {
		nativeReloadWrite(t, directory, role+"-all-ready", nil)
	} else {
		ready := nativeReloadReady{PID: os.Getpid(), Endpoints: []string{fmt.Sprintf("http://127.0.0.1:%d", a.GetListenedPort()), fmt.Sprintf("https://127.0.0.1:%d", a.GetListenedHTTPSPort()), fmt.Sprintf("http://127.0.0.1:%d", b.GetListenedPort())}}
		nativeReloadWrite(t, directory, "parent-ready", ready)
	}
	deadline := time.Now().Add(30 * time.Second)
	var reloadDone chan struct{}
	var cancelReload context.CancelFunc
	reloadStarted, retryStarted := false, false
	observedRunning := false
	lastReload := time.Now()
	defer func() {
		if cancelReload != nil {
			cancelReload()
		}
		if reloadDone != nil {
			<-reloadDone
		}
	}()
	launch := func(resultMarker string) {
		reloadStarted = true
		lastReload = time.Now()
		reloadDone = make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		cancelReload = cancel
		if err := os.Setenv("GF_NATIVE_RELOAD_TEST_GENERATION", strconv.Itoa(generation+1)); err != nil {
			t.Fatal(err)
		}
		done := reloadDone
		go func() {
			defer close(done)
			defer cancel()
			err := ghttp.RestartAllServer(ctx, os.Args[0])
			result := nativeReloadResult{}
			if err != nil {
				result.Error = err.Error()
			}
			nativeReloadWrite(t, directory, resultMarker, result)
		}()
	}
	for {
		if !child && mode == "http-restart" && time.Since(lastReload) > 2100*time.Millisecond && !nativeReloadExists(directory, "admin-ready") {
			nativeReloadWrite(t, directory, "admin-ready", nil)
		}
		if nativeReloadExists(directory, "stop") || time.Now().After(deadline) {
			return
		}
		if !child && !reloadStarted && nativeReloadExists(directory, "reload") && time.Since(lastReload) > 2100*time.Millisecond {
			launch("reload-result")
		}
		if child && generation == 1 && mode == "second-generation" && !reloadStarted && nativeReloadExists(directory, "reload-generation-2") && time.Since(lastReload) > 2100*time.Millisecond {
			launch("generation-2-result")
		}
		if !child && cancelReload != nil && nativeReloadExists(directory, "cancel-reload") {
			cancelReload()
		}
		if !child && mode == "retry-success" && !retryStarted && nativeReloadExists(directory, "retry-reload") && time.Since(lastReload) > 2100*time.Millisecond {
			select {
			case <-reloadDone:
				retryStarted = true
				if err := os.Setenv("GF_NATIVE_RELOAD_TEST_ATTEMPT", "2"); err != nil {
					t.Fatal(err)
				}
				launch("retry-result")
			default:
			}
		}
		// Done may still wait for /hold even after accepting has stopped.
		// Both Start calls have returned, so Status safely observes either
		// Serve ending and catches premature shutdown regardless of map order.
		if a.Status() == ghttp.ServerStatusRunning && b.Status() == ghttp.ServerStatusRunning {
			observedRunning = true
		} else if observedRunning {
			nativeReloadWrite(t, directory, role+"-stopped", nil)
		}
		select {
		case <-a.Done():
			select {
			case <-b.Done():
				nativeReloadWrite(t, directory, role+"-done", nil)
				return
			default:
			}
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nativeReloadServer(t *testing.T, name string, budget int, directory, role string) *ghttp.Server {
	s := ghttp.GetServer(name)
	config := ghttp.NewConfig()
	config.Name = name
	config.Address = "127.0.0.1:0"
	config.Graceful = true
	config.GracefulTimeout = budget
	config.SessionStorage = gsession.NewStorageMemory()
	config.LogStdout = false
	config.DumpRouterMap = false
	config.AccessLogEnabled = false
	config.ErrorLogEnabled = false
	if err := s.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	s.Use(func(r *ghttp.Request) {
		if r.Header.Get("Authorization") != "Bearer reload-fixture" {
			r.Response.WriteStatus(http.StatusUnauthorized, "denied")
			return
		}
		r.Middleware.Next()
	})
	s.BindHandler("/who", func(r *ghttp.Request) { r.Response.Write(strconv.Itoa(os.Getpid())) })
	s.BindHandler("/hold", func(r *ghttp.Request) {
		nativeReloadWrite(t, directory, role+"-hold", nil)
		for !nativeReloadExists(directory, "hold-release") && !nativeReloadExists(directory, "stop") {
			time.Sleep(5 * time.Millisecond)
		}
		r.Response.Write(fmt.Sprintf("held:%d", os.Getpid()))
	})
	return s
}

type nativeReloadPlugin struct{ install func() error }

func (nativeReloadPlugin) Name() string                  { return "reload-fixture" }
func (nativeReloadPlugin) Author() string                { return "wechat-bot" }
func (nativeReloadPlugin) Version() string               { return "1" }
func (nativeReloadPlugin) Description() string           { return "delays or rejects child startup" }
func (p nativeReloadPlugin) Install(*ghttp.Server) error { return p.install() }
func (nativeReloadPlugin) Remove() error                 { return nil }

func nativeReloadEnvironment(directory, mode string) []string {
	var result []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "GF_SERVER_RESTART" || key == "GF_SERVER_RELOAD" || key == "GF_SERVER_RELOAD_READY_FD" || key == "GPROC_PPID" || strings.HasPrefix(key, "GF_NATIVE_RELOAD_TEST_") {
			continue
		}
		result = append(result, item)
	}
	return append(result, "GF_NATIVE_RELOAD_TEST_DIR="+directory, "GF_NATIVE_RELOAD_TEST_MODE="+mode)
}
func nativeReloadExists(directory, name string) bool {
	_, err := os.Stat(filepath.Join(directory, name))
	return err == nil
}
func nativeReloadWrite(t *testing.T, directory, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Error(err)
		return
	}
	temp, err := os.CreateTemp(directory, "marker-")
	if err != nil {
		t.Error(err)
		return
	}
	file := temp.Name()
	defer os.Remove(file)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		t.Error(err)
		return
	}
	if err = temp.Close(); err != nil {
		t.Error(err)
		return
	}
	if err = os.Rename(file, filepath.Join(directory, name)); err != nil {
		t.Error(err)
	}
}
func nativeReloadWaitFile(t *testing.T, directory, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !nativeReloadExists(directory, name) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func nativeReloadReadJSON(t *testing.T, directory, name string, value any) {
	t.Helper()
	nativeReloadWaitFile(t, directory, name, 8*time.Second)
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
func nativeReloadAssertHTTP(t *testing.T, client *http.Client, endpoint string, pid int) {
	t.Helper()
	for _, authorized := range []bool{false, true} {
		request, _ := http.NewRequest(http.MethodGet, endpoint+"/who", nil)
		if authorized {
			request.Header.Set("Authorization", "Bearer reload-fixture")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Errorf("%s unavailable: %v", endpoint, err)
			continue
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		wantStatus, wantBody := http.StatusUnauthorized, "denied"
		if authorized {
			wantStatus, wantBody = http.StatusOK, strconv.Itoa(pid)
		}
		if err != nil || response.StatusCode != wantStatus || string(body) != wantBody {
			t.Errorf("%s auth=%t got %d %q %v, want %d %q", endpoint, authorized, response.StatusCode, body, err, wantStatus, wantBody)
		}
	}
}
