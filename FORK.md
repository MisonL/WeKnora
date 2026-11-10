# Nora（WeKnora fork）独立分叉约定

> 2026-11-10 确立。从"少量向上游提 PR"转为**独立分叉开发 + 定期同步上游**。

## 仓库与分支

| 位置 | 用途 |
|---|---|
| `/Volumes/Work/code/WeKnora` | **主工作树**，停在 `main`；负责 fetch upstream、与上游同步 |
| `/Volumes/Work/code/WeKnora-sts` | **开发工作树**（同一仓库的 linked worktree），分支 `nora/sts` |
| `origin` = `github.com/MisonL/WeKnora` | 自己的 fork，**远端只有 main**，`nora/*` 分支从未推送 |
| `upstream` = `github.com/Tencent/WeKnora` | 上游 |

两个工作树共用同一 `.git`，所以 `git -C /Volumes/Work/code/WeKnora` 上的分支操作对另一棵树同样可见。

**分支命名**：功能/修复分支统一 `nora/<topic>`，合入 `nora/sts` 后删除；里程碑打 tag `nora-v<上游版本>-r<镜像序号>`（当前 `nora-v0.9.0-r9` = 生产在用镜像）。删除分支前先打 `nora-<branch>-<date>` 备份标签（2026-11-10 已对 faq-csv-fix / security-aes / sts-core 做过）。

## 收敛状态（2026-11-10，rebase 后）

- `nora/sts` 已 rebase 到 `upstream/main`（`cbdcebabf`），22 → 21 个自有提交，无 merge 提交；`main` 以 `--no-ff` 合入（merge commit `63b97d65c`）。
- rebase 前状态留档：`backup/nora-sts-20261110`（`ba7d26394`）、`backup/main-20261110`（`7efa17381`）、tag `nora-v0.9.0-r9`。
- **上游已原生包含 #5223**（`417e39593`）和 #5207（`b1bb33971`）：我们此前移植的 `5f9c2e04c` 在 rebase 时让位给上游实现，只保留 `initialization_partial_test.go` 的 11 场景作为契约。
- 原来刻意不带的上游"顺带改动"（questionGeneration 独立模型 ID、ASR 报错细化、`applyKBStorageBinding` 提前 return）**现已随上游进入代码**，分叉点 4 的差异只剩测试契约。

## 同步上游的流程

```bash
# 1) 主工作树：main 快进到上游
git -C /Volumes/Work/code/WeKnora fetch upstream
git -C /Volumes/Work/code/WeKnora merge --ff-only upstream/main

# 2) 开发工作树：rebase 自有提交
cd /Volumes/Work/code/WeKnora-sts
git rebase main                  # 冲突按语义修，不要整块取上游

# 3) 必须验证（见下"验证清单"），通过后合回 main
git -C /Volumes/Work/code/WeKnora merge --no-ff nora/sts
```

也可以走 merge（`git merge upstream/main`）而不是 rebase，两种都行；rebase 的好处是与上游线性对齐，代价是冲突要逐个提交重放。

**验证清单（合并/rebase 后必跑）**：
```bash
go build ./...
go vet ./...
go test ./internal/handler ./internal/types ./internal/im ./docs
go test ./internal/handler -run TestUpdateKBConfigPartial -v   # 11 场景必须全绿
```

## 有意的分叉点（合并上游时注意别被覆盖回去）

1. OSS STS 凭据链路（`internal/types/oss_auth.go`、`internal/application/service/file/oss_sts.go`：`ram_role_arn`、`CredentialsProvider` 接缝、失败 backoff、`GET /system/admin/diagnostics/sts`）——上游没有同等实现。
2. 存储闸门（`internal/types/storagebackend.go` 的 `Validate()` 拒绝非 env 来源的 `ram_role_arn`；`internal/application/service/storagebackend.go` 的 `ErrStorageRoleNotAllowed` fail-closed；env 后端只读、不可删）。
3. `im_channels.credentials` 整列加密（`internal/im/credentials_crypto.go`，对应 [[weknora-im-credentials-encrypted]]）。
4. `PUT /initialization/config` 的 presence-aware 合并——**上游已原生实现**，我们保留 `initialization_partial_test.go` 11 场景作为契约；上游若改动这块，以测试为准。
5. 部署脚本 `deploy/nora/`（build-delta 增量发布、staging overlay、`nora-up.sh` 强制带 `-f` 加载 override）。

## 合并陷阱（已踩过的坑）

- **swagger 不要手工解冲突**：`docs/` 三个文件是生成物，冲突时直接取上游基线，再用仓库固定的 `swag v1.16.6`（`go install github.com/swaggo/swag/cmd/swag@v1.16.6`）按 `make docs` 的参数重新生成，最后跑 `go test ./docs` 让契约测试兜底。2026-11-10 曾因取错 side 一次删掉上游 5000+ 行文档。
- **rebase 的 `--ours/--theirs` 与 merge 相反**：rebase 中 `--ours` 是新基底（上游），`--theirs` 是正在重放的提交（我们的）。
- **上游测试能暴露分叉自带的缺陷**：2026-11-10 `internal/im` 的 9 个用例失败，根因是我们 `newOSSClientWithConfig` 把 endpoint 的 `http://` 剥掉，SDK 缺省升级为 https——明文 HTTP endpoint（本地 MinIO、httptest）全挂。已修。
- **本机测试噪声**：外部域名被 DNS 劫持到 `198.18.0.0/15`，被 SSRF 守卫拒绝，导致大量与代码无关的失败（如 `internal/application/service` 48 个）。判定方法：在干净的 `main` 工作树跑同一条用例做基线对照，数量一致即环境问题。

## 发布

镜像 `weknora-app:v0.9.0-oss-sts-rN`。本机直连 `mirrors.tencent.com:80` 会被 DNS 劫持（198.18.x），**构建必须走代理** `127.0.0.1:7890`（构建参数 `host.docker.internal:7890`）。增量发布用 `deploy/nora/build-delta.sh`，生产回滚备份/保留策略见 [[nora-image-rotation-policy]]。

> 注意：仓库里的 `deploy/nora/docker-compose.app-oss.yml` 写的是不带序号的 `weknora-app:v0.9.0-oss-sts`，而生产实际使用 `-r9`（服务器上的副本已改过）。从仓库直接部署前必须先把 tag 改成目标 `-rN`，否则会指向不存在的镜像。

相关：[[nora-r9-pr5223-released]]、[[weknora-partial-update-field-wipe]]、[[nora-fork-independent-development]]
