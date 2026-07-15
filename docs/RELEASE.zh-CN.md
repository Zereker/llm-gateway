[English](RELEASE.md) | [简体中文](RELEASE.zh-CN.md)

# 发布流程

首个计划 Tag 为 `v0.1.0`。合并发布准备变更不会自动发布任何内容；只有以下门禁
全部完成后，维护者才创建 Tag。

## 创建 Tag 前

1. 确认 `main` 工作区干净、受保护 CI 全部通过，并且 `codecov/project` 与
   `codecov/patch` 都通过。
2. 复核 `SECURITY.md`、公共契约、支持平台、依赖版本与升级边界。
3. 在 `deploy/helm/llm-gateway/Chart.yaml` 中设置发布版本：Chart `version`
   使用 `0.1.0`，`appVersion` 使用已发布的镜像 Tag `v0.1.0`。
4. 在两份 Changelog 中把用户可见变更从 `Unreleased` 移到带日期的发布章节。
5. 执行与 Release 等价的检查：

   ```sh
   go vet ./...
   go test ./internal/...
   make release-snapshot VERSION=v0.1.0
   (cd dist && sha256sum --check SHA256SUMS)
   ```

6. 执行 `make build VERSION=v0.1.0`，再通过 `bin/* -version` 确认两个本地
   二进制都输出预期版本。
7. 确认生产 Helm Chart 只有在显式提供 Secret 后才能渲染，并确认 CI 中的
   Quickstart、多供应商 Smoke Test、Benchmark 和生产镜像检查都已通过。

## 创建 Tag 与发布

从已经复核的 `main` Commit 创建带签名的 Annotated Tag，并只推送该 Tag：

```sh
git switch main
git pull --ff-only
git tag -s v0.1.0 -m "llm-gateway v0.1.0"
git push origin v0.1.0
```

`Release` Workflow 随后会：

- 校验 Tag、Chart 与 Changelog 中的版本一致；
- 运行源码测试，并为所有支持平台构建两个命令；
- 向 GitHub 发布压缩包、`SHA256SUMS` 与打包后的 Helm Chart；
- 向 GHCR 分别发布多架构 Gateway 和 Console 镜像；
- 在每个二进制和镜像中嵌入 Tag、Commit 与构建时间。

## 发布后

1. 从 GitHub 下载一个压缩包，并使用 `SHA256SUMS` 校验。
2. 对两个命令执行 `-version`。
3. 按不可变 Tag 拉取两个镜像，确认它们以 UID/GID `65532:65532` 运行，并在
   合成部署中检查 `/healthz`。
4. 在临时 Namespace 安装打包后的 Chart，验证就绪状态、一次鉴权请求、Console
   访问、Metrics 与优雅退出。
5. 只有这些检查全部通过后才将 Release 标记为非 Draft，并公告兼容性和
   Migration 边界。

发布后的 Tag 绝不能移动或复用。若发布错误，应记录问题并创建下一个 Patch 版本，
同时保留旧产物用于审计。已经随 Tag 发布的 Migration 绝不能修改。
