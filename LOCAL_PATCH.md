# GoFrame 2.10.0 HTTP 生命周期补丁

本仓库完整保留 `github.com/gogf/gf/v2 v2.10.0` 发布模块的源码和 MIT LICENSE，通过应用仓库根 `go.mod` 的 `replace github.com/gogf/gf/v2 => github.com/tjzhixi/gf/v2 v2.10.0-zhixi.2` 使用（模块声明路径仍保持上游原值）。上游模块校验值：

```text
github.com/gogf/gf/v2 v2.10.0 h1:rzDROlyqGMe/eM6dCalSR8dZOuMIdLhmxKSH1DGhbFs=
github.com/gogf/gf/v2 v2.10.0/go.mod h1:Svl1N+E8G/QshU2DUbh/3J/AJauqCgUnxHurXWR4Qx0=
```

应用需要把 HTTP 启动失败交给统一命令协调器。上游在端口绑定、Serve、插件、注册中心等失败时调用 Fatal，直接退出进程；多监听 Start 的创建屏障先于 Serve 状态发布，Shutdown 又跳过尚未 Running 的监听，启动取消时可能留下端口。外层 recover 或汇总 Status 轮询无法修复这两个问题。

## 改动

- `net/ghttp/ghttp.go`、`ghttp_server.go`、新增 `ghttp_server_lifecycle.go`：同一实例的 Start/Shutdown 串行；一次性接管资源；同步创建所有监听，失败完整回滚；全部初始化成功后才启动 Serve。独立协调协程在任一 Serve 结束时关闭所有监听并等待每个 Serve。新增 `Done()` 和 `Err()`，供应用观察完整终态及错误，移除运行中的 Fatal。停止的实例不能重新 Start，重建时获取新实例。
- `ghttp_server_admin.go`、`ghttp_server_admin_process.go`：重复关闭等待同一次清理；按实例快照遍历，避免持全局注册表锁等待 Serve。Run 启动失败记录错误并返回，应用仍只用 Start/Shutdown。
- `ghttp_server_graceful.go`、`internal/graceful/graceful.go`：保留原地址、TLS、配置字段和继承 fd 构造；Shutdown 即使发生在 Serve 前也发布标准库关闭终态并关闭原始监听。继承 fd 在 session/plugin 初始化前即被接管，消费时与未消费时共用同一 `*os.File`，避免泄漏和重复关闭已复用的 fd。
- `ghttp_server_registry.go`、`ghttp_server_openapi.go`：初始化错误返回，监听一起回滚；失败注册保留待清理的 service。
- `ghttp_server_config.go`、`ghttp_server_config_static.go`、`ghttp_server_router.go`、`ghttp_server_router_group.go`、`ghttp_server_service_handler.go`、`ghttp_server_service_object.go`：同步 void 配置/注册方法的 Fatal 改为可恢复的 Panic；SetConfig/SetConfigWithMap 返回 error，Start 恢复初始化 panic 并清理资源。合法配置、路由和 middleware 规则不变。
- 新增 `ghttp_server_reload.go`、`ghttp_server_reload_child.go`：Unix 原生热重载采用每次尝试独立的继承就绪管道，替代每个 Server 向父进程发送通用 `exit` 消息。子进程必须成功初始化清单内全部 Server，逐个 HTTP/HTTPS 监听进入 Accept，并核对协议和实际地址，才开始原配置的最大 GracefulTimeout 延迟。父进程等待完整就绪后才异步关闭原 Server，让触发重载的 HTTP 请求和已有请求正常完成；普通 gproc 子进程不能触发这条退出路径。
- `internal/graceful/descriptor_unix.go`、`descriptor_windows.go`：导出时直接复制原非阻塞 fd，并保留明确的 os.File 所有权；不使用会在 ExtraFiles 取 Fd 时改变原监听阻塞状态的 TCPListener.File。启动成功或失败均关闭父导出副本；不支持导出的自定义 listener 返回错误而不 panic。每次候选只有一个 Wait 所有者，启动失败、提前退出、取消或就绪超时都杀死并回收候选，原服务保留；成功后请求 context 不再拥有新进程。
- `ghttp_reload_fd_unix.go`、`ghttp_reload_fd_windows.go`：候选初始化最前面为完整 manifest 的监听及就绪管道恢复 CLOEXEC，防止插件启动外部程序时再传给孙进程。成功回报前清除已消费的两条 reload 环境变量，同进程新实例不会重新包装旧数字 fd；下一代重载仍从当前监听重新导出。
- `ghttp_server_reload_application.go`：提供应用自有的 reload handler、监听导出快照、完整继承清单就绪核查与提前 fd 接管/失败清理。`SetAcceptGate` 在底层 raw Accept 之前等待应用提交，Serve 的就绪信号先发布；Shutdown 能中断未激活 gate。未配置这些接口的调用方沿用原生 HTTP 交接路径。真实 `http/all` 命令通过它协调 TCP、租户控制面和后台任务，不把 HTTP ready 误当作整组 ready。

