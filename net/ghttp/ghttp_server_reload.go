// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
// Licensed under the MIT License. Local lifecycle patch: see LOCAL_PATCH.md.

package ghttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/os/gproc"
)

const reloadStartupTimeout = 30 * time.Second

func marshalReloadManifest(manifest map[string]listenerFdMap) ([]byte, error) {
	return json.Marshal(manifest)
}

// The attempt lock also covers internal/signal callers that bypass the public
// administration lock. An attempt owns every exported file and one Wait call.
var reloadAttemptMu sync.Mutex

type reloadExport struct {
	manifest   map[string]listenerFdMap
	files      []*os.File
	servers    []*Server
	graceDelay time.Duration
}

func (export *reloadExport) closeFiles() {
	for _, file := range export.files {
		_ = file.Close()
	}
	export.files = nil
}

func exportReloadListeners() (*reloadExport, error) {
	export := &reloadExport{manifest: make(map[string]listenerFdMap)}
	// Release the registry lock before taking lifecycle locks: Shutdown removes
	// itself from that registry while holding its lifecycle lock.
	for _, server := range serverMapping.Values() {
		server.lifecycleMu.Lock()
		if !server.lifecycleCounted || server.lifecycleStopped {
			server.lifecycleMu.Unlock()
			continue
		}
		if delay := time.Duration(server.config.GracefulTimeout) * time.Second; delay > export.graceDelay {
			export.graceDelay = delay
		}
		descriptors := listenerFdMap{"http": "", "https": ""}
		var exportErr error
		for _, listener := range server.servers {
			select {
			case <-listener.Ready():
			default:
				exportErr = fmt.Errorf("HTTP server %q is not ready for reload", server.instance)
			}
			if exportErr != nil {
				break
			}
			var file *os.File
			file, exportErr = listener.File()
			if exportErr != nil {
				break
			}
			protocol := "http"
			if listener.IsHttps() {
				protocol = "https"
			}
			if descriptors[protocol] != "" {
				descriptors[protocol] += ","
			}
			descriptors[protocol] += fmt.Sprintf("%s#%d", listener.GetListenedAddress(), 3+len(export.files))
			export.files = append(export.files, file)
		}
		server.lifecycleMu.Unlock()
		if exportErr != nil {
			export.closeFiles()
			return nil, exportErr
		}
		export.manifest[server.instance] = descriptors
		export.servers = append(export.servers, server)
	}
	if len(export.servers) == 0 {
		return nil, errors.New("no running HTTP servers to reload")
	}
	return export, nil
}

func reloadEnvironment(manifest []byte, readyFD int) []string {
	var environment []string
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case adminActionReloadEnvKey, adminActionReloadReadyFDEnvKey, adminActionRestartEnvKey, "GPROC_PPID":
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, adminActionReloadEnvKey+"="+string(manifest), fmt.Sprintf("%s=%d", adminActionReloadReadyFDEnvKey, readyFD))
}

func forkReloadProcess(ctx context.Context, newExeFilePath ...string) error {
	reloadAttemptMu.Lock()
	defer reloadAttemptMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	binaryPath := os.Args[0]
	if len(newExeFilePath) > 0 && newExeFilePath[0] != "" {
		binaryPath = newExeFilePath[0]
	}
	export, err := exportReloadListeners()
	if err != nil {
		return err
	}
	defer export.closeFiles()
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()
	buffer, err := json.Marshal(export.manifest)
	if err != nil {
		return err
	}
	process := gproc.NewProcess(binaryPath, os.Args[1:])
	// NewProcess appends environment to os.Environ; replace Env rather than
	// inheriting old ready-fd/reload variables from the preceding generation.
	process.Env = reloadEnvironment(buffer, 3+len(export.files))
	process.ExtraFiles = append(append([]*os.File(nil), export.files...), writer)
	if _, err = process.Start(ctx); err != nil {
		return fmt.Errorf("start HTTP reload candidate: %w", err)
	}
	// The child has its own inherited copies now. Keep none in the parent.
	export.closeFiles()
	_ = writer.Close()
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	committed := false
	defer func() {
		if !committed {
			_ = process.Process.Kill()
			<-exited
		}
	}()
	type readiness struct {
		Ready bool   `json:"ready"`
		Error string `json:"error"`
	}
	result := make(chan error, 1)
	decoderDone := make(chan struct{})
	go func() {
		defer close(decoderDone)
		var frame readiness
		err := json.NewDecoder(io.LimitReader(reader, 8192)).Decode(&frame)
		if err == nil && (!frame.Ready || frame.Error != "") {
			err = fmt.Errorf("HTTP reload candidate failed: %s", frame.Error)
		}
		result <- err
	}()
	defer func() {
		_ = reader.Close()
		<-decoderDone
	}()
	startupCtx, cancel := context.WithTimeout(ctx, reloadStartupTimeout+export.graceDelay)
	defer cancel()
	select {
	case <-startupCtx.Done():
		return fmt.Errorf("wait for HTTP reload candidate: %w", startupCtx.Err())
	case err = <-exited:
		exited <- err
		return fmt.Errorf("HTTP reload candidate exited during startup: %v", err)
	case err = <-result:
		if err != nil {
			return fmt.Errorf("read HTTP reload readiness: %w", err)
		}
	}
	// Reject a candidate that exited or a cancellation already observed before
	// commit. After commit the request context no longer owns the new process.
	if err = startupCtx.Err(); err != nil {
		return fmt.Errorf("HTTP reload canceled before handoff: %w", err)
	}
	select {
	case err = <-exited:
		exited <- err // The rollback owner performs the single receive.
		return fmt.Errorf("HTTP reload candidate exited before handoff: %v", err)
	default:
	}
	// Commit and a concurrent parent Shutdown must have one ordering. Acquire
	// the complete snapshot in its stable export order; no other path holds
	// more than one lifecycle lock. Recheck cancellation after those locks.
	for _, server := range export.servers {
		server.lifecycleMu.Lock()
	}
	commitErr := startupCtx.Err()
	for _, server := range export.servers {
		if server.lifecycleStopped || !server.lifecycleCounted || server.Err() != nil {
			commitErr = fmt.Errorf("HTTP reload parent %q stopped before handoff: %v", server.instance, server.Err())
			break
		}
	}
	if commitErr == nil {
		select {
		case err = <-exited:
			exited <- err
			commitErr = fmt.Errorf("HTTP reload candidate exited before handoff: %v", err)
		default:
		}
	}
	if commitErr == nil {
		committed = true
	}
	for index := len(export.servers) - 1; index >= 0; index-- {
		export.servers[index].lifecycleMu.Unlock()
	}
	if commitErr != nil {
		return fmt.Errorf("commit HTTP reload: %w", commitErr)
	}
	// A successful candidate remains a child until the old process exits; its
	// unique waiter reaps it if it ends sooner. Do not synchronously drain the
	// server here: RestartAllServer may itself be running in an HTTP handler.
	go func() {
		for _, server := range export.servers {
			_ = server.Shutdown()
		}
	}()
	return nil
}
