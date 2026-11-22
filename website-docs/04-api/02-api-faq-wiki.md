# API 参考：FAQ 与 Wiki

管理知识库中的 FAQ 条目与 Wiki 页面，支持导入、检索、编辑和版本恢复。

两组均为 KB 内容子资源：读为 Viewer+ 且 KB read（API key `retrieve`/full）；写为“KB 创建者 OR Admin+”且 KB write（API key `ingest`/full），并受 KB 白名单约束。跨库 Wiki 搜索 `POST /wiki-search` 无路径 KB，Viewer+ 且 API key `retrieve`/full，handler 内校验每个目标库的 allow-list 与共享库 Viewer 权限。

## FAQ（/api/v1/knowledge-bases/:id/faq）

### GET /api/v1/knowledge-bases/:id/faq/entries

用途：FAQ 条目列表。

| 查询参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `page` / `page_size` | int | 否 | 分页 |
| `tag_id` | int | 否 | 旧版单标签 seq_id |
| `tag_ids` | string | 否 | 逗号分隔标签 UUID |
| `keyword` | string | 否 | 关键字 |
| `search_field` | string | 否 | `standard_question`/`similar_questions`/`answers`（默认全字段） |
| `sort_order` | string | 否 | `asc`（默认按更新时间倒序） |
| `is_enabled` | bool | 否 | 按启用状态筛选：`true` 仅启用、`false` 仅停用，不传返回全部；其他取值返回 400 |

响应：200 `{"success":true,"data":{分页 FAQEntry 列表}}`

```bash
curl "$BASE/api/v1/knowledge-bases/kb-1/faq/entries?page=1&is_enabled=false" -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledge-bases/:id/faq/entries/export

用途：导出 FAQ。查询参数：`format`（`csv` 默认 / `json`）。

响应：200 文件下载（`text/csv` 或 `application/json`）。

```bash
curl -OJ "$BASE/api/v1/knowledge-bases/kb-1/faq/entries/export?format=csv" -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledge-bases/:id/faq/entries/:entry_id

用途：FAQ 条目详情（`entry_id` 为整数 seq_id）。

响应：200 `{"success":true,"data":{FAQEntry}}`

