> **补丁 fork 说明**：本仓库是 [gogf/gf](https://github.com/gogf/gf) 上游 `v2.10.0` 的维护版 fork，对 `net/ghttp` 和 `internal/graceful` 做了本地补丁（HTTP 生命周期、关闭错误传播、Unix 原生平滑热重载），由 `wechat-bot` 项目通过 `go.mod` 的 `replace` 指令引用。补丁范围、使用约束和回归测试见 [LOCAL_PATCH.md](LOCAL_PATCH.md)。上游采用 MIT 协议，原始 [LICENSE](LICENSE) 保留。以下为上游 README 原文。


[English](README.MD) | 简体中文

<div align=center>
<img src="https://goframe.org/img/logo_full.png" width="300" alt="goframe gf logo"/>

[![Go Reference](https://pkg.go.dev/badge/github.com/gogf/gf/v2.svg)](https://pkg.go.dev/github.com/gogf/gf/v2)
[![GoFrame CI](https://github.com/gogf/gf/actions/workflows/ci-main.yml/badge.svg)](https://github.com/gogf/gf/actions/workflows/ci-main.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/gogf/gf/badge)](https://scorecard.dev/viewer/?uri=github.com/gogf/gf)
[![CII Best Practices](https://bestpractices.coreinfrastructure.org/projects/9233/badge)](https://bestpractices.coreinfrastructure.org/projects/9233)
[![Go Report Card](https://goreportcard.com/badge/github.com/gogf/gf/v2)](https://goreportcard.com/report/github.com/gogf/gf/v2)
[![Code Coverage](https://codecov.io/gh/gogf/gf/branch/master/graph/badge.svg)](https://codecov.io/gh/gogf/gf)
[![Production Ready](https://img.shields.io/badge/production-ready-blue.svg?style=flat)](https://github.com/gogf/gf)
[![License](https://img.shields.io/github/license/gogf/gf.svg?style=flat)](https://github.com/gogf/gf)

[![Release](https://img.shields.io/github/v/release/gogf/gf?style=flat)](https://github.com/gogf/gf/releases)
[![GitHub pull requests](https://img.shields.io/github/issues-pr/gogf/gf?style=flat)](https://github.com/gogf/gf/pulls)
[![GitHub closed pull requests](https://img.shields.io/github/issues-pr-closed/gogf/gf?style=flat)](https://github.com/gogf/gf/pulls?q=is%3Apr+is%3Aclosed)
[![GitHub issues](https://img.shields.io/github/issues/gogf/gf?style=flat)](https://github.com/gogf/gf/issues)
[![GitHub closed issues](https://img.shields.io/github/issues-closed/gogf/gf?style=flat)](https://github.com/gogf/gf/issues?q=is%3Aissue+is%3Aclosed)
![Stars](https://img.shields.io/github/stars/gogf/gf?style=flat)
![Forks](https://img.shields.io/github/forks/gogf/gf?style=flat)

</div>

一个强大的框架，为了更快、更轻松、更高效的项目开发。

## 安装

```bash
go get -u github.com/gogf/gf/v2
```

## 文档

- 官方网站: [https://goframe.org](https://goframe.org)
- 官方网站(en): [https://goframe.org/en](https://goframe.org/en)
- 国内镜像: [https://goframe.org.cn](https://goframe.org.cn)
- 镜像网站: [Github Pages](https://pages.goframe.org)
- 镜像网站: [离线文档](https://github.com/gogf/goframe.org-pdf?tab=readme-ov-file#%E6%9C%80%E6%96%B0%E7%89%88%E6%9C%AC)
- Go包文档: [https://pkg.go.dev/github.com/gogf/gf/v2](https://pkg.go.dev/github.com/gogf/gf/v2)
- 文档源码: [https://github.com/gogf/gf-site](https://github.com/gogf/gf-site)

## 贡献者

💖 [感谢所有使 GoFrame 成为可能的贡献者](https://github.com/gogf/gf/graphs/contributors) 💖

<a href="https://github.com/gogf/gf/graphs/contributors">
<img src="https://goframe.org/img/contributors.svg?version=v2.10.0" alt="goframe contributors"/>
</a>

## 许可证

`GoFrame` 采用 [MIT License](LICENSE) 许可，100% 免费和开源，永久保持。
