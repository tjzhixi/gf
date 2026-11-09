package ghttp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
)

func TestLifecycleConfigurationReturnsMissingTLSFile(t *testing.T) {
	for _, useMap := range []bool{false, true} {
		t.Run(fmt.Sprintf("map=%t", useMap), func(t *testing.T) {
			s := ghttp.GetServer(fmt.Sprintf("config-tls-%d", time.Now().UnixNano()))
			s.SetLogStdout(false)
			missing := filepath.Join(t.TempDir(), "missing-cert.pem")
			var err error
			if useMap {
				err = s.SetConfigWithMap(map[string]any{"HTTPSCertPath": missing, "HTTPSKeyPath": missing})
			} else {
				config := ghttp.NewConfig()
				config.HTTPSCertPath, config.HTTPSKeyPath = missing, missing
				config.LogStdout = false
				err = s.SetConfig(config)
			}
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing certificate must return configuration error, got %v", err)
			}
		})
	}
}

func TestLifecycleConfigurationReturnsMissingTLSKey(t *testing.T) {
	s := ghttp.GetServer(fmt.Sprintf("config-key-%d", time.Now().UnixNano()))
	s.SetLogStdout(false)
	directory := t.TempDir()
	certificate := filepath.Join(directory, "certificate.pem")
	if err := os.WriteFile(certificate, []byte("resolved during configuration; parsed during Start"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(directory, "missing-key.pem")
	err := s.SetConfigWithMap(map[string]any{"HTTPSCertPath": certificate, "HTTPSKeyPath": missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing key must return configuration error, got %v", err)
	}
}

func TestLifecycleConfigurationReturnsMissingStaticRoot(t *testing.T) {
	s := ghttp.GetServer(fmt.Sprintf("config-static-%d", time.Now().UnixNano()))
	s.SetLogStdout(false)
	missing := filepath.Join(t.TempDir(), "missing-static-root")
	err := s.SetConfigWithMap(map[string]any{"ServerRoot": missing})
	if err == nil || !strings.Contains(err.Error(), "SetServerRoot failed") {
		t.Fatalf("missing static root must return configuration error, got %v", err)
	}
}

func TestLifecycleVoidTLSConfigurationCanUnwind(t *testing.T) {
	s := ghttp.GetServer(fmt.Sprintf("config-void-%d", time.Now().UnixNano()))
	s.SetLogStdout(false)
	missing := filepath.Join(t.TempDir(), "missing-cert.pem")
	unwound := false
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		defer func() { unwound = true }()
		s.EnableHTTPS(missing, missing)
	}()
	if !unwound || recovered == nil || !strings.Contains(fmt.Sprint(recovered), missing) {
		t.Fatalf("void setter must unwind to command owner: unwound=%t panic=%v", unwound, recovered)
	}
}