```bash
curl $BASE/api/v1/knowledge-bases/kb-1/faq/entries/12 -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/knowledge-bases/:id/faq/entries

用途：批量 upsert / 导入（异步任务）。Handler 方法 `UpsertEntries`。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `entries` | []FAQEntryPayload | 是，且**不能是空数组** | 批量条目 |
| `mode` | string | 是，只能是 `append` 或 `replace` | 追加或替换 |
| `knowledge_id` | string | 否 | FAQ 知识实体 ID |
| `task_id` | string | 否 | 自定义任务 ID，仅允许字母、数字、`_`、`-`，最长 128 字符，否则返回 400 |
| `dry_run` | bool | 否 | 仅校验不落库 |

请求体必须是**单个完整的 JSON 对象**：读完整个值之后若还有多余内容（例如 `{"mode":"append","entries":[...]}xyz`），会被拒绝。这是为了在做条数预算前先把整个请求体读进内存——解码器不接受尾随内容。

响应：200 `{"success":true,"data":{"task_id"}}`

**导入上限**（超限在解析前直接拒绝，不会创建任务）：

| 预算 | 默认 | 配置 | 超限响应 |
| --- | --- | --- | --- |
| 单次条目数 | 1000 | `WEKNORA_FAQ_IMPORT_MAX_ENTRIES` | 400 |
| 请求体字节数 | 32 MiB，且不超过 `MAX_FILE_SIZE_MB` | `WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES` | 413（`error.code` 1011） |

同时刻的在途导入请求数也有预算（`WEKNORA_FAQ_IMPORT_MAX_INFLIGHT`/`WEKNORA_FAQ_IMPORT_MAX_INFLIGHT_BYTES`），超限返回 429 并带 `Retry-After`；请求在读取请求体之前就被拒绝，队列稍后按该头重试即可。

`append` 超限时按上限分批提交即可。**`replace` 不能用分批绕过**：它按完整新集合计算删除差集，
连续提交多批 replace 会把每批以外的条目当作待删除项。也不能直接换用 append 顶替：append 只追加、
不删除，旧条目会和新的并存。需要整库替换时的正路是调高导入上限、一次提交完整集合——**两个预算都可能要先调**：条目数不够就调
`WEKNORA_FAQ_IMPORT_MAX_ENTRIES`；字节数先触顶就调 `WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES`，而它又被
`MAX_FILE_SIZE_MB` 封顶，所以那次要一并调高 `MAX_FILE_SIZE_MB`。只调条目数而请求体仍超字节预算，一样是 413。「先清空再 append 分批」不是等价替代：清空会立即且不可逆地删除全部旧条目，
之后任何一批 append 失败都不会把它恢复，所以只有在**已导出备份**的前提下才这么做。

两个预算都生效，哪个先到就用哪个拒绝。默认值下条目数通常先到：常见的一条 FAQ（一个标准问加几句
答案）只有几百字节，1000 条的请求体通常在几百 KB 量级，离 32 MiB 很远。但如果单条携带大量相似问和长答案，
字节数会先触顶。字节超限在 `replace` 模式下同样不能靠拆分解决——拆批会让批外条目被删；只有
`append` 才可以把请求体按字节拆成几批提交。

```bash
curl -X POST $BASE/api/v1/knowledge-bases/kb-1/faq/entries -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"mode":"append","entries":[{"standard_question":"如何退款?","answers":["联系客服"]}]}'
```

### POST /api/v1/knowledge-bases/:id/faq/entry

用途：创建单条 FAQ。请求体（`types.FAQEntryPayload`）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `standard_question` | string | 是（`binding:"required"`） | 标准问 |
| `similar_questions` | []string | 否 | 相似问 |
| `negative_questions` | []string | 否 | 负样例问 |
| `answers` | []string | 否 | 答案列表 |
| `answer_strategy` | string | 否 | `all` / `random` |
| `tag_id` | int64 | 否 | 标签 seq_id |
| `tag_name` | string | 否 | 标签名 |
| `is_enabled` / `is_recommended` | *bool | 否 | 启用/推荐 |

响应：200 `{"success":true,"data":{FAQEntry}}`

```bash
curl -X POST $BASE/api/v1/knowledge-bases/kb-1/faq/entry -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"standard_question":"如何退款?","answers":["7 天内可退"]}'
```

### PUT /api/v1/knowledge-bases/:id/faq/entries/:entry_id

用途：更新单条 FAQ（请求体同创建）。

响应：200 `{"success":true,"data":{FAQEntry}}`

```bash
curl -X PUT $BASE/api/v1/knowledge-bases/kb-1/faq/entries/12 -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"standard_question":"如何退款?","answers":["30 天内可退"]}'
```

### POST /api/v1/knowledge-bases/:id/faq/entries/:entry_id/similar-questions

用途：追加相似问。请求体：`{"similar_questions":["..."]}`（`binding:"required,min=1"`）。

响应：200 `{"success":true,"data":{FAQEntry}}`

```bash
curl -X POST $BASE/api/v1/knowledge-bases/kb-1/faq/entries/12/similar-questions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"similar_questions":["退款怎么操作"]}'
```

### PUT /api/v1/knowledge-bases/:id/faq/entries/fields

用途：批量更新条目字段（`is_enabled`/`is_recommended`/`tag_id`）。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `by_id` | map[int64]object | 否 | 按条目 seq_id 更新，**最多 20 个** |
| `by_tag` | map[int64]object | 否 | 按标签批量更新，不设条数上限（会触达该标签下全部条目） |
| `exclude_ids` | []int64 | 否 | `by_tag` 时排除的条目，**最多 20 个**；不带 `by_tag` 时它不参与任何更新（因此不受上限约束） |

**排除列表不能拆批、不能截短。** 每次 `by_tag` 更新都会改动它没有排除的所有条目，所以分两次提交时，各自会把对方排除的条目改掉；把列表删到 20 条再提交，被删掉的那些条目就会被更新。要处理「除这 30 条以外全部改」这类请求，先按完整排除集合算出真正要改的条目，再用 `by_id` 分批提交这些条目，并且**不要同时带 `by_tag` 或 `exclude_ids`**——`by_id` 会精确更新你列出的条目，不需要排除。

响应：200 `{"success":true}`

```bash
curl -X PUT $BASE/api/v1/knowledge-bases/kb-1/faq/entries/fields -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"by_id":{"12":{"is_enabled":false}}}'
```

### PUT /api/v1/knowledge-bases/:id/faq/entries/tags

用途：批量改条目标签。请求体：`{"updates":{"<entry_id>":<tag_id|null>}}`（`binding:"required,min=1,max=20"`；null 移除标签）。

响应：200 `{"success":true}`

```bash
curl -X PUT $BASE/api/v1/knowledge-bases/kb-1/faq/entries/tags -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"updates":{"12":3}}'
```

### DELETE /api/v1/knowledge-bases/:id/faq/entries

用途：批量删除条目。请求体：`{"ids":[int64]}`（`binding:"required,min=1,max=20"`）。

一次最多 20 个 ID，与 Web UI 每页 20 行一致。需要删除更多时按 20 个一组分批提交。

响应：200 `{"success":true}`

```bash
curl -X DELETE $BASE/api/v1/knowledge-bases/kb-1/faq/entries -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"ids":[12,13]}'
```

### POST /api/v1/knowledge-bases/:id/faq/search

用途：FAQ 检索（只读语义，scoped key 用 `retrieve` 亦可调用）。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `query_text` | string | 是（`binding:"required"`） | 查询 |
| `vector_threshold` | float64 | 否 | 向量阈值，默认 0.7 |
| `match_count` | int | 否 | 默认 10，上限 50 |
| `first_priority_tag_ids` / `second_priority_tag_ids` | []int64 | 否 | 标签优先级过滤 |
| `only_recommended` | bool | 否 | 仅推荐条目 |

响应：200 `{"success":true,"data":[FAQEntry(含 match_type/score)]}`

```bash
curl -X POST $BASE/api/v1/knowledge-bases/kb-1/faq/search -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/json' -d '{"query_text":"退款"}'
```

### PUT /api/v1/knowledge-bases/:id/faq/import/last-result/display

用途：设置最近一次导入结果面板的显示状态。请求体：`{"display_status":"open|close"}`（`binding:"required,oneof=open close"`）。

响应：200 `{"success":true}`

```bash
curl -X PUT $BASE/api/v1/knowledge-bases/kb-1/faq/import/last-result/display \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"display_status":"close"}'
```

### GET /api/v1/faq/import/progress/:task_id

用途：查询 FAQ 导入/dry-run 进度（任务按空间隔离）。权限：Viewer+；API key `retrieve`/`ingest`/full。

响应：200 `{"success":true,"data":{status,progress,failed_entries,...}}`

任务完成时 `data` 带四类计数：

| 字段 | 含义 |
|---|---|
| `success_count` | 完全按提交内容导入的条目数 |
| `partial_failed_count` | 已导入、但冲突的相似问/反例被移除的条目数（`failed_entries` 中 `is_partial_failure` 为真） |
| `failed_count` | 未能导入的条目数 |
| `skipped_count` | 跳过的条目数，按 `total - 前三项` 推出，下限为 0 |

正常情况下四项相加等于 `total`；`skipped_count` 是推出来的余数而非独立计数，所以计数本身异常时它会停在 0，四者可能对不上。

`success_count` 与 `failed_count` 始终出现；`partial_failed_count`、`skipped_count` 等可选计数带 `omitempty`，零值不出现在 JSON 里，因此「字段缺失」等于 0，不代表旧服务端不支持。`failed_entries` 非空时会尝试导出为 CSV：生成成功才清空内联列表并给出 `failed_entries_url`（此时判断部分失败必须看 `partial_failed_count`）；导出失败则保留内联 `failed_entries`、不写 URL。

dry-run 任务的记录只描述它自己验证的这批条目，不会套用知识库上一次真实导入的统计。

```bash
curl $BASE/api/v1/faq/import/progress/task-1 -H "X-API-Key: $API_KEY"
```

## Wiki（/api/v1/knowledgebase/:kb_id/wiki）

注意此组前缀为 `/knowledgebase/:kb_id/wiki`（单数，无连字符）。Handler: `internal/handler/wiki_page.go`。本组响应多为**原始对象**（不带 `success` 包装）。

### GET /api/v1/knowledgebase/:kb_id/wiki/pages

用途：Wiki 页面列表。

| 查询参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `page_type` | string | 否 | 逗号分隔类型 |
| `status` | string | 否 | 页面状态 |
| `query` | string | 否 | 全文搜索 |
| `category_path` | string | 否 | `/` 分隔路径过滤 |
| `folder_id` | string | 否 | 精确目录过滤（空串=根） |
| `category_depth` | int | 否 | 目录深度 |
| `page` / `page_size` | int | 否 | 分页（默认 1/20） |
| `sort_by` / `sort_order` | string | 否 | 排序字段：`title`、`created_at`、`updated_at`、`page_type`、`wiki_path`、`sort_order`、`depth`，其他值按 `updated_at`；默认 `updated_at` desc |

响应：200 `WikiPageListResponse`

```bash
curl "$BASE/api/v1/knowledgebase/kb-1/wiki/pages?page=1" -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/knowledgebase/:kb_id/wiki/pages

