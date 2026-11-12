# Nora（WeKnora fork）维护规范

> 2026-11-10 确立分叉方针：从"少量向上游提 PR"转为**独立分叉开发 + 定期同步上游**。
> 2026-11-12 重构为单一工作区，并压缩自有历史、固化本节流程。

## 一、仓库与分支

| 位置 | 用途 |
|---|---|
| `/Volumes/Work/code/WeKnora` | **唯一工作树**，停在 `main` |
| `origin` = `github.com/MisonL/WeKnora` | 自己的 fork（public），远端只有 `main` 和发布 tag |
| `upstream` = `github.com/Tencent/WeKnora` | 上游 |

**单一工作区**：不使用 linked worktree。需要隔离时临时开分支：

```bash
git switch -c nora/<topic>
# 开发、提交
git switch main && git merge --no-ff nora/<topic>
git branch -d nora/<topic>
```

**`main` 的职责**：上游的镜像 + 已合入的 fork 结果。`main` 上不留半成品，工作树保持干净——否则 `git merge --ff-only upstream/main` 会被未提交改动挡住。

**发布锚点是 tag，不是分支**：`nora-v<上游版本>-r<镜像序号>`，当前 `nora-v0.9.0-r9`（`ba7d26394`）＝ 生产在用镜像。tag 必须推到 `origin`。任何时候都能用 `git diff nora-v0.9.0-r9..main` 看"生产在跑什么、本地多出什么"。

不再保留 `backup/*` 分支：打 tag 即可，分支是多余额外的引用，容易和 tag 重复。

## 二、提交规范

**一个提交一个主题**，不按时间切、不按文件切。提交信息要说明"为什么改"和"不改会怎样"，而不只是"改了什么"。当前 `main` 有 12 个自有提交：

| 主题 | 覆盖范围 |
|---|---|
| security: AES 密钥 fail-closed | `internal/utils/crypto.go`、两个 `main.go`、`runtime/startup.go` |
| security: IM 凭据加密 + 日志脱敏 | `internal/im/credentials_crypto.go`、`types.go`、`types/config_redaction.go`、`middleware/logger.go` |
| security: 保留无法解密的密文 | `types/undecryptable_secrets.go`、`engine_config_crypto.go`、`mcp.go`、`datasource*` |
| storage: OSS STS + 存储闸门 | `types/oss_auth.go`、`storagebackend*.go`、`service/file/`、`go.mod` |
| retrieval: 生成问题行排除 | `types/generated_question_retrieval.go`、`retriever/postgres/` |
| llm: 重试边界 + 流式错误终态 | `models/api/*`、`chat_pipeline/chat_completion_stream.go` |
| faq: 重试游标 + 推荐标记 + 失败导出 | `knowledge_faq*.go`、`repository/chunk.go`、`client/faq.go` |
| im: 问答队列 + 上游超时 | `internal/im/qaqueue.go`、`service.go`、`cmd_search.go` |
| api: 部分更新不再清空字段 | 各 handler 的 presence 标记 + 对应 types/repository |
| db: 连接池默认上限 | `container/db_pool.go` |
| deploy: staging overlay + 增量发布 | `deploy/nora/`、`docker-compose.yml`、`.env.example` |
| docs: API 文档 + 部分更新契约 | `docs/`、`initialization_partial_test.go` |

**压缩自有历史**：把零碎的 `docs:` 和在同主题里的多个 fix 合并，减少同步上游时的重放/冲突面。压缩时用 `git reset --soft <base>` 重建，**重建后必须校验树内容未变**：

```bash
BEFORE=$(git rev-parse HEAD^{tree})
# ...重建提交...
git diff --stat $BEFORE HEAD    # 应为空
```

注意 `go.mod` 这类依赖变更要落在**引入该依赖的那个提交**里，否则中间提交无法编译。

## 三、同步上游：用 merge，不要 rebase

```bash
git fetch upstream
git switch -c nora/sync-upstream
git merge upstream/main        # 冲突一次解完
# 按第五节验证，通过后：
git switch main && git merge --ff-only nora/sync-upstream
git branch -d nora/sync-upstream
```

**为什么不用 rebase**：自有提交数量多（当前 12 个），rebase 要逐个重放，同一批文件（`internal/application`、`internal/types`）会被反复冲突。而且 rebase 的 `--ours/--theirs` 语义与 merge 相反，历史上已经踩过一次。

代价是历史里多出 merge 提交，以及 `git log upstream/main..main` 会混入上游提交——用 `git log --no-merges --first-parent` 过滤即可。

**必须开启的 git 配置**（一次性，已配置）：

```bash
git config rerere.enabled true      # 记住冲突解法，上游再改同一处时自动复用
git config rerere.autoUpdate true   # 解完即暂存
git config merge.conflictstyle zdiff3  # 冲突块带公共祖先，便于分辨谁改了什么
```

## 四、发布流程

镜像 `weknora-app:v0.9.0-oss-sts-rN`。compose 里统一写 `${NORA_IMAGE_TAG:-v0.9.0-oss-sts-r9}`，发新版只需在 `/opt/nora/.env` 设 `NORA_IMAGE_TAG`，不改 compose。

**本机直连 `mirrors.tencent.com:80` 会被 DNS 劫持（198.18.x），构建必须走代理** `127.0.0.1:7890`（构建参数 `host.docker.internal:7890`）。

