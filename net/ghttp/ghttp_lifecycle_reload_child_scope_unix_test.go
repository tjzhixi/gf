//go:build !windows

package ghttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gogf/gf/v2/os/gsession"
)

// An inherited descriptor is intentionally open across the reload exec, but
// must not escape again when the first server's plugin launches another program.
// Both instances are in the manifest before either Start has consumed its fd.
func TestLifecycleReloadChildDescriptorScope(t *testing.T) {
	switch os.Getenv("GF_TEST_RELOAD_FD_SCOPE") {
	case "grandchild":
		var expected map[int]string
		if err := json.Unmarshal([]byte(os.Getenv("GF_TEST_RELOAD_FD_IDENTITIES")), &expected); err != nil {
			t.Fatal(err)
		}
		for fd, identity := range expected {
			if actual, err := reloadTestDescriptorIdentity(fd); err == nil && actual == identity {
				t.Errorf("inherited reload descriptor %d escaped to plugin subprocess", fd)
			}
		}
		return
	case "child":
		reloadTestChildDescriptorScope(t)
		return
	}
	var files []*os.File
	manifest := make(map[string]listenerFdMap)
	for index, name := range []string{"scope-a", "scope-b"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		file, err := listener.(*net.TCPListener).File()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		defer file.Close()
		manifest[name] = listenerFdMap{"http": fmt.Sprintf("%s#%d", listener.Addr(), index+3)}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	buffer, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleReloadChildDescriptorScope$", "-test.count=1")
	child.ExtraFiles = append(files, writer)
	child.Env = reloadTestScopeEnvironment("child")
	child.Env = append(child.Env, adminActionReloadEnvKey+"="+string(buffer), adminActionReloadReadyFDEnvKey+"=5")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("reload child descriptor scope: %v\n%s", err, output)
	}
}

func reloadTestChildDescriptorScope(t *testing.T) {
	newServer := func(name string) *Server {
		s := GetServer(name)
		config := NewConfig()
		config.Name, config.Address = name, "127.0.0.1:0"
		config.GracefulTimeout = 0
		config.LogStdout, config.DumpRouterMap = false, false
		config.SessionStorage = gsession.NewStorageMemory()
		if err := s.SetConfig(config); err != nil {
			t.Fatal(err)
		}
		s.BindHandler("/health", func(r *Request) { r.Response.Write("ready") })
		return s
	}
	a, b := newServer("scope-a"), newServer("scope-b")
	a.Plugin(reloadScopePlugin{install: func() error {
		identities := make(map[int]string)
		for _, fd := range []int{3, 4, 5} {
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Errorf("reload descriptor %d is not close-on-exec before first plugin: flags=%d err=%v", fd, flags, err)
			}
			identity, err := reloadTestDescriptorIdentity(fd)
			if err != nil {
				return err
			}
			identities[fd] = identity
		}
		buffer, _ := json.Marshal(identities)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		grandchild := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleReloadChildDescriptorScope$", "-test.count=1")
		grandchild.Env = append(reloadTestScopeEnvironment("grandchild"), "GF_TEST_RELOAD_FD_IDENTITIES="+string(buffer))
		if output, err := grandchild.CombinedOutput(); err != nil {
			t.Errorf("plugin exec inherited reload descriptors: %v\n%s", err, output)
		}
		return nil
	}})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()
	select {
	case <-reloadChildState.child.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not finish readiness handshake")
	}
	// The finished signal wakes before the frame write/cleanup under this lock.
	reloadChildState.child.mu.Lock()
	reloadChildState.child.mu.Unlock()
	for _, key := range []string{adminActionReloadEnvKey, adminActionReloadReadyFDEnvKey} {
		if value := os.Getenv(key); value != "" {
			t.Errorf("completed reload retained stale descriptor environment %s=%q", key, value)
		}
	}
	originalPort := a.GetListenedPort()
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// Rebuilding an instance uses its new configuration, never stale fd numbers.
	replacement := newServer("scope-a")
	if err := replacement.Start(); err != nil {
		t.Fatalf("same-process replacement reused expired inherited descriptors: %v", err)
	}
	defer replacement.Shutdown()
	if replacement.GetListenedPort() == originalPort {
		t.Error("same-process replacement ignored its random-port configuration and reused expired manifest")
	}
}

func reloadTestDescriptorIdentity(fd int) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, stat.Mode), nil
}

func reloadTestScopeEnvironment(mode string) []string {
	var environment []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GF_SERVER_RELOAD") || key == "GF_SERVER_RESTART" || key == "GPROC_PPID" || strings.HasPrefix(key, "GF_TEST_RELOAD_FD_") {
			continue
		}
		environment = append(environment, item)
	}
	return append(environment, "GF_TEST_RELOAD_FD_SCOPE="+mode)
}

type reloadScopePlugin struct{ install func() error }

func (reloadScopePlugin) Name() string    { return "reload-scope" }
func (reloadScopePlugin) Author() string  { return "test" }
func (reloadScopePlugin) Version() string { return "1" }
func (reloadScopePlugin) Description() string {
	return "exec before consuming inherited listener descriptors"
}
func (p reloadScopePlugin) Install(*Server) error { return p.install() }
func (reloadScopePlugin) Remove() error           { return nil }