用途：创建页面。请求体（`types.WikiPage`）：`slug`、`title`、`content`、`folder_id`、`page_type` 等（均可选，slug 缺省自动生成）。

响应：201 `WikiPage`

```bash
curl -X POST $BASE/api/v1/knowledgebase/kb-1/wiki/pages -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"架构概览","content":"# 概览"}'
```

### PUT /api/v1/knowledgebase/:kb_id/wiki/move-page

用途：移动页面到目录。请求体：`{"slug":"<页面slug>","folder_id":"<目录ID|空=根>"}`（slug 必填）。

响应：200 `WikiPage`

```bash
curl -X PUT $BASE/api/v1/knowledgebase/kb-1/wiki/move-page -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"slug":"overview","folder_id":"f-1"}'
```

### GET /api/v1/knowledgebase/:kb_id/wiki/pages/*slug

用途：获取页面（`*slug` 为通配路径）。

响应：200 `WikiPageDetail`。保留 `WikiPage` 的所有字段，并增加 `in_link_titles`，将 `in_links` 中存在的页面 slug 映射为展示标题，例如 `{"concept/example":"示例标题"}`。没有可解析的反向链接时返回空对象；不存在的目标不出现在映射中。标题查询失败时返回 500，避免返回不完整的成功响应。

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/pages/overview -H "Authorization: Bearer $TOKEN"
```

### PUT /api/v1/knowledgebase/:kb_id/wiki/pages/*slug

用途：更新页面（请求体同创建）。旧版本会先整份快照进 `wiki_page_revisions`，`version` 递增，`last_edit_source` 记为 `user`（Agent 工具写入时为 `agent`）。

响应：200 `WikiPage`

```bash
curl -X PUT $BASE/api/v1/knowledgebase/kb-1/wiki/pages/overview -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"content":"# 更新后的概览"}'
```

### GET /api/v1/knowledgebase/:kb_id/wiki/revisions/*slug

用途：页面版本历史（migration `000075`）。权限：Viewer+ + KBAccessRead。

| 查询参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `version` | int | 否 | 传入时返回**该版本全文**（用于 diff），无效或 < 1 返回 400，找不到返回 404（升级前写入的版本或已被保留策略清理的快照都没有全文，属正常情况） |
| `limit` | int | 否 | 默认 50，上限 200；仅列表模式生效 |
| `offset` | int | 否 | 分页偏移 |

不带 `version` 时返回历史列表（版本号倒序、**不含正文**）加上页面当前版本号；每条含 `edit_source`（`pipeline` / `agent` / `user` / `revert`）、`editor_id`、`edited_at`。

历史保留是两级上限：软上限 50 版只裁剪 `pipeline` 与空来源的快照，硬上限 200 版对所有来源生效，因此人工编辑不会被管道刷掉。

```bash
# 历史列表
curl $BASE/api/v1/knowledgebase/kb-1/wiki/revisions/entity/acme-corp -H "Authorization: Bearer $TOKEN"
# 取第 3 版全文
curl "$BASE/api/v1/knowledgebase/kb-1/wiki/revisions/entity/acme-corp?version=3" -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/knowledgebase/:kb_id/wiki/revert