增量发布用 `deploy/nora/build-delta.sh`：只传与目标机已有镜像不同的文件，在目标机上用 `FROM` + `COPY` 组装，避免通过慢链路搬 2.45 GB。生产回滚镜像的保留策略见 [[nora-image-rotation-policy]]。

## 五、验证清单

合并、rebase 或压缩历史后必跑：

```bash
go build ./...
go vet ./...
go test ./internal/handler ./internal/types ./internal/im ./docs
go test ./internal/handler -run TestUpdateKBConfigPartial -v   # 11 场景必须全绿
```

### 环境噪声的判定方法

本机 DNS 把外部域名解析进保留段 `198.18.0.0/15`，被 SSRF 守卫拒绝，导致一批与代码无关的失败。当前基线是 **10 个包**：

```
internal/application/service
internal/application/service/file
internal/datasource
internal/datasource/connector/notion
internal/datasource/connector/yuque
internal/handler
internal/models/embedding
internal/models/parity
internal/sandbox
internal/utils
```

判定方法：用临时 worktree 在干净 HEAD 上跑同一套测试做基线对照，失败包集合一致即环境问题。

```bash
git worktree add --detach /tmp/nora-baseline HEAD
(cd /tmp/nora-baseline && go test ./... 2>&1 | awk -F'\t' '/^FAIL\t/{print $2}' | sort -u > /tmp/baseline.norm)
go test ./... 2>&1 | awk -F'\t' '/^FAIL\t/{print $2}' | sort -u > /tmp/current.norm
comm -23 /tmp/current.norm /tmp/baseline.norm   # 新增回归，应为空
git worktree remove --force /tmp/nora-baseline
```

**坑**：`go test` 原始输出行是 `FAIL<TAB>包名<TAB>耗时`，直接和裸包名列表 `comm` 会得出满屏假阳性。必须先按第二列归一化。

## 六、有意的分叉点（合并上游时注意别被覆盖回去）

1. **OSS STS 凭据链路**（`internal/types/oss_auth.go`、`internal/application/service/file/oss_sts.go`：`ram_role_arn`、`CredentialsProvider` 接缝、失败 backoff、`GET /system/admin/diagnostics/sts`）——上游没有同等实现。
2. **存储闸门**（`types/storagebackend.go` 的 `Validate()` 拒绝非环境来源的 `ram_role_arn`；`service/storagebackend.go` 的 `ErrStorageRoleNotAllowed` fail-closed；env 后端只读、不可删）。
3. **`im_channels.credentials` 整列加密**（`internal/im/credentials_crypto.go`）——对应 [[weknora-im-credentials-encrypted]]。回滚旧镜像前必须先还原明文行。
4. **`PUT /initialization/config` 的 presence-aware 合并**——上游已原生实现，我们只保留 `initialization_partial_test.go` 的 11 场景作为契约；上游若改动这块，以测试为准。
5. **部署脚本 `deploy/nora/`**（build-delta 增量发布、staging overlay、`nora-up.sh` 强制带 `-f` 加载 override）。

## 七、合并陷阱（已踩过的坑）

- **swagger 不要手工解冲突**：`docs/` 三个文件是生成物，冲突时直接取上游基线，再用仓库固定的 `swag v1.16.6`（`go install github.com/swaggo/swag/cmd/swag@v1.16.6`）按 `make docs` 的参数重新生成，最后跑 `go test ./docs` 让契约测试兜底。2026-11-10 曾因取错 side 一次删掉上游 5000+ 行文档。
- **rebase 的 `--ours/--theirs` 与 merge 相反**：rebase 中 `--ours` 是新基底（上游），`--theirs` 是正在重放的提交（我们的）。这也是改用 merge 的原因之一。
- **上游测试能暴露分叉自带的缺陷**：2026-11-10 `internal/im` 的 9 个用例失败，根因是我们 `newOSSClientWithConfig` 把 endpoint 的 `http://` 剥掉，SDK 缺省升级为 https——明文 HTTP endpoint（本地 MinIO、httptest）全挂。已修。
- **部分更新类接口的零值陷阱**：`KnowledgeBaseConfig` 等结构体按值存字段，"未提供"和"零值"无法区分，任何 PATCH 都可能清空未提及的配置。修法是请求类型在 `UnmarshalJSON` 里记录 presence，再在 handler 里用存量值回填。
- **日志脱敏要覆盖 IM 的密钥名**：IM 渠道凭证不走 `api_key`/`secret` 命名，`middleware/logger.go` 需要额外识别 `bot_secret`、`app_secret` 等。

## 八、历史沿革

- 2026-11-10：确立独立分叉；`nora/sts` rebase 到 `upstream/main`（`cbdcebabf`），以 `--no-ff` 合入 `main`（merge commit `63b97d65c`）。
- 2026-11-12：移除 linked worktree `WeKnora-sts`（它与 `main` 同点，无独有工作）；删除 `backup/main-20261110`（已被 `main` 完全包含）和 `backup/nora-sts-20261110`（与 tag `nora-v0.9.0-r9` 同一提交）；自有历史由 26 个提交压缩为 12 个，改动规模 138 个文件、+7531/−650（相对 `upstream/main`）。

相关：[[nora-r9-pr5223-released]]、[[weknora-partial-update-field-wipe]]、[[nora-fork-independent-development]]、[[nora-worktree-and-recovery]]
