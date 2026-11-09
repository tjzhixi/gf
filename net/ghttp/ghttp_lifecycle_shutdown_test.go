package ghttp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gsession"
)

// Reading a partial request/handshake proves the accepted connection was
// registered as StateNew before Shutdown. Closing only the listener leaks it.
func TestLifecycleShutdownTimeoutClosesUnfinishedConnection(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			s, listener := shutdownFixture(t, useTLS)
			readStarted := make(chan struct{})
			if err := s.SetListener(&shutdownObservedListener{Listener: listener, readStarted: readStarted}); err != nil {
				t.Fatal(err)
			}
			s.BindHandler("/", func(r *ghttp.Request) { r.Response.Write("ready") })
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			peer, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			partial := []byte("GET / HTTP/1.1\r\n")
			if useTLS {
				partial = []byte{0x16} // An unfinished TLS record header.
			}
			if _, err := peer.Write(partial); err != nil {
				t.Fatal(err)
			}
			shutdownWaitSignal(t, readStarted, "server to read the incomplete request")
			started := time.Now()
			err = s.Shutdown()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("expected shutdown deadline error, got %v", err)
			}
			if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
				t.Errorf("existing one-second shutdown budget changed: %v", elapsed)
			}
			_ = peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, err = peer.Read(make([]byte, 1))
			var networkError net.Error
			if err == nil || (errors.As(err, &networkError) && networkError.Timeout()) {
				t.Errorf("unfinished connection survived shutdown: %v", err)
			}
			shutdownAssertListenerClosed(t, listener)
		})
	}
}

// The owner must close the connection and return its timeout error even if a
// handler ignores cancellation. Waiting for such a handler would hang shutdown.
func TestLifecycleShutdownTimeoutDoesNotWaitForIgnoringHandler(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			s, listener := shutdownFixture(t, useTLS)
			entered, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer func() {
				releaseOnce.Do(func() { close(release) })
				select {
				case <-handlerDone:
				case <-time.After(time.Second):
					t.Error("test handler did not exit after release")
				}
			}()
			s.BindHandler("/slow", func(r *ghttp.Request) {
				defer close(handlerDone)
				close(entered)
				<-release
				r.Response.Write("finished too late")
			})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			clientResult := shutdownRequest(listener, useTLS, "/slow")
			shutdownWaitSignal(t, entered, "handler to start")
			shutdownResult := make(chan error, 1)
			go func() { shutdownResult <- s.Shutdown() }()
			select {
			case err := <-shutdownResult:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("expected shutdown deadline error, got %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("Shutdown waited indefinitely for a handler that ignores cancellation")
				releaseOnce.Do(func() { close(release) })
				<-shutdownResult
			}
			select {
			case result := <-clientResult:
				if result.err == nil {
					t.Errorf("timed-out request unexpectedly completed: %q", result.body)
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("active client connection remained open after shutdown returned")
				releaseOnce.Do(func() { close(release) })
				<-clientResult
			}
			shutdownAssertListenerClosed(t, listener)
		})
	}
}

func TestLifecycleShutdownHTTPSStillCompletesNormalRequest(t *testing.T) {
	s, listener := shutdownFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s.BindHandler("/slow", func(r *ghttp.Request) {
		close(entered)
		<-release
		r.Response.Write("gracefully completed")
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	clientResult := shutdownRequest(listener, true, "/slow")
	shutdownWaitSignal(t, entered, "HTTPS handler to start")
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- s.Shutdown() }()
	select {
	case err := <-shutdownResult:
		t.Fatalf("shutdown interrupted a normal in-flight request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-clientResult:
		if result.err != nil || result.body != "gracefully completed" {
			t.Errorf("normal request was not preserved: body=%q error=%v", result.body, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("normal request did not finish")
	}
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("graceful shutdown did not finish")
	}
	shutdownAssertListenerClosed(t, listener)
}

func shutdownFixture(t *testing.T, useTLS bool) (*ghttp.Server, net.Listener) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	s := ghttp.GetServer(fmt.Sprintf("shutdown-timeout-%s-%d", t.Name(), time.Now().UnixNano()))
	config := ghttp.NewConfig()
	config.SessionStorage = gsession.NewStorageMemory()
	config.DumpRouterMap, config.LogStdout = false, false
	config.ErrorLogEnabled, config.AccessLogEnabled = false, false
	config.GracefulTimeout = 1
	config.GracefulShutdownTimeout = 7 // Preserve the existing effective field mapping.
	if useTLS {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		config.TLSConfig = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	}
	if err := s.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := s.SetListener(listener); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	return s, listener
}

type shutdownRequestResult struct {
	body string
	err  error
}

func shutdownRequest(listener net.Listener, useTLS bool, path string) <-chan shutdownRequestResult {
	result := make(chan shutdownRequestResult, 1)
	go func() {
		transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // Only task-generated loopback certificate.
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		protocol := "http"
		if useTLS {
			protocol = "https"
		}
		response, err := client.Get(protocol + "://" + listener.Addr().String() + path)
		if err != nil {
			result <- shutdownRequestResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		result <- shutdownRequestResult{body: string(body), err: err}
	}()
	return result
}

func shutdownWaitSignal(t *testing.T, signal <-chan struct{}, action string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", action)
	}
}

func shutdownAssertListenerClosed(t *testing.T, listener net.Listener) {
	t.Helper()
	peer, err := net.DialTimeout("tcp", listener.Addr().String(), 200*time.Millisecond)
	if err == nil {
		_ = peer.Close()
		t.Errorf("listener %s still accepts after shutdown", listener.Addr())
	}
}

type shutdownObservedListener struct {
	net.Listener
	readStarted chan struct{}
}

func (l *shutdownObservedListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &shutdownObservedConn{Conn: connection, readStarted: l.readStarted}, nil
}

type shutdownObservedConn struct {
	net.Conn
	readStarted chan struct{}
	once        sync.Once
}

func (c *shutdownObservedConn) Read(buffer []byte) (int, error) {
	c.once.Do(func() { close(c.readStarted) })
	return c.Conn.Read(buffer)
}
