//go:build !windows

package ghttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gogf/gf/v2/os/gsession"
)

func TestLifecycleApplicationClaimClosesUnstartedDescriptors(t *testing.T) {
	if os.Getenv("GF_APP_RELOAD_CLAIM_CHILD") == "1" {
		if err := ClaimReloadDescriptors(); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(3, unix.F_GETFD, 0); err != nil {
			t.Fatalf("claim closed inherited listener before application initialization: %v", err)
		}
		// Application initialization fails before any Server.Start call.
		CloseUnstartedReloadDescriptors()
		CloseUnstartedReloadDescriptors()
		if _, err := unix.FcntlInt(3, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("failed application retained its unstarted HTTP descriptor: %v", err)
		}
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	manifest, _ := json.Marshal(map[string]listenerFdMap{"application-unstarted": {"http": listener.Addr().String() + "#3"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleApplicationClaimClosesUnstartedDescriptors$", "-test.count=1")
	child.ExtraFiles = []*os.File{file}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GF_SERVER_RELOAD") || key == "GF_SERVER_RESTART" || key == "GPROC_PPID" || key == "GF_APP_RELOAD_CLAIM_CHILD" {
			continue
		}
		child.Env = append(child.Env, item)
	}
	child.Env = append(child.Env, "GF_APP_RELOAD_CLAIM_CHILD=1", adminActionReloadEnvKey+"="+string(manifest))
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("real-exec inherited claim cleanup failed: %v\n%s", err, output)
	}
}

func TestLifecycleApplicationValidatesEntireManifest(t *testing.T) {
	t.Setenv(adminActionReloadEnvKey, "")
	server := applicationReloadTestServer(t, "127.0.0.1:0")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "missing-server", "unstarted-server", "wrong-protocol", "wrong-address"} {
		t.Run(mode, func(t *testing.T) {
			manifest := map[string]listenerFdMap{server.instance: {"http": server.GetListenedAddress() + "#3"}}
			switch mode {
			case "missing-server":
				manifest["application-missing"] = listenerFdMap{"http": "127.0.0.1:1#4"}
			case "unstarted-server":
				unstarted := GetServer("application-unstarted-ready")
				manifest[unstarted.instance] = listenerFdMap{"http": "127.0.0.1:1#4"}
			case "wrong-protocol":
				manifest[server.instance] = listenerFdMap{"https": server.GetListenedAddress() + "#3"}
			case "wrong-address":
				manifest[server.instance] = listenerFdMap{"http": "127.0.0.1:1#3"}
			}
			data, _ := json.Marshal(manifest)
			t.Setenv(adminActionReloadEnvKey, string(data))
			err := ValidateReloadReadiness(ctx)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("manifest %s readiness error=%v", mode, err)
			}
		})
	}
}

func TestLifecycleApplicationAcceptGateHoldsRawAcceptAndCloses(t *testing.T) {
	for _, mode := range []string{"release", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			observed := &applicationAcceptListener{Listener: listener, accepted: make(chan struct{})}
			server := applicationReloadTestServer(t, listener.Addr().String())
			if err := server.SetListener(observed); err != nil {
				t.Fatal(err)
			}
			gateEntered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server.SetAcceptGate(func(_ context.Context, closed <-chan struct{}) error {
				once.Do(func() { close(gateEntered) })
				select {
				case <-closed:
					return net.ErrClosed
				case <-release:
					return nil
				}
			})
			if err := server.Start(); err != nil {
				t.Fatal(err)
			}
			defer server.Shutdown()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := server.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			<-gateEntered
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			response := make(chan error, 1)
			go func() {
				resp, err := client.Get("http://" + listener.Addr().String() + "/health")
				if resp != nil {
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if readErr != nil || string(body) != "application-ready" {
						err = fmt.Errorf("unexpected response %q: %v", body, readErr)
					}
				}
				response <- err
			}()
			select {
			case <-observed.accepted:
				t.Fatal("candidate entered raw Accept before application commit")
			case err := <-response:
				t.Fatalf("candidate answered before application commit: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if mode == "release" {
				close(release)
				if err := <-response; err != nil {
					t.Fatalf("released candidate did not answer: %v", err)
				}
			} else {
				closed := make(chan error, 1)
				go func() { closed <- server.Shutdown() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Shutdown did not interrupt the candidate accept gate")
				}
				if err := <-response; err == nil {
					t.Fatal("stopped uncommitted candidate served a request")
				}
			}
		})
	}
}

func TestLifecycleApplicationOrdinaryHTTPRemainsUngated(t *testing.T) {
	server := applicationReloadTestServer(t, "127.0.0.1:0")
	if server.applicationReload != nil || server.applicationAccept != nil {
		t.Fatal("ordinary server implicitly opted into application reload ownership")
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + server.GetListenedAddress() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "application-ready" {
		t.Fatalf("ordinary HTTP changed response: %q %v", body, err)
	}
}

type applicationAcceptListener struct {
	net.Listener
	once     sync.Once
	accepted chan struct{}
}

func (l *applicationAcceptListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepted) })
	return l.Listener.Accept()
}

func applicationReloadTestServer(t *testing.T, address string) *Server {
	t.Helper()
	server := GetServer(fmt.Sprintf("application-reload-%d", time.Now().UnixNano()))
	config := NewConfig()
	config.Address = address
	config.LogStdout, config.DumpRouterMap = false, false
	config.GracefulTimeout = 1
	config.SessionStorage = gsession.NewStorageMemory()
	if err := server.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	server.BindHandler("/health", func(r *Request) { r.Response.Write("application-ready") })
	return server
}
