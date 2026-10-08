# Codex 真实客户端版本配对

Desktop 和 VSCode 的应用版本与其内置 CLI 版本一起同步、一起缓存。应用版本升级时，CLI 可能保持不变、降级或带 `alpha` 后缀，不能用最新稳定 CLI 拼接一个不存在的配对。

## 来源与目标

| 客户端 | 目标 | 首选来源 | 校验与回退 |
| --- | --- | --- | --- |
| Desktop macOS | `darwin-arm64`、`darwin-x64` | 官方对应架构的 appcast | Range 读取 ZIP 的外层 `Contents/Info.plist` 与 `codex-cli/codex-package.json`；核对应用、构建号和 CLI 架构 |
| Desktop Windows | `win32-x64`、`win32-arm64` | `https://codexapp.agentsmirror.com/latest/manifest` | 映射必须匹配官方 Windows update manifest 的包身份、包版本和架构；失败时读取官方下载地址的 MSIX |
| VSCode | macOS、Windows、Linux，各自 x64/ARM64 | 官方 Marketplace 最新稳定 `openai.chatgpt` | Range 读取对应原生 VSIX 的扩展版本与 CLI manifest；Windows 包内的 Linux/WSL CLI 不作为 Windows 版本 |
| TUI / exec | 独立 CLI | 官方 GitHub 最新稳定 release | 保留原来的 CLI 同步逻辑，独立于 Desktop / VSCode 配对 |

选择稳定版 Desktop/扩展包，不代表其内置 CLI 必须是稳定版。内置 manifest 或二进制中的完整 CLI 版本（例如 `0.158.0-alpha.2.1`）会原样保留。

## Windows 官方 fallback

现有 MSIX 可能没有 `codex-package.json`，不能把“manifest 不存在”当作所有官方包都不受支持。fallback 流程如下：

先读取官方版本化下载地址；仅在返回 404 时，回退到对应架构的官方最新包地址。最新包可能落后于 Store 清单，沿用上游对主次版本的校验，保存包内实际版本和完整配对。可变下载地址以 URL、ETag、Last-Modified 和大小标识安装包；ETag 未变化时只需再次探测一个字节，并重新校验实际包版本与当前 Store 清单，即可复用配对缓存。

1. 获取 ZIP/ZIP64 目录，读取 `AppxManifest.xml`，核对 `OpenAI.Codex`、官方包版本与目标架构。
2. 若包内有 CLI manifest，使用 manifest；无 manifest 时读取 `AppxBlockMap.xml`。损坏或架构不匹配的 manifest 不会被忽略。
3. 利用 block map 中每个 64 KiB 原始块的压缩大小定位字节区间，独立解压所需块；核对原始长度与 SHA-256。非末块没有 DEFLATE 结束标志，仅在长度和哈希均正确时接受解压的 `UnexpectedEOF`。
4. 读取 `app.asar` 的头和根 `package.json` 所在块，直接跳过中间内容，取得真实桌面端构建号。
5. 读取 `codex.exe` 的 PE 头，核对 x64/ARM64，定位只读 `.rdata`，分批读取最多 8 MiB 原始数据。CLI 编译版本必须同时出现在 `codex-doctor/<version>` 与 `version: <version>\nplatform: ` 两个固定字符串中，且版本一致。不会执行下载的二进制。

如果压缩格式、块索引、哈希或版本字符串不受支持，仅返回失败状态，不写入数据库，继续使用上一次成功同步的完整配对。初次同步没有缓存配对时，使用明确标为 `builtin_observed` 的内置配对；不会改用任意最新 CLI，也不会继续下载全包。

读取预算对每个安装包独立生效：最多 **12 MiB Range 响应正文、64 次请求**，缓存块大小 256 KiB。服务器必须返回严格匹配的 `206`、`Content-Range`、正文长度和稳定的 ETag / Last-Modified；返回整包 `200` 时直接停止。只有格式正确的强 ETag 才作为 `If-Range` 发出，兼容 Marketplace 的未加引号 ETag。普通小文件最多 64 KiB，block map 和 ASAR 头分别最多 8 MiB。

2026-10-01 在官方包 `26.928.3736.0` 上验证：

| 架构 | 桌面端版本 | 内置 CLI | Range 正文字节 | 请求次数 |
| --- | --- | --- | --- | --- |
| x64 | `26.928.31416` | `0.159.2` | 5,297,477（约 5.1 MiB） | 22 |
| ARM64 | `26.928.31416` | `0.159.2` | 5,588,152（约 5.3 MiB） | 23 |

这些数据包含应用版本与 CLI 版本的完整解析。小映射清单有效时无需读取 MSIX；已验证安装包未变化时直接复用缓存，不重新读取安装包。SHA-256 校验的是从官方 HTTPS block map 取得的块哈希，不等同于下载全包后验证 MSIX 签名。