用途：把页面回滚到某个历史版本。权限：KB owner 或 Admin+ + KBAccessWrite。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `slug` | string | 是 | 目标页面 |
| `version` | int | 是 | 目标版本号（≥ 1） |

回滚**不会把版本号退回去**：目标版本的内容会作为一个新版本写入，`last_edit_source` 记为 `revert`，所以回滚也能被回滚。回滚到当前版本返回 400（一般是前端历史列表过期）。

响应：200 `WikiPage`

```bash
curl -X POST $BASE/api/v1/knowledgebase/kb-1/wiki/revert -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"slug":"entity/acme-corp","version":3}'
```

### DELETE /api/v1/knowledgebase/:kb_id/wiki/pages/*slug

用途：删除页面。

响应：204 No Content

```bash
curl -X DELETE $BASE/api/v1/knowledgebase/kb-1/wiki/pages/overview -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/folders

用途：目录列表。查询参数：`parent_id`（空=根）、`page_types`（逗号分隔）。

响应：200 `WikiFolderListResponse`

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/folders -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/knowledgebase/:kb_id/wiki/folders

用途：创建目录。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | 目录名 |
| `parent_id` | string | 否 | 父目录 |

响应：201 `WikiFolder`

```bash
curl -X POST $BASE/api/v1/knowledgebase/kb-1/wiki/folders -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"设计文档"}'
```

### PUT /api/v1/knowledgebase/:kb_id/wiki/folders/:folder_id

用途：重命名/移动目录。请求体：`name`、`parent_id`、`move_parent`（bool），均可选。

响应：200 `WikiFolder`

```bash
curl -X PUT $BASE/api/v1/knowledgebase/kb-1/wiki/folders/f-1 -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"架构设计"}'
```

### DELETE /api/v1/knowledgebase/:kb_id/wiki/folders/:folder_id

用途：删除目录。

响应：204 No Content

```bash
curl -X DELETE $BASE/api/v1/knowledgebase/kb-1/wiki/folders/f-1 -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/index

