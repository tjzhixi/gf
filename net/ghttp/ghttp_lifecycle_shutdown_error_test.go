package ghttp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
)

// A real listener that closes successfully but reports a shutdown failure makes
// the lifecycle owner prove it preserves cleanup errors while closing sockets.
type shutdownErrorListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (l *shutdownErrorListener) Close() error {
	l.once.Do(func() { _ = l.Listener.Close() })
	return l.err
}

func TestLifecycleShutdownPreservesListenerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cause := errors.New("fixture listener shutdown failure")
	wrapped := &shutdownErrorListener{Listener: listener, err: cause}
	s := ghttp.GetServer(fmt.Sprintf("shutdown-error-%d", time.Now().UnixNano()))
	s.SetDumpRouterMap(false)
	s.SetAccessLogEnabled(false)
	s.SetErrorLogEnabled(false)
	if err := s.SetConfigWithMap(map[string]any{"SessionPath": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetListener(wrapped); err != nil {
		t.Fatal(err)
	}
	s.BindHandler("/health", func(r *ghttp.Request) { r.Response.Write("ready") })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	err = s.Shutdown()
	if !errors.Is(err, cause) {
		t.Errorf("shutdown lost listener cleanup failure: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("completed request reported timeout: %v", err)
	}
	if !errors.Is(s.Err(), cause) {
		t.Errorf("final Err lost cleanup failure: %v", s.Err())
	}
	select {
	case <-s.Done():
	default:
		t.Error("shutdown did not publish complete cleanup")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("failed shutdown left listener open")
	}
}
