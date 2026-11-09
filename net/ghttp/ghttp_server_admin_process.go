// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
//
// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT was not distributed with this file,
// You can obtain one at https://github.com/gogf/gf.

package ghttp

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gogf/gf/v2/container/gtype"
	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gcode"
	"github.com/gogf/gf/v2/errors/gerror"
	"github.com/gogf/gf/v2/internal/intlog"
	"github.com/gogf/gf/v2/os/glog"
	"github.com/gogf/gf/v2/os/gproc"
	"github.com/gogf/gf/v2/os/gtime"
	"github.com/gogf/gf/v2/os/gtimer"
)

const (
	// Allow executing management command after server starts after this interval in milliseconds.
	adminActionIntervalLimit = 2000
	adminActionNone          = 0
	adminActionRestarting    = 1
	adminActionShuttingDown  = 2
	adminActionReloadEnvKey  = "GF_SERVER_RELOAD"
	adminActionRestartEnvKey = "GF_SERVER_RESTART"
	adminGProcCommGroup      = "GF_GPROC_HTTP_SERVER"
)

var (
	// serverActionLocker is the locker for server administration operations.
	serverActionLocker sync.Mutex

	// serverActionLastTime is timestamp in milliseconds of last administration operation.
	serverActionLastTime = gtype.NewInt64(gtime.TimestampMilli())

	// serverProcessStatus is the server status for operation of current process.
	serverProcessStatus = gtype.NewInt()
)

// RestartAllServer restarts all the servers of the process gracefully.
// The optional parameter `newExeFilePath` specifies the new binary file for creating process.
func RestartAllServer(ctx context.Context, newExeFilePath string) error {
	if !gracefulEnabled {
		return gerror.NewCode(
			gcode.CodeInvalidOperation,
			"graceful reload feature is disabled",
		)
	}
	serverActionLocker.Lock()
	defer serverActionLocker.Unlock()
	if err := checkProcessStatus(); err != nil {
		return err
	}
	if err := checkActionFrequency(); err != nil {
		return err
	}
	return restartWebServers(ctx, nil, newExeFilePath)
}

// ShutdownAllServer shuts down all servers of current process gracefully.
func ShutdownAllServer(ctx context.Context) error {
	serverActionLocker.Lock()
	defer serverActionLocker.Unlock()
	if err := checkProcessStatus(); err != nil {
		return err
	}
	if err := checkActionFrequency(); err != nil {
		return err
	}
	shutdownWebServersGracefully(ctx, nil)
	return nil
}

// checkProcessStatus checks the server status of current process.
func checkProcessStatus() error {
	status := serverProcessStatus.Val()
	if status > 0 {
		switch status {
		case adminActionRestarting:
			return gerror.NewCode(gcode.CodeInvalidOperation, "server is restarting")

		case adminActionShuttingDown:
			return gerror.NewCode(gcode.CodeInvalidOperation, "server is shutting down")
		}
	}
	return nil
}

// checkActionFrequency checks the operation frequency.
// It returns error if it is too frequency.
func checkActionFrequency() error {
	interval := gtime.TimestampMilli() - serverActionLastTime.Val()
	if interval < adminActionIntervalLimit {
		return gerror.NewCodef(
			gcode.CodeInvalidOperation,
			"too frequent action, please retry in %d ms",
			adminActionIntervalLimit-interval,
		)
	}
	serverActionLastTime.Set(gtime.TimestampMilli())
	return nil
}

// forkRestartProcess creates a new server process.
func forkRestartProcess(ctx context.Context, newExeFilePath ...string) error {
	var (
		path = os.Args[0]
	)
	if len(newExeFilePath) > 0 && newExeFilePath[0] != "" {
		path = newExeFilePath[0]
	}
	if err := os.Unsetenv(adminActionReloadEnvKey); err != nil {
		intlog.Errorf(ctx, `%+v`, err)
	}
	env := os.Environ()
	env = append(env, adminActionRestartEnvKey+"=1")
	p := gproc.NewProcess(path, os.Args[1:], env)
	if _, err := p.Start(ctx); err != nil {
		glog.Errorf(
			ctx,
			`%d: fork process failed, error:%s, are you running using "go run"?`,
			gproc.Pid(), err.Error(),
		)
		return err
	}
	return nil
}

// bufferToServerFdMap converts binary content to fd map.
func bufferToServerFdMap(buffer []byte) map[string]listenerFdMap {
	sfm := make(map[string]listenerFdMap)
	if len(buffer) > 0 {
		j, _ := gjson.LoadContent(buffer)
		for k := range j.Var().Map() {
			m := make(map[string]string)
			for mapKey, mapValue := range j.Get(k).MapStrStr() {
				m[mapKey] = mapValue
			}
			sfm[k] = m
		}
	}
	return sfm
}

// restartWebServers restarts all servers.
func restartWebServers(ctx context.Context, signal os.Signal, newExeFilePath string) error {
	serverProcessStatus.Set(adminActionRestarting)
	for _, server := range serverMapping.Values() {
		if server.applicationReload != nil {
			if err := server.applicationReload(ctx, newExeFilePath); err != nil {
				serverProcessStatus.Set(adminActionNone)
				return err
			}
			return nil
		}
	}
	if runtime.GOOS == "windows" {
		if signal != nil {
			// Controlled by signal.
			forceCloseWebServers(ctx)
			if err := forkRestartProcess(ctx, newExeFilePath); err != nil {
				intlog.Errorf(ctx, `%+v`, err)
			}
			return nil
		}
		// Controlled by web page.
		// It should ensure the response wrote to client and then close all servers gracefully.
		gtimer.SetTimeout(ctx, time.Second, func(ctx context.Context) {
			forceCloseWebServers(ctx)
			if err := forkRestartProcess(ctx, newExeFilePath); err != nil {
				intlog.Errorf(ctx, `%+v`, err)
			}
		})
		return nil
	}
	if err := forkReloadProcess(ctx, newExeFilePath); err != nil {
		glog.Printf(ctx, "%d: server restarts failed", gproc.Pid())
		serverProcessStatus.Set(adminActionNone)
		return err
	} else {
		if signal != nil {
			glog.Printf(ctx, "%d: server restarting by signal: %s", gproc.Pid(), signal)
		} else {
			glog.Printf(ctx, "%d: server restarting by web admin", gproc.Pid())
		}
	}

	return nil
}

// shutdownWebServersGracefully gracefully shuts down all servers.
func shutdownWebServersGracefully(ctx context.Context, signal os.Signal) {
	serverProcessStatus.Set(adminActionShuttingDown)
	if signal != nil {
		glog.Printf(
			ctx,
			"%d: server gracefully shutting down by signal: %s",
			gproc.Pid(), signal.String(),
		)
	} else {
		glog.Printf(ctx, "pid[%d]: server gracefully shutting down by api", gproc.Pid())
	}
	// Never hold the registry map lock while joining Serve: terminal cleanup
	// removes the owner from that same map.
	for _, server := range serverMapping.Values() {
		_ = server.Shutdown()
	}
}

// forceCloseWebServers forced shuts down all servers.
func forceCloseWebServers(ctx context.Context) {
	for _, server := range serverMapping.Values() {
		server.lifecycleMu.Lock()
		for _, listener := range server.servers {
			listener.Close(ctx)
		}
		server.lifecycleMu.Unlock()
		_ = server.Shutdown()
	}
}