用途：Wiki 索引页（按类型分组窗口）。查询参数：`types`（逗号分隔）、`limit`（1-200，默认 50）、`cursor`（游标）。

响应：200 `WikiIndexResponse`

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/index -H "Authorization: Bearer $TOKEN"
```

::: warning 已移除
`GET /api/v1/knowledgebase/:kb_id/wiki/log`（Wiki 变更日志）已随 migration `000077_remove_wiki_log` 一并下线，`wiki_log_entries` 表被删除。Wiki 变更现在统一投影到知识库活动流，改用 `GET /api/v1/knowledge-bases/:id/activity`。
:::

### GET /api/v1/knowledgebase/:kb_id/wiki/graph

用途：页面关系图。

| 查询参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `mode` | string | 否 | `overview`（默认）/ `ego` |
| `center` | string | 否 | ego 模式中心 slug（ego 时必填） |
| `depth` | int | 否 | 1-3，默认 1 |
| `types` | string | 否 | page_type 过滤 |
| `limit` | int | 否 | 默认 500，上限 2000 |

响应：200 `WikiGraphData`

```bash
curl "$BASE/api/v1/knowledgebase/kb-1/wiki/graph?mode=overview" -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/stats

用途：Wiki 统计。

响应：200 `WikiStats`

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/stats -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/search

用途：页面搜索。查询参数：`q`（必填）、`limit`（默认 10）。

匹配语义随存储方言变化：

- Postgres：`q` 按 POSIX 正则（`~*`，大小写不敏感）匹配。
- SQLite / Lite：按 `LIKE` 字面子串匹配；`%` / `_` 会转义，不当通配符。

响应：200 `{"pages":[WikiPage]}`

