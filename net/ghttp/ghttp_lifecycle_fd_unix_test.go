//go:build !windows

package ghttp_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
)

// net.FileListener duplicates its descriptor. Both that duplicate and the
// inherited descriptor must be closed by rollback and normal shutdown.
func TestLifecycleInheritedFDCloses(t *testing.T) {
	if mode := os.Getenv("GF_LIFECYCLE_FD_CHILD"); mode != "" {
		testLifecycleInheritedFDChild(t, mode, os.Getenv("GF_LIFECYCLE_FD_ADDRESS"))
		return
	}
	for _, protocol := range []string{"http", "https-address-fallback"} {
		for _, operation := range []string{"shutdown", "later-bind-failure", "earlier-bind-failure", "invalid-tls", "plugin-failure", "session-failure"} {
			if protocol == "http" && operation == "invalid-tls" {
				continue
			}
			t.Run(protocol+"/"+operation, func(t *testing.T) {
				listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
				if err != nil {
					t.Fatal(err)
				}
				file, err := listener.File()
				if err != nil {
					listener.Close()
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleInheritedFDCloses$")
				child.ExtraFiles = []*os.File{file}
				child.Env = append(os.Environ(), "GF_LIFECYCLE_FD_CHILD="+protocol+"/"+operation,
					"GF_LIFECYCLE_FD_ADDRESS="+listener.Addr().String())
				var output strings.Builder
				child.Stdout, child.Stderr = &output, &output
				err = child.Start()
				// Only the child owns the inherited listener from this point.
				_ = file.Close()
				_ = listener.Close()
				if err != nil {
					t.Fatal(err)
				}
				if err = child.Wait(); err != nil {
					t.Fatalf("inherited fd lifecycle: %v\n%s", err, output.String())
				}
			})
		}
	}
}

func testLifecycleInheritedFDChild(t *testing.T, mode, address string) {
	s := ghttp.GetServer("lifecycle-fd-child")
	config := ghttp.NewConfig()
	config.Address = fmt.Sprintf("%s#3", address)
	config.SessionPath = t.TempDir()
	if strings.HasSuffix(mode, "/session-failure") {
		config.SessionPath += "/not-a-directory"
		if err := os.WriteFile(config.SessionPath, []byte("fixture file"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config.LogStdout = false
	config.DumpRouterMap = false
	if strings.HasPrefix(mode, "https") {
		certificate, err := tls.LoadX509KeyPair("testdata/https/files/server.crt", "testdata/https/files/server.key")
		if err != nil {
			t.Fatal(err)
		}
		config.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}}
	}
	failStartup := !strings.HasSuffix(mode, "/shutdown")
	if strings.HasSuffix(mode, "/invalid-tls") {
		config.TLSConfig = nil
		// Existing files pass configuration validation; Start must reject their contents.
		config.HTTPSCertPath = t.TempDir() + "/cert.pem"
		config.HTTPSKeyPath = t.TempDir() + "/key.pem"
		if err := os.WriteFile(config.HTTPSCertPath, []byte("invalid certificate"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.HTTPSKeyPath, []byte("invalid key"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasSuffix(mode, "bind-failure") {
		blocked, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blocked.Close()
		if strings.HasSuffix(mode, "/earlier-bind-failure") {
			config.Address = blocked.Addr().String() + "," + config.Address
		} else {
			config.Address += "," + blocked.Addr().String()
		}
	}
	if err := s.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(mode, "/plugin-failure") {
		s.Plugin(inheritedFDFailingPlugin{})
	}
	s.BindHandler("/health", func(r *ghttp.Request) { r.Response.Write("ready") })
	err := s.Start()
	if failStartup {
		if err == nil {
			_ = s.Shutdown()
			t.Fatal("occupied second address must fail startup")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}
	connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Errorf("inherited listener %s still accepts after %s", address, mode)
	}
}

type inheritedFDFailingPlugin struct{}

func (inheritedFDFailingPlugin) Name() string        { return "inherited-fd-failure" }
func (inheritedFDFailingPlugin) Author() string      { return "test" }
func (inheritedFDFailingPlugin) Version() string     { return "1" }
func (inheritedFDFailingPlugin) Description() string { return "Fail before listener construction" }
func (inheritedFDFailingPlugin) Install(*ghttp.Server) error {
	return errors.New("fixture plugin failure")
}
func (inheritedFDFailingPlugin) Remove() error { return nil }
