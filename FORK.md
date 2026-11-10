# Nora（WeKnora fork）独立分叉约定

> 2026-11-10 确立。从"少量向上游提 PR"转为**独立分叉开发 + 定期合并上游**。

## 仓库与分支

| 位置 | 用途 |
|---|---|
| `/Volumes/Work/code/WeKnora-sts` | **唯一开发工作树**，分支 `nora/sts`（持久，见 [[nora-worktree-and-recovery]]） |
| `/Volumes/Work/code/WeKnora` | 父仓库克隆，平时停在 `main`，只用来 fetch / 建临时 worktree 做上游合并 |
| `origin` = `github.com/MisonL/WeKnora` | 自己的 fork，**远端只有 main**，`nora/*` 分支从未推送 |
| `upstream` = `github.com/Tencent/WeKnora` | 上游 |

**分支命名**：功能/修复分支统一 `nora/<topic>`，合入 `nora/sts` 后删除；里程碑打 tag `nora-v<上游版本>-r<镜像序号>`（当前 `nora-v0.9.0-r9` = 生产在用镜像）。删除分支前先打 `nora-<branch>-<date>` 备份标签（2026-11-10 已对 faq-csv-fix / security-aes / sts-core 做过）。

## 收敛状态（2026-11-10）

- `nora/sts` HEAD `5f9c2e04c`，工作树干净。
- 分叉自 `v0.9.0`，34 个自有提交，与 `upstream/main` 的合并基是 `f17ee8a8d`；上游此后又走了 129 个提交。
- 原有三条并行小分支（faq-csv-fix、security-aes、sts-core）内容已全部进入 `nora/sts`，分支已删、以 tag 留档。

## 合并上游的流程

```bash
cd /Volumes/Work/code/WeKnora-sts
git fetch upstream
git checkout -b nora/merge-upstream-<date> nora/sts
git merge upstream/main          # 冲突按语义修，不要整块取上游
# 合并后必须：go build ./... 、go test ./internal/handler ./internal/application/service
# 然后部署到 staging（/opt/nora-stg，见 docker-compose.staging.r9.yml）验证，再发生产 rN+1
git checkout nora/sts && git merge --no-ff nora/merge-upstream-<date>
```

**有意的分叉点**（合并上游时注意别被覆盖回去）：
1. OSS STS 凭据链路（`ram_role_arn`、`CredentialsProvider` 接缝、backoff、诊断接口）——上游没有对企业 STS 的同等实现。
2. 存储闸门（`internal/types/storagebackend.go` 的 `Validate()` 拒绝非 env 来源的 ram_role_arn、`extract.go` fail-closed）。
3. `im_channels.credentials` 整列加密（对应 [[weknora-im-credentials-encrypted]]）。
4. `PUT /initialization/config` 的 presence-aware 合并（PR #5223 移植，`initialization_partial_test.go` 11 场景）。若上游后续改了这块，以测试为准重放。
5. 部署脚本 `deploy/nora/`（build-delta 增量发布、staging overlay）。

**已经吃过一次亏的合并陷阱**：上游在同一文件邻近位置还带了三类"顺带"改动（questionGeneration 独立模型 ID、ASR 报错细化、`applyKBStorageBinding` 提前 return），移植 #5223 时刻意没带——以后合并要继续只取需要的部分。

## 发布

镜像 `weknora-app:v0.9.0-oss-sts-rN`。本机直连 `mirrors.tencent.com:80` 会被 DNS 劫持（198.18.x），**构建必须走代理** `127.0.0.1:7890`（构建参数 `host.docker.internal:7890`）。增量发布用 `deploy/nora/build-delta.sh`，生产回滚备份/保留策略见 [[nora-image-rotation-policy]]。

相关：[[nora-r9-pr5223-released]]、[[weknora-partial-update-field-wipe]]