```bash
curl "$BASE/api/v1/knowledgebase/kb-1/wiki/search?q=部署" -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/wiki-search

用途：跨知识库 Wiki 页面搜索（无会话、非混合检索）。Handler: `internal/handler/wiki_page.go` 的 `SearchPagesAcross`。算法与单库 `GET .../wiki/search` 相同（标题 > slug > summary > 正文），对多个库做一次查询后按 `match_rank` 全局截断。API key：`retrieve`/full。

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `query` | string | 是（`binding:"required"`） | 与单库 `q` 相同：Postgres 为 POSIX 正则（大小写不敏感）；SQLite / Lite 为字面 `LIKE` 子串 |
| `knowledge_base_ids` | []string | 条件必填 | 多库；与 `knowledge_base_id` 至少提供一个，最多 32 个 |
| `knowledge_base_id` | string | 否 | 单库兼容字段，合并进 ids |
| `limit` | int | 否 | 全局 top-N，默认 10，最大 50 |

任一目标库未开启 wiki → 400。库不存在或无读权限 → 404。API Key 白名单越权 → 403。非法正则（Postgres）→ 400。

响应：200 `{"success":true,"data":[WikiSearchHit]}`。命中只含导航字段 + `match_snippet`，不含正文；全文用 `GET /knowledgebase/:kb_id/wiki/pages/{slug}`。

| 响应字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 页面 ID |
| `knowledge_base_id` | string | 来源库，跨库时用来拼读页 URL |
| `slug` | string | 页面 slug |
| `title` | string | 标题 |
| `page_type` | string | 页面类型 |
| `aliases` | []string | 别名 |
| `summary` | string | 摘要 |
| `match_snippet` | string | 正文中首次命中附近约 60+命中+60 字；无正文命中时省略。snippet 仍用 Go 正则编译 `query`；SQLite 下 `query` 含正则元字符时库内可命中、snippet 可能为空 |

`content`、树/链接/元数据、时间戳不在搜索结果里。单库 `GET .../wiki/search` 仍返回完整 `WikiPage`。

```bash
curl -X POST $BASE/api/v1/wiki-search -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"query":"部署|发布","knowledge_base_ids":["kb-1","kb-2"],"limit":10}'
```

### POST /api/v1/knowledgebase/:kb_id/wiki/rebuild-links

用途：重建页面互链。写权限。无请求体。

响应：200 `{"message":"Links rebuilt successfully"}`

```bash
curl -X POST $BASE/api/v1/knowledgebase/kb-1/wiki/rebuild-links -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/lint

用途：Wiki 一致性检查报告。

响应：200 `WikiLintReport`

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/lint -H "Authorization: Bearer $TOKEN"
```

### POST /api/v1/knowledgebase/:kb_id/wiki/auto-fix

用途：自动修复 lint 问题。写权限。无请求体。

响应：200 `{"fixed":N,"message":"Auto-fixed N issues"}`

```bash
curl -X POST $BASE/api/v1/knowledgebase/kb-1/wiki/auto-fix -H "Authorization: Bearer $TOKEN"
```

### GET /api/v1/knowledgebase/:kb_id/wiki/issues

用途：问题列表。查询参数：`slug`（按页面过滤）、`status`（`pending/ignored/resolved`）。

响应：200 `[WikiPageIssue]`

```bash
curl $BASE/api/v1/knowledgebase/kb-1/wiki/issues -H "Authorization: Bearer $TOKEN"
```

### PUT /api/v1/knowledgebase/:kb_id/wiki/issues/:issue_id/status

用途：更新问题状态。写权限。请求体：`{"status":"pending|ignored|resolved"}`（`binding:"required"`）。`issue_id` 必须属于路径中的知识库，否则 404。

响应：200 `{"message":"Issue status updated successfully"}`

```bash
curl -X PUT $BASE/api/v1/knowledgebase/kb-1/wiki/issues/i-1/status -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"status":"resolved"}'
```

## 实现参考

路由注册：`internal/router/routes_knowledge.go` 的 `RegisterFAQRoutes` 与 `RegisterWikiPageRoutes`。Handler：`internal/handler/faq.go`、`internal/handler/wiki_page.go`。