补丁改变 `net/ghttp` 的上述 14 个上游生产文件，并新增生命周期、热重载及平台描述符实现和对应回归文件。其他上游源码保持原样；本说明和 `.gitignore` 中的 fixture 例外属于打包维护信息。

## 使用约束

- 配置和路由在 Start 前设置。Status、GetListenedPorts 等观察 API 在 Start 返回后使用；本补丁没有将所有配置 API 改为并发安全。
- Plugin.Install/Remove 和 Registrar 回调不得同步重入同一实例的 Start/Shutdown，也不得无限阻塞。应用当前没有注册这类插件。并发调用 Shutdown 安全。
- 保留原 GracefulTimeout 和底层超时映射（每监听沿用原顺序和预算）。Shutdown 正常等待在途请求；超时或关闭失败后调用 net/http.Close 强制关闭剩余 StateNew/Active/Idle 连接，再返回带协议/监听地址的错误，保留 errors.Is(context.DeadlineExceeded) 和强关失败。上层收集每个监听错误，继续关闭其余监听并等待全部 Serve；重复/并发 Shutdown 和 Err 共享最终结果。
- 强关会断开 HTTP 连接，但不会强杀忽略取消的业务 goroutine。平台中间件当前可能使用 GetNeverDoneCtx，业务是否结束仍取决于自身逻辑。Done 表示连接、Serve 和生命周期清理结束，不承诺所有业务 handler 已退出。
- hijacked WebSocket 仍由浏览器/设备传输 owner 关闭，不能用 HTTP Done 代替。
- 保留上游 HTTP/HTTPS、多地址、证书文件、TLSConfig、自定义监听及继承 fd 解析。Unix 原生热重载覆盖真实多进程 HTTP/HTTPS 交接、回滚、重试、连续两代重载及原生管理员 HTTP 重载响应。候选就绪等待上限为 30 秒加本批 Server 的最大原 GracefulTimeout，调用方更早取消或超时优先生效；原管理操作两秒频率限制保持。
- 原生交接的就绪范围是导出清单中的 GoFrame HTTP Server，其他外部资源仍由各应用 owner 管理；没有把应用命令改回 GoFrame Run/Wait，也没有新增信号监听。Windows 原生重启和强制关闭对在途生命周期回调的抢占未验收，独立记录。
- 应用 override 的四个服务阶段为 Prepared、Active、Finalized、Committed，通过独立双向管道与 `internal/cmd` owner 交接。事务 seal 之前的失败可以回滚并恢复父服务；seal 之后的错误或确认丢失不证明数据库已回滚，必须停止父业务并保留错误，不能自动恢复旧消费者。它不提供跨系统原子事务。旧在途连接仍按各 transport 的原关闭语义收尾，TCP/WebSocket 已接受连接不跨进程迁移。

## 验证与升级

应用回归位于应用仓库的 `internal/cmd/http_lifecycle_test.go`、`http_startup_failure_test.go`、`http_config_lifecycle_test.go`、`http_shutdown_timeout_test.go` 和 `http_shutdown_coordinator_test.go`。Shutdown 回归覆盖正常 HTTP/HTTPS 在途请求、超时活动/未完成头部/TLS 连接、忽略取消的 handler、多个监听分别超时和错误累积、并发关闭与命令错误传播。本仓库的补丁回归在仓库根目录直接执行：

```sh
go test -race ./net/ghttp -run '^TestLifecycle' -count=3
```

原生热重载回归另见 `ghttp_lifecycle_reload_unix_test.go`、`ghttp_lifecycle_reload_export_test.go` 和 `ghttp_lifecycle_reload_child_scope_unix_test.go`：真实父/子/孙进程验证交接、失败回滚、取消、失败重试、连续两代、管理员 HTTP 重载、CLOEXEC 与已消费环境清理。RED/GREEN、最终整套 race 和根工程验收证据归档在 `.scratch/http-native-reload/verification.md`。

`ghttp_lifecycle_reload_application_unix_test.go` 另验完整 manifest、应用尚未启动时 fd 回收、Accept 前门禁及关闭。真实命令和独立 MySQL/Redis/Disk 业务装配验收见 `.scratch/command-native-reload/verification.md`，应用协调不新增公开 HTTP 路由或信号 owner。

运行上游 `g.Server()` 测试时应使用独立 GF_GCFG_PATH，避免继承业务工程的静态目录配置。此次上游 HTTPS 测试首次读取业务 SPA，导致预期 404 的三个测试失败；换用空静态目录的独立配置后全部通过。

升级 GoFrame 时，先将本仓库与指定版本原始发布模块比较，再逐条移植上述生命周期差异；不要取消应用仓库的 replace，也不要直接用上游版本覆盖本仓库。升级后运行应用仓库 test/race/build/vet 和本仓库配置/继承 fd 回归。若上游已修复，可在等价回归通过后删除本地补丁并还原为上游模块。应用构建通过 go module 代理拉取本仓库，无需在应用仓库内保留源码副本；本仓库的维护不影响用户机器的 Go 模块缓存。