迁移至 upstream main 后，另外验证了 x64 的版本化地址 404 回退：官方最新包完整配对解析仍读取 5,297,477 字节，共 23 次请求；再次同步只需 2 次请求（版本化地址 404 探测、最新包的 1 字节 Range 探测），Range 正文总共 1 字节。

## 选择与手动配置

- 自动 Desktop / VSCode 根据客户端种类、操作系统和架构选择完整配对，优先最新应用构建。
- `auto` 兼容模式的最低 CLI 版本仅过滤候选配对，按完整语义版本比较；`0.158.0-alpha.1` 不满足 `0.158.0`。没有满足条件的已知配对时，出站前返回不可重试的 HTTP 503，错误码 `codex_client_version_unavailable`，预览返回 400。
- 手填 `client_version` 或 `app_version` 分别覆盖对应默认值，不根据手填版本重新配对；手填 CLI 不受最低版本抬升。只填应用版本时，自动 CLI 仍受最低版本过滤。
- 原始 `user_agent` 保持原样。手动覆盖在预览中标为 `custom`。
- HTTP、compact、WebSocket、alpha search、live 和遥测共享身份解析。WebSocket 按最终 UA、Version、Originator 隔离池；配置或配对变化后重新握手，旧身份的续链连接不再被新身份复用。版本变化时旧连接上的 `previous_response_id` 上下文可能需要由客户端重新建立。

## 缓存和管理接口

数据库自动创建 `codex_client_version_cache`，以 `(client_kind, target_platform)` 为主键，每个目标只保存最后一次成功同步的完整版本配对，不累积历史。同步成功后更新单个目标，事务成功后发布不可变内存快照；同步或数据库写入失败时保留原数据和生效版本。重启会先加载缓存，即使关闭后台同步也会加载。加载缓存不修改数据库；已有历史配对由实例管理员手动清理。

`GET /api/admin/settings` 的只读 `codex_client_versions` 展示 10 个 Desktop / VSCode 目标及当前配对、来源、状态和最近成功检查时间；本次同步错误只在同步响应中返回。普通设置保存不会写回此缓存。兼容字段 `codex_synced_desktop_mac_build`、`codex_synced_desktop_windows_build`、`codex_synced_vscode_build` 分别投影 macOS ARM64、Windows x64 和 VSCode Linux x64；旧版独立字符串不迁移成配对。

`POST /api/admin/settings/codex-client-versions/sync` 保留 `cli`、`desktop_mac`、`desktop_windows`、`vscode` 四类结果，增加 CLI 版本、来源、状态与各目标结果。并发调用合并，安装包解析最多并发 2 个；单一来源失败不阻断其他来源。

管理后台「系统设置 → Codex」的「立即同步」可同步各架构的配对并显示来源。CLI/应用版本输入框留空即可使用自动配对，默认值取后端预览。现有同步开关、间隔与 `CODEX_DISABLE_CLI_VERSION_SYNC` 继续生效；该环境变量关闭后台同步，不关闭手动同步。

## 验证

关键回归覆盖 ZIP64、Range 拒绝整包与预算、版本/架构不匹配、原生 VSIX 与 WSL 区分、MSIX 独立块及哈希、稀疏 ASAR、alpha 字符串一致性、缓存失败不写库/重启恢复/并发替换、设置只读、手动覆盖、最低版本不可用以及 HTTP / WS 出站前拦截。

```sh
go test -tags=http2legacy ./proxy ./proxy/wsrelay ./admin ./database
go test -race -tags=http2legacy ./proxy -run 'TestCodex(ClientVersionCache|ClientVersionSync|ClientCandidate|MSIX|PEVersion|OfficialMSIX|Archive|ClientIdentity)'
cd frontend
pnpm typecheck
pnpm test
pnpm build
```

本次环境为 Go 1.27.1。已有 uTLS / `golang.org/x/net/http2` 路径在该环境的部分网络测试中触发空指针，因此完整相关包验证使用仓库已有的 `http2legacy` 构建标签。版本读取本身不依赖该标签；无网络依赖的版本用例也可直接运行。

参考：[Microsoft 的 MSIX 更新与 64 KiB block map](https://learn.microsoft.com/en-us/windows/msix/app-package-updates)、[MSIX 的 DEFLATE 块讨论](https://stackoverflow.com/questions/79511882/are-the-blocks-in-appxblockmap-xml-directly-tied-to-deflate-compressed-blocks-in)、[Codex doctor 编译版本标记](https://github.com/openai/codex/blob/main/codex-rs/cli/src/doctor/updates.rs)。
