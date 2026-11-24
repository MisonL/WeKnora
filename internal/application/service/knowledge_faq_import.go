package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// UpsertFAQEntries imports or appends FAQ entries asynchronously.
// Returns task ID (UUID) for tracking import progress.
func (s *knowledgeService) UpsertFAQEntries(ctx context.Context,
	kbID string, payload *types.FAQBatchUpsertPayload,
) (string, error) {
	if payload == nil || len(payload.Entries) == 0 {
		return "", werrors.NewBadRequestError("FAQ 条目不能为空")
	}
	// The mode decides what an over-limit refusal tells the caller to do, so it
	// is settled before the cap is applied: checking the size first would word
	// the advice for an unknown mode even for an explicit replace request.
	if payload.Mode == "" {
		payload.Mode = types.FAQBatchModeAppend
	}
	if payload.Mode != types.FAQBatchModeAppend && payload.Mode != types.FAQBatchModeReplace {
		return "", werrors.NewBadRequestError("模式仅支持 append 或 replace")
	}

	// The HTTP handler already refuses an over-limit batch before decoding it;
	// this is the same bound for every other path into the import. It has to
	// hold here too because the work that follows scales with the entry count
	// and not with the request's bytes.
	if maxEntries := secutils.GetFAQImportMaxEntries(); len(payload.Entries) > maxEntries {
		// Worded by the same helper as the handler, so the two entry points
		// cannot disagree about what a replace caller should do: splitting
		// deletes everything outside each batch, and switching to append leaves
		// the old entries in place next to the new ones.
		return "", werrors.NewBadRequestError(
			secutils.FAQTooManyEntriesMessage(maxEntries, len(payload.Entries), payload.Mode, true))
	}

	// The caller's own tenant, taken before writableFAQKnowledgeBase switches
	// the context to the knowledge base's owner: importing into a shared KB
	// runs as the owner, but the task id is what the progress route checks
	// against the caller's tenant, and minting it with the owner's would make
	// the initiator's own poll a 404.
	callerTenantID, _ := types.TenantIDFromContext(ctx)

	// 验证知识库是否存在且有效
	kb, ctx, err := s.writableFAQKnowledgeBase(ctx, kbID)
	if err != nil {
		return "", err
	}

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// 使用传入的TaskID，如果没传则生成增强的TaskID。
	// 客户端传入的 task_id 会进文件名和 Redis key，必须是无路径分隔符的标识符。
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		taskID = secutils.GenerateTaskID("faq_import", faqImportTaskTenant(callerTenantID, tenantID), kbID)
	} else if err := secutils.ValidateTaskID(taskID); err != nil {
		return "", werrors.NewBadRequestError("task_id 格式不合法")
	}

	// 记录任务入队时间
	// The millisecond value breaks ties inside one second: two imports can
	// legitimately finish and be replaced within a second, and a second-
	// resolution comparison cannot tell that apart from a same-second
	// completion of the task being retried.
	enqueuedAtTime := time.Now()
	enqueuedAt := enqueuedAtTime.Unix()
	enqueuedAtMillis := enqueuedAtTime.UnixMilli()
	instanceID := uuid.NewString()
	runningInfoSet := false
	enqueueSucceeded := false

	// Claim the per-KB lease before anything is queued or created. This is one
	// atomic operation, so two concurrent submissions cannot both be admitted
	// the way the previous GET-then-SET check allowed. It also runs before the
	// FAQ container row is ensured: that step is a find-then-create with no
	// unique constraint, so two concurrent submissions could both insert one.
	// A Redis failure refuses the submission instead of proceeding: admitting
	// an import while the guard is unreachable is exactly the case the guard
	// exists for.
	if err := s.claimFAQImportLease(ctx, kbID, &runningFAQImportInfo{
		TaskID:     taskID,
		EnqueuedAt: enqueuedAt,
		InstanceID: instanceID,
	}); err != nil {
		var held *faqImportLeaseHeldError
		if errors.As(err, &held) {
			logger.Warnf(ctx, "Import task already running for KB %s: %s", kbID, held.TaskID)
			if held.TaskID == "" {
				// The marker exists but its holder could not be read (or
				// predates the JSON format); refuse without an empty id.
				return "", werrors.NewBadRequestError("该知识库已有导入任务正在进行中，请等待完成后再试")
			}
			return "", werrors.NewBadRequestError(fmt.Sprintf("该知识库已有导入任务正在进行中（任务ID: %s），请等待完成后再试", held.TaskID))
		}
		logger.Errorf(ctx, "Failed to claim FAQ import lease for KB %s: %v", kbID, err)
		return "", werrors.NewInternalServerError("暂时无法确认该知识库的导入状态，请稍后重试")
	}
	runningInfoSet = true
	defer func() {
		if !runningInfoSet || enqueueSucceeded {
			return
		}
		if clearErr := s.clearRunningFAQImportInfoIfMatches(context.WithoutCancel(ctx), kbID, taskID, instanceID, enqueuedAt); clearErr != nil {
			logger.Warnf(ctx, "Failed to clear FAQ import running info after setup failure: %v", clearErr)
		}
	}()

	if err := s.validateFAQImportTags(ctx, kb, payload.Entries); err != nil {
		return "", err
	}

	var knowledgeID string

	// 确保 FAQ knowledge 存在（持有租约后执行，同一 KB 只会有一个提交在建容器）
	faqKnowledge, err := s.ensureFAQKnowledge(ctx, tenantID, kb)
	if err != nil {
		return "", fmt.Errorf("failed to ensure FAQ knowledge: %w", err)
	}
	knowledgeID = faqKnowledge.ID

	// 初始化导入任务状态到Redis
	progress := &types.FAQImportProgress{
		TaskID:        taskID,
		KBID:          kbID,
		KnowledgeID:   knowledgeID,
		TenantID:      tenantID,
		InstanceID:    instanceID,
		EnqueuedAt:    enqueuedAt,
		Status:        types.FAQImportStatusPending,
		Progress:      0,
		Total:         len(payload.Entries),
		Processed:     0,
		SuccessCount:  0,
		FailedCount:   0,
		FailedEntries: make([]types.FAQFailedEntry, 0),
		Message:       "任务已创建，等待处理",
		CreatedAt:     time.Now().Unix(),
		UpdatedAt:     time.Now().Unix(),
		DryRun:        payload.DryRun,
	}
	if err := s.saveFAQImportProgress(ctx, progress); err != nil {
		logger.Errorf(ctx, "Failed to initialize FAQ import task status: %v", err)
		return "", fmt.Errorf("failed to initialize task: %w", err)
	}

	logger.Infof(ctx, "FAQ import task initialized: %s, kb_id: %s, total entries: %d, dry_run: %v",
		taskID, kbID, len(payload.Entries), payload.DryRun)

	// Enqueue FAQ import task to Asynq
	logger.Info(ctx, "Enqueuing FAQ import task to Asynq")

	// 构建任务 payload
	taskPayload := types.FAQImportPayload{
		TenantID:         tenantID,
		TaskID:           taskID,
		KBID:             kbID,
		KnowledgeID:      knowledgeID,
		Mode:             payload.Mode,
		DryRun:           payload.DryRun,
		EnqueuedAt:       enqueuedAt,
		EnqueuedAtMillis: enqueuedAtMillis,
		InstanceID:       instanceID,
		Initiator:        types.TaskInitiatorFromContext(ctx),
	}
	// An entries object uploaded before a failed enqueue is unreachable: no
	// task will ever read it, and its per-instance name means deleting here
	// cannot touch another submission's file.
	defer func() {
		if enqueueSucceeded || taskPayload.EntriesURL == "" || s.fileSvc == nil {
			return
		}
		if delErr := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), taskPayload.EntriesURL); delErr != nil {
			logger.Warnf(ctx, "Failed to clean up FAQ entries object after setup failure: %v", delErr)
		}
	}()

	// 阈值：超过 200 条或序列化后超过 50KB 时使用对象存储
	const (
		entryCountThreshold  = 200
		payloadSizeThreshold = 50 * 1024 // 50KB
	)

	entryCount := len(payload.Entries)
	if entryCount > entryCountThreshold {
		// 数据量较大，上传到对象存储
		entriesData, err := json.Marshal(payload.Entries)
		if err != nil {
			logger.Errorf(ctx, "Failed to marshal FAQ entries: %v", err)
			return "", fmt.Errorf("failed to marshal entries: %w", err)
		}

		logger.Infof(ctx, "FAQ entries size: %d bytes, uploading to object storage", len(entriesData))

		// 上传到私有桶（主桶），任务处理完成后清理
		fileName, err := faqImportEntriesFileName(taskID, enqueuedAt, instanceID)
		if err != nil {
			return "", fmt.Errorf("invalid task id for object name: %w", err)
		}
		entriesURL, err := s.fileSvc.SaveBytes(ctx, entriesData, tenantID, fileName, false)
		if err != nil {
			logger.Errorf(ctx, "Failed to upload FAQ entries to object storage: %v", err)
			return "", fmt.Errorf("failed to upload entries: %w", err)
		}

		logger.Infof(ctx, "FAQ entries uploaded to: %s", entriesURL)
		taskPayload.EntriesURL = entriesURL
		taskPayload.EntryCount = entryCount
	} else {
		// 数据量较小，直接存储在 payload 中
		taskPayload.Entries = payload.Entries
	}

	langfuse.InjectTracing(ctx, &taskPayload)
	payloadBytes, err := json.Marshal(taskPayload)
	if err != nil {
		logger.Errorf(ctx, "Failed to marshal FAQ import task payload: %v", err)
		return "", fmt.Errorf("failed to marshal task payload: %w", err)
	}

	// 再次检查 payload 大小
	if len(payloadBytes) > payloadSizeThreshold && taskPayload.EntriesURL == "" {
		// payload 太大但还没上传，现在上传
		entriesData, _ := json.Marshal(payload.Entries)
		fileName, nameErr := faqImportEntriesFileName(taskID, enqueuedAt, instanceID)
		if nameErr != nil {
			return "", fmt.Errorf("invalid task id for object name: %w", nameErr)
		}
		entriesURL, err := s.fileSvc.SaveBytes(ctx, entriesData, tenantID, fileName, false)
		if err != nil {
			logger.Errorf(ctx, "Failed to upload FAQ entries to object storage: %v", err)
			return "", fmt.Errorf("failed to upload entries: %w", err)
		}

		logger.Infof(ctx, "FAQ entries uploaded to (size exceeded): %s", entriesURL)
		taskPayload.Entries = nil
		taskPayload.EntriesURL = entriesURL
		taskPayload.EntryCount = entryCount

		payloadBytes, _ = json.Marshal(taskPayload)
	}

	logger.Infof(ctx, "FAQ import task payload size: %d bytes", len(payloadBytes))

	maxRetry := 5
	if payload.DryRun {
		maxRetry = 3 // dry run 重试次数少一些
	}

	// 使用 taskID:instanceID 作为 asynq 的唯一任务标识
	asynqTaskID := fmt.Sprintf("%s:%s", taskID, instanceID)

	task := asynq.NewTask(
		types.TypeFAQImport,
		payloadBytes,
		asynq.TaskID(asynqTaskID),
		asynq.Queue(types.QueueMaintenance),
		asynq.MaxRetry(maxRetry),
		asynq.Timeout(2*time.Hour),
	)
	info, err := s.task.Enqueue(task)
	if err != nil {
		logger.Errorf(ctx, "Failed to enqueue FAQ import task: %v", err)
		return "", fmt.Errorf("failed to enqueue task: %w", err)
	}
	logger.Infof(ctx, "Enqueued FAQ import task: id=%s queue=%s task_id=%s dry_run=%v", info.ID, info.Queue, taskID, payload.DryRun)
	enqueueSucceeded = true

	if !payload.DryRun {
		recordKBActivity(ctx, s.audit, tenantID, kbID, types.AuditActionFAQImportStarted,
			"faq_entry", knowledgeID, types.AuditOutcomeAccepted,
			map[string]any{
				"task_id": taskID, "mode": payload.Mode, "total": len(payload.Entries),
				"trigger": kbActivityTrigger(ctx), "processing_status": "pending",
			})
	}

	return taskID, nil
}

// faqImportEntriesFileName names the object that carries a large import's
// entries. The instance id is part of the name: two submissions may reuse a
// task id within the same second (for example when a lost marker lets both
// through), and a shared object name would let one refusal delete the file the
// other is about to read.
func faqImportEntriesFileName(taskID string, enqueuedAt int64, instanceID string) (string, error) {
	return secutils.SafeFileName(fmt.Sprintf("faq_import_entries_%s_%d_%s.json", taskID, enqueuedAt, instanceID))
}

// generateFailedEntriesCSV 生成失败条目的 CSV 文件并上传
func (s *knowledgeService) generateFailedEntriesCSV(ctx context.Context,
	tenantID uint64, taskID string, failedEntries []types.FAQFailedEntry,
) (string, error) {
	// 生成 CSV 内容
	var buf strings.Builder

	// 写入 BOM 以支持 Excel 正确识别 UTF-8
	buf.WriteString("\xEF\xBB\xBF")

	// 写入表头
	buf.WriteString("错误原因,分类(必填),问题(必填),相似问题(选填-多个用##分隔),反例问题(选填-多个用##分隔),机器人回答(必填-多个用##分隔),是否全部回复(选填-默认FALSE),是否停用(选填-默认FALSE)\n")

	// 写入数据行
	for _, entry := range failedEntries {
		// The same escaping as the export endpoint: a failing entry's question is
		// user text, so a leading '=' would make this download a formula in Excel
		// (CWE-1236). The web importer strips the marker again, so a fixed file
		// re-uploaded there restores the original text.
		reason := escapeCSVField(entry.Reason)
		tagName := escapeCSVField(entry.TagName)
		standardQ := escapeCSVField(entry.StandardQuestion)
		similarQs := ""
		if len(entry.SimilarQuestions) > 0 {
			similarQs = escapeCSVField(strings.Join(entry.SimilarQuestions, "##"))
		}
		negativeQs := ""
		if len(entry.NegativeQuestions) > 0 {
			negativeQs = escapeCSVField(strings.Join(entry.NegativeQuestions, "##"))
		}
		answers := ""
		if len(entry.Answers) > 0 {
			answers = escapeCSVField(strings.Join(entry.Answers, "##"))
		}
		answerAll := "false"
		if entry.AnswerAll {
			answerAll = "true"
		}
		isDisabled := "false"
		if entry.IsDisabled {
			isDisabled = "true"
		}

		buf.WriteString(fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%s\n",
			reason, tagName, standardQ, similarQs, negativeQs, answers, answerAll, isDisabled))
	}

	// 上传 CSV 文件到临时存储（会自动过期）
	fileName, err := secutils.SafeFileName(fmt.Sprintf("faq_dryrun_failed_%s.csv", taskID))
	if err != nil {
		return "", fmt.Errorf("invalid task id for object name: %w", err)
	}
	filePath, err := s.fileSvc.SaveBytes(ctx, []byte(buf.String()), tenantID, fileName, true)
	if err != nil {
		return "", fmt.Errorf("failed to save CSV file: %w", err)
	}

	// Return the object path, not a presigned URL. A presigned URL embeds the
	// STS security token, is written to logs and persisted in the knowledge
	// JSON column, and expires (at most the token TTL) with no way to re-sign
	// later. The frontend downloads through the authenticated /files proxy,
	// which resolves the current storage backend on every request.
	logger.Infof(ctx, "Generated failed entries CSV at object path: %s, entries: %d", filePath, len(failedEntries))
	return filePath, nil
}

// saveFAQImportResultToDatabase 保存FAQ导入结果统计到数据库
func (s *knowledgeService) saveFAQImportResultToDatabase(ctx context.Context,
	payload *types.FAQImportPayload, progress *types.FAQImportProgress, originalTotalEntries int,
) error {
	// 获取FAQ知识库实例
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, payload.KnowledgeID)
	if err != nil {
		return fmt.Errorf("failed to get FAQ knowledge: %w", err)
	}

	// 计算跳过的条目数（总数 - 完全成功 - 部分失败 - 完全失败）
	skippedCount := originalTotalEntries - progress.SuccessCount - progress.PartialFailedCount - progress.FailedCount
	if skippedCount < 0 {
		skippedCount = 0
	}

	// 创建导入结果统计
	importedAt := time.Now()
	importResult := &types.FAQImportResult{
		TotalEntries:       originalTotalEntries,
		SuccessCount:       progress.SuccessCount,
		FailedCount:        progress.FailedCount,
		PartialFailedCount: progress.PartialFailedCount,
		SkippedCount:       skippedCount,
		MergedCount:        progress.MergedCount,
		AddedCount:         progress.AddedCount,
		ImportMode:         payload.Mode,
		ImportedAt:         importedAt,
		ImportedAtMillis:   importedAt.UnixMilli(),
		TaskID:             payload.TaskID,
		InstanceID:         payload.InstanceID,
		ProcessingTime:     time.Now().Unix() - progress.CreatedAt, // 处理耗时（秒）
		DisplayStatus:      "open",                                 // 新导入的结果默认显示
	}

	// 如果有失败/部分失败条目 CSV 下载 URL，则写入结果（FailedEntries 包含完全失败与部分失败）
	if progress.FailedEntriesURL != "" {
		importResult.FailedEntriesURL = progress.FailedEntriesURL
	}

	// 设置导入结果到Knowledge的metadata中
	if err := knowledge.SetLastFAQImportResult(importResult); err != nil {
		return fmt.Errorf("failed to set FAQ import result: %w", err)
	}

	// 更新数据库
	if err := s.repo.UpdateKnowledge(ctx, knowledge); err != nil {
		return fmt.Errorf("failed to update knowledge with import result: %w", err)
	}

	logger.Infof(ctx, "Saved FAQ import result to database: knowledge_id=%s, task_id=%s, total=%d, success=%d, added=%d, merged=%d, failed=%d, partial_failed=%d, skipped=%d",
		payload.KnowledgeID, payload.TaskID, originalTotalEntries, progress.SuccessCount, progress.AddedCount, progress.MergedCount, progress.FailedCount, progress.PartialFailedCount, skippedCount)

	return nil
}

// buildFAQFailedEntry 构建 FAQFailedEntry
func buildFAQFailedEntry(idx int, reason string, entry *types.FAQEntryPayload) types.FAQFailedEntry {
	answerAll := false
	if entry.AnswerStrategy != nil && *entry.AnswerStrategy == types.AnswerStrategyAll {
		answerAll = true
	}
	isDisabled := false
	if entry.IsEnabled != nil && !*entry.IsEnabled {
		isDisabled = true
	}
	return types.FAQFailedEntry{
		Index:             idx,
		Reason:            reason,
		TagName:           entry.TagName,
		StandardQuestion:  strings.TrimSpace(entry.StandardQuestion),
		SimilarQuestions:  entry.SimilarQuestions,
		NegativeQuestions: entry.NegativeQuestions,
		Answers:           entry.Answers,
		AnswerAll:         answerAll,
		IsDisabled:        isDisabled,
	}
}

func buildFAQPartialFailedEntry(idx int, entry *types.FAQEntryPayload,
	removedSimilarQuestions, removedNegativeQuestions []string,
) types.FAQFailedEntry {
	answerAll := false
	if entry.AnswerStrategy != nil && *entry.AnswerStrategy == types.AnswerStrategyAll {
		answerAll = true
	}
	isDisabled := false
	if entry.IsEnabled != nil && !*entry.IsEnabled {
		isDisabled = true
	}

	// 构建失败原因描述：汇总信息 + 详细信息
	var summary []string
	if len(removedSimilarQuestions) > 0 {
		summary = append(summary, fmt.Sprintf("%d条相似问被移除", len(removedSimilarQuestions)))
	}
	if len(removedNegativeQuestions) > 0 {
		summary = append(summary, fmt.Sprintf("%d条反例被移除", len(removedNegativeQuestions)))
	}

	// 完整的 reason：汇总 | 相似问详情 | 反例详情
	var reasonParts []string
	reasonParts = append(reasonParts, "部分成功："+strings.Join(summary, "，"))
	if len(removedSimilarQuestions) > 0 {
		reasonParts = append(reasonParts, strings.Join(removedSimilarQuestions, "; "))
	}
	if len(removedNegativeQuestions) > 0 {
		reasonParts = append(reasonParts, strings.Join(removedNegativeQuestions, "; "))
	}

	return types.FAQFailedEntry{
		Index:                    idx,
		Reason:                   strings.Join(reasonParts, " | "),
		IsPartialFailure:         true,
		TagName:                  entry.TagName,
		StandardQuestion:         strings.TrimSpace(entry.StandardQuestion),
		SimilarQuestions:         entry.SimilarQuestions,
		NegativeQuestions:        entry.NegativeQuestions,
		Answers:                  entry.Answers,
		AnswerAll:                answerAll,
		IsDisabled:               isDisabled,
		RemovedSimilarQuestions:  removedSimilarQuestions,
		RemovedNegativeQuestions: removedNegativeQuestions,
	}
}

// executeFAQDryRunValidation 执行 FAQ dry run 验证，返回通过验证的条目索引
func (s *knowledgeService) executeFAQDryRunValidation(ctx context.Context,
	payload *types.FAQImportPayload, progress *types.FAQImportProgress,
) []int {
	entries := payload.Entries

	// 用于记录已通过基本验证和重复检查的条目索引，后续进行安全检查
	validEntryIndices := make([]int, 0, len(entries))

	// 根据模式选择不同的验证逻辑
	if payload.Mode == types.FAQBatchModeAppend {
		validEntryIndices = s.validateEntriesForAppendModeWithProgress(ctx, payload.TenantID, payload.KBID, entries, progress)
	} else {
		validEntryIndices = s.validateEntriesForReplaceModeWithProgress(ctx, entries, progress)
	}

	return validEntryIndices
}

// validateEntriesForAppendModeWithProgress 验证 Append 模式下的条目（带进度更新）
// 注意：验证阶段不更新 Processed，只有实际导入时才更新
// validateEntriesForAppendModeWithProgress 验证 Append 模式下的条目（带进度更新）
// 注意：验证阶段不更新 Processed，只有实际导入时才更新
//
// 分四阶段校验：
// 阶段一（预校验）：
//  1. 标准问 - 仅文件内部去重；如已有KB标准问则标记为合并候选
//  2. 相似问 - 对比文件+知识库（合并候选排除自身已有问题） → 单条移除冲突相似问
//  3. 反例   - 仅对比当前QA自身标准问+相似问 → 单条移除冲突反例
//
// 阶段二（后校验，仅合并候选）：
//  4. 对合并后的完整数据重跑反例校验 → 冲突则整条回退到合并前状态
func (s *knowledgeService) validateEntriesForAppendModeWithProgress(ctx context.Context,
	tenantID uint64, kbID string, entries []types.FAQEntryPayload, progress *types.FAQImportProgress,
) []int {
	totalEntries := len(entries)

	// 查询知识库中已有的所有 FAQ chunks 的 metadata
	existingChunks, err := s.chunkRepo.ListAllFAQChunksWithMetadataByKnowledgeBaseID(ctx, tenantID, kbID)
	if err != nil {
		logger.Warnf(ctx, "Failed to list existing FAQ chunks for dry run: %v", err)
	}

	// 构建已存在的标准问 → chunk 映射（用于合并候选识别）
	existingStdQToChunk := make(map[string]*types.Chunk)
	// 构建已存在的所有问题 → 所属 chunkID 映射（用于相似问冲突检测）
	existingQuestionToChunkID := make(map[string]string)
	// 构建每个 chunk 拥有的问题集合（用于合并时排除自身）
	existingChunkQuestions := make(map[string]map[string]bool)
	// 构建 chunkID → 标准问 映射（用于冲突失败原因展示）
	existingChunkIDToStdQ := make(map[string]string)

	for _, chunk := range existingChunks {
		meta, err := chunk.FAQMetadata()
		if err != nil || meta == nil {
			continue
		}
		qs := make(map[string]bool)
		if meta.StandardQuestion != "" {
			existingStdQToChunk[meta.StandardQuestion] = chunk
			existingQuestionToChunkID[meta.StandardQuestion] = chunk.ID
			qs[meta.StandardQuestion] = true
		}
		for _, q := range meta.SimilarQuestions {
			if q != "" {
				existingQuestionToChunkID[q] = chunk.ID
				qs[q] = true
			}
		}
		existingChunkQuestions[chunk.ID] = qs
		existingChunkIDToStdQ[chunk.ID] = meta.StandardQuestion
	}

	// 合并候选跟踪：entry index → 目标已有 chunk
	mergeChunkMap := make(map[int]*types.Chunk)

	// ==================== 第一次迭代：基本格式验证 + 文件内标准问去重 + 合并候选识别 ====================
	batchStandardQuestions := make(map[string]int) // value 为首次出现的索引
	validIndicesAfterStdQ := make([]int, 0, totalEntries)

	for i, entry := range entries {
		if err := validateFAQEntryPayloadBasic(&entry); err != nil {
			progress.FailedCount++
			fe := buildFAQFailedEntry(i, err.Error(), &entry)
			fe.FailureType = "pre_validation"
			progress.FailedEntries = append(progress.FailedEntries, fe)
			continue
		}

		standardQ := strings.TrimSpace(entry.StandardQuestion)

		// 文件内标准问去重
		if firstIdx, exists := batchStandardQuestions[standardQ]; exists {
			progress.FailedCount++
			fe := buildFAQFailedEntry(i, fmt.Sprintf("标准问冲突：与批次内第 %d 条标准问重复", firstIdx+1), &entry)
			fe.FailureType = "pre_validation"
			progress.FailedEntries = append(progress.FailedEntries, fe)
			continue
		}

		// 判断是否为合并候选
		if chunk, exists := existingStdQToChunk[standardQ]; exists {
			// 标准问在 KB 中已存在 → 标记为合并候选
			mergeChunkMap[i] = chunk
			logger.Infof(ctx, "FAQ entry %d: standard question '%s' exists in KB, marking as merge candidate (chunk_id=%s)", i, standardQ, chunk.ID)
		} else if conflictChunkID, hit := existingQuestionToChunkID[standardQ]; hit {
			// 标准问未与任何已有标准问重复，但撞上了 KB 中其他条目的相似问
			// → 前置校验失败，避免下游 calculateAppendOperations 静默丢弃导致统计失真
			conflictStdQ := existingChunkIDToStdQ[conflictChunkID]
			progress.FailedCount++
			fe := buildFAQFailedEntry(i,
				fmt.Sprintf(`标准问冲突：与知识库中标准问"%s"的相似问"%s"重复`, conflictStdQ, standardQ),
				&entry)
			fe.FailureType = "pre_validation"
			progress.FailedEntries = append(progress.FailedEntries, fe)
			logger.Infof(ctx,
				"FAQ entry %d: standard question '%s' conflicts with existing similar question of chunk_id=%s (std='%s')",
				i, standardQ, conflictChunkID, conflictStdQ)
			continue
		}

		batchStandardQuestions[standardQ] = i
		validIndicesAfterStdQ = append(validIndicesAfterStdQ, i)

		if (i+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证标准问 %d/%d...", i+1, totalEntries)
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// ==================== 第二次迭代：相似问冲突检测 ====================
	// 构建批次内所有标准问 + 相似问的集合
	batchAllQuestions := make(map[string]int)
	for _, i := range validIndicesAfterStdQ {
		standardQ := strings.TrimSpace(entries[i].StandardQuestion)
		batchAllQuestions[standardQ] = i
		for _, q := range entries[i].SimilarQuestions {
			q = strings.TrimSpace(q)
			if q != "" {
				if _, exists := batchAllQuestions[q]; !exists {
					batchAllQuestions[q] = i
				}
			}
		}
	}

	removedSimilarQuestionsMap := make(map[int][]string)
	removedNegativeQuestionsMap := make(map[int][]string)

	for idx, i := range validIndicesAfterStdQ {
		entry := &entries[i]
		standardQ := strings.TrimSpace(entry.StandardQuestion)

		// 合并候选：获取目标 chunk 自身的问题集合，用于排除
		var ownChunkQuestions map[string]bool
		if mergeChunk, isMerge := mergeChunkMap[i]; isMerge {
			ownChunkQuestions = existingChunkQuestions[mergeChunk.ID]
		}

		validSimilarQuestions := make([]string, 0, len(entry.SimilarQuestions))
		var removedSimilarQuestions []string
		for _, q := range entry.SimilarQuestions {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			// 相似问不能与自己的标准问相同
			if q == standardQ {
				removedSimilarQuestions = append(removedSimilarQuestions, fmt.Sprintf(`「相似问冲突」："%s"与本条"标准问"冲突`, q))
				continue
			}
			// 相似问校验：对比知识库已有标准问 + 相似问
			if _, exists := existingQuestionToChunkID[q]; exists {
				// 合并候选：如果问题属于合并目标自身的 chunk，允许（去重合并时会处理）
				if ownChunkQuestions != nil && ownChunkQuestions[q] {
					validSimilarQuestions = append(validSimilarQuestions, q)
					continue
				}
				removedSimilarQuestions = append(removedSimilarQuestions, fmt.Sprintf(`「相似问冲突」："%s"与知识库已有"标准问/相似问"冲突`, q))
				continue
			}
			// 相似问校验：对比批次内标准问 + 相似问
			if firstIdx, exists := batchAllQuestions[q]; exists && firstIdx != i {
				removedSimilarQuestions = append(removedSimilarQuestions, fmt.Sprintf(`「相似问冲突」："%s"与第 %d 行"标准问/相似问"冲突`, q, firstIdx+1))
				continue
			}
			validSimilarQuestions = append(validSimilarQuestions, q)
		}
		entries[i].SimilarQuestions = validSimilarQuestions

		if len(removedSimilarQuestions) > 0 {
			removedSimilarQuestionsMap[i] = removedSimilarQuestions
		}

		if (idx+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证相似问 %d/%d...", idx+1, len(validIndicesAfterStdQ))
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// ==================== 第三次迭代：反例冲突检测（预校验，仅检查新条目自身数据） ====================
	for idx, i := range validIndicesAfterStdQ {
		entry := &entries[i]
		standardQ := strings.TrimSpace(entry.StandardQuestion)

		currentQAQuestions := make(map[string]bool)
		currentQAQuestions[standardQ] = true
		for _, q := range entry.SimilarQuestions {
			currentQAQuestions[q] = true
		}

		validNegativeQuestions := make([]string, 0, len(entry.NegativeQuestions))
		var removedNegativeQuestions []string
		for _, q := range entry.NegativeQuestions {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			if currentQAQuestions[q] {
				removedNegativeQuestions = append(removedNegativeQuestions, fmt.Sprintf(`「反例冲突」："%s"与本条"标准问/相似问"冲突`, q))
				continue
			}
			validNegativeQuestions = append(validNegativeQuestions, q)
		}
		entries[i].NegativeQuestions = validNegativeQuestions

		if len(removedNegativeQuestions) > 0 {
			removedNegativeQuestionsMap[i] = removedNegativeQuestions
		}

		if (idx+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证反例 %d/%d...", idx+1, len(validIndicesAfterStdQ))
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// ==================== 第四次迭代：后校验（仅合并候选） ====================
	// 对合并后的完整数据重跑反例校验，冲突则整条回退到合并前状态
	postValidationFailed := make(map[int]bool)
	mergeCount := 0
	for _, i := range validIndicesAfterStdQ {
		mergeChunk, isMerge := mergeChunkMap[i]
		if !isMerge {
			continue
		}

		existingMeta, err := mergeChunk.FAQMetadata()
		if err != nil || existingMeta == nil {
			logger.Warnf(ctx, "FAQ entry %d: failed to get merge target metadata, skipping post-validation", i)
			continue
		}

		entry := &entries[i]

		// 计算合并后的完整数据
		mergedSimilar := unionStrings(existingMeta.SimilarQuestions, entry.SimilarQuestions)
		mergedNegative := unionStrings(existingMeta.NegativeQuestions, entry.NegativeQuestions)

		// 构建合并后的冲突集合（标准问 + 所有合并后的相似问）
		mergedPositiveSet := make(map[string]bool)
		mergedPositiveSet[existingMeta.StandardQuestion] = true
		for _, q := range mergedSimilar {
			mergedPositiveSet[q] = true
		}

		// 检查每个合并后的反例是否与合并后的标准问 / 相似问冲突
		var conflictingNegatives []string
		for _, q := range mergedNegative {
			if mergedPositiveSet[q] {
				conflictingNegatives = append(conflictingNegatives, q)
			}
		}

		if len(conflictingNegatives) > 0 {
			// 后校验失败 → 整条回退到合并前状态
			postValidationFailed[i] = true
			delete(mergeChunkMap, i)
			progress.FailedCount++
			reason := fmt.Sprintf("后校验失败：合并后反例「%s」与相似问冲突", strings.Join(conflictingNegatives, "、"))
			fe := buildFAQFailedEntry(i, reason, entry)
			fe.FailureType = "post_validation"
			progress.FailedEntries = append(progress.FailedEntries, fe)
			logger.Infof(ctx, "FAQ entry %d: post-validation failed, conflicting negatives after merge: %v", i, conflictingNegatives)
		} else if shapeErr := secutils.CheckFAQEntryShape(
			existingMeta.StandardQuestion, mergedSimilar, mergedNegative, entry.Answers,
		); shapeErr != nil {
			// 并集本身可能把两边各自合规的条目顶出上限：相似问数量翻倍、
			// 文本合计超字节数，都会在下游按相似问倍数膨胀索引文本。
			postValidationFailed[i] = true
			delete(mergeChunkMap, i)
			progress.FailedCount++
			fe := buildFAQFailedEntry(i, "后校验失败：合并后超出单条上限："+shapeErr.Error(), entry)
			fe.FailureType = "post_validation"
			progress.FailedEntries = append(progress.FailedEntries, fe)
			logger.Infof(ctx, "FAQ entry %d: post-validation failed, merged entry too large: %v", i, shapeErr)
		} else {
			mergeCount++
		}
	}

	// 从有效索引中移除后校验失败的条目
	if len(postValidationFailed) > 0 {
		filtered := make([]int, 0, len(validIndicesAfterStdQ))
		for _, i := range validIndicesAfterStdQ {
			if !postValidationFailed[i] {
				filtered = append(filtered, i)
			}
		}
		validIndicesAfterStdQ = filtered
	}

	// 将部分失败信息添加到 FailedEntries
	for _, i := range validIndicesAfterStdQ {
		removedSimilar := removedSimilarQuestionsMap[i]
		removedNegative := removedNegativeQuestionsMap[i]
		if len(removedSimilar) > 0 || len(removedNegative) > 0 {
			pf := buildFAQPartialFailedEntry(i, &entries[i], removedSimilar, removedNegative)
			progress.FailedEntries = append(progress.FailedEntries, pf)
			progress.PartialFailedCount++
		}
	}

	// 记录合并条目索引到 progress（用于执行阶段和重试）
	mergeIndices := make([]int, 0, mergeCount)
	for _, i := range validIndicesAfterStdQ {
		if _, isMerge := mergeChunkMap[i]; isMerge {
			mergeIndices = append(mergeIndices, i)
		}
	}
	progress.MergeEntryIndices = mergeIndices

	logger.Infof(ctx, "Append mode validation completed: total=%d, valid=%d, merge_candidates=%d, failed=%d, partial_failed=%d",
		totalEntries, len(validIndicesAfterStdQ), mergeCount, progress.FailedCount, progress.PartialFailedCount)

	return validIndicesAfterStdQ
}

// validateEntriesForReplaceModeWithProgress 验证 Replace 模式下的条目（带进度更新）
// 注意：验证阶段不更新 Processed，只有实际导入时才更新
// 分三次迭代校验，确保前面过滤掉的数据不会被后面考虑：
// 1. 标准问 - 对比所有标准问 → 整条QA失败「标准问冲突」
// 2. 相似问 - 对比所有标准问+相似问 → 单条问法失败「相似问冲突」（仅移除冲突的相似问）
// 3. 反例 - 对比当前QA下所有标准问+相似问 → 单条问法失败「反例冲突」（仅移除冲突的反例）
func (s *knowledgeService) validateEntriesForReplaceModeWithProgress(ctx context.Context,
	entries []types.FAQEntryPayload, progress *types.FAQImportProgress,
) []int {
	totalEntries := len(entries)

	// ==================== 第一次迭代：基本格式验证 + 标准问冲突检测 ====================
	// 标准问冲突会导致整条QA失败
	batchStandardQuestions := make(map[string]int) // value 为首次出现的索引
	validIndicesAfterStdQ := make([]int, 0, totalEntries)

	for i, entry := range entries {
		// 验证条目基本格式
		if err := validateFAQEntryPayloadBasic(&entry); err != nil {
			progress.FailedCount++
			progress.FailedEntries = append(progress.FailedEntries, buildFAQFailedEntry(i, err.Error(), &entry))
			continue
		}

		standardQ := strings.TrimSpace(entry.StandardQuestion)

		// 标准问校验：对比所有标准问 → 整条QA失败「标准问冲突」
		if firstIdx, exists := batchStandardQuestions[standardQ]; exists {
			progress.FailedCount++
			progress.FailedEntries = append(progress.FailedEntries, buildFAQFailedEntry(i, fmt.Sprintf("标准问冲突：与批次内第 %d 条标准问重复", firstIdx+1), &entry))
			continue
		}

		// 记录标准问
		batchStandardQuestions[standardQ] = i
		validIndicesAfterStdQ = append(validIndicesAfterStdQ, i)

		// 定期更新进度消息
		if (i+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证标准问 %d/%d...", i+1, totalEntries)
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// ==================== 第二次迭代：相似问冲突检测 ====================
	// 只处理通过第一次校验的条目，相似问冲突只移除冲突的相似问
	// 构建所有标准问+相似问的集合（只包含通过第一次校验的条目）
	batchAllQuestions := make(map[string]int) // value 为首次出现的索引
	for _, i := range validIndicesAfterStdQ {
		standardQ := strings.TrimSpace(entries[i].StandardQuestion)
		batchAllQuestions[standardQ] = i
		for _, q := range entries[i].SimilarQuestions {
			q = strings.TrimSpace(q)
			if q != "" {
				// 只记录第一次出现的位置
				if _, exists := batchAllQuestions[q]; !exists {
					batchAllQuestions[q] = i
				}
			}
		}
	}

	// 用于收集每个条目被移除的相似问和反例
	removedSimilarQuestionsMap := make(map[int][]string)  // key 为条目索引
	removedNegativeQuestionsMap := make(map[int][]string) // key 为条目索引

	// 对每个通过第一次校验的条目，过滤冲突的相似问
	for idx, i := range validIndicesAfterStdQ {
		entry := &entries[i]
		standardQ := strings.TrimSpace(entry.StandardQuestion)

		validSimilarQuestions := make([]string, 0, len(entry.SimilarQuestions))
		var removedSimilarQuestions []string
		for _, q := range entry.SimilarQuestions {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			// 相似问校验：对比所有标准问+相似问 → 单条问法失败「相似问冲突」
			// 如果该相似问与其他条目的标准问或相似问冲突（且不是自己的标准问），则移除
			if firstIdx, exists := batchAllQuestions[q]; exists && firstIdx != i {
				logger.Infof(ctx, "FAQ entry %d: similar question '%s' conflicts with entry %d, removing", i, q, firstIdx+1)
				removedSimilarQuestions = append(removedSimilarQuestions, fmt.Sprintf(`「相似问冲突」："%s"与第 %d 行"标准问/相似问"冲突`, q, firstIdx+1))
				continue
			}
			// 相似问不能与自己的标准问相同
			if q == standardQ {
				logger.Infof(ctx, "FAQ entry %d: similar question '%s' same as standard question, removing", i, q)
				removedSimilarQuestions = append(removedSimilarQuestions, fmt.Sprintf(`「相似问冲突」："%s"与本条"标准问"冲突`, q))
				continue
			}
			validSimilarQuestions = append(validSimilarQuestions, q)
		}
		entries[i].SimilarQuestions = validSimilarQuestions

		// 记录被移除的相似问
		if len(removedSimilarQuestions) > 0 {
			removedSimilarQuestionsMap[i] = removedSimilarQuestions
		}

		// 定期更新进度消息
		if (idx+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证相似问 %d/%d...", idx+1, len(validIndicesAfterStdQ))
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// ==================== 第三次迭代：反例冲突检测 ====================
	// 只处理通过前两次校验的条目，反例冲突只移除冲突的反例
	for idx, i := range validIndicesAfterStdQ {
		entry := &entries[i]
		standardQ := strings.TrimSpace(entry.StandardQuestion)

		// 构建当前QA的所有问题集合（标准问+通过校验的相似问）
		currentQAQuestions := make(map[string]bool)
		currentQAQuestions[standardQ] = true
		for _, q := range entry.SimilarQuestions {
			currentQAQuestions[q] = true
		}

		validNegativeQuestions := make([]string, 0, len(entry.NegativeQuestions))
		var removedNegativeQuestions []string
		for _, q := range entry.NegativeQuestions {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			// 反例校验：对比当前QA下所有标准问+相似问 → 单条问法失败「反例冲突」
			if currentQAQuestions[q] {
				logger.Infof(ctx, "FAQ entry %d: negative question '%s' conflicts with current QA's questions, removing", i, q)
				removedNegativeQuestions = append(removedNegativeQuestions, fmt.Sprintf(`「反例冲突」："%s"与本条"标准问/相似问"冲突`, q))
				continue
			}
			validNegativeQuestions = append(validNegativeQuestions, q)
		}
		entries[i].NegativeQuestions = validNegativeQuestions

		// 记录被移除的反例
		if len(removedNegativeQuestions) > 0 {
			removedNegativeQuestionsMap[i] = removedNegativeQuestions
		}

		// 定期更新进度消息
		if (idx+1)%100 == 0 {
			progress.Message = fmt.Sprintf("正在验证反例 %d/%d...", idx+1, len(validIndicesAfterStdQ))
			progress.UpdatedAt = time.Now().Unix()
			if err := s.saveFAQImportProgress(ctx, progress); err != nil {
				logger.Warnf(ctx, "Failed to update FAQ dry run progress: %v", err)
			}
		}
	}

	// 将部分失败信息添加到 FailedEntries
	for _, i := range validIndicesAfterStdQ {
		removedSimilar := removedSimilarQuestionsMap[i]
		removedNegative := removedNegativeQuestionsMap[i]
		if len(removedSimilar) > 0 || len(removedNegative) > 0 {
			pf := buildFAQPartialFailedEntry(i, &entries[i], removedSimilar, removedNegative)
			progress.FailedEntries = append(progress.FailedEntries, pf)
			progress.PartialFailedCount++
		}
	}

	return validIndicesAfterStdQ
}

// dropUnusableExternalSeqIDs clears the client-supplied seq ids that cannot be
// written as chunk seq ids: values at or above the sequence start, values a row
// already holds (soft-deleted rows included, because the unique index covers
// them), and values repeated within the same payload. Clearing one is not an
// error -- the row is created with the next id the database assigns, which is
// what an import without ids gets anyway.
//
// Replace mode clears them all: it creates the new rows before deleting the old
// ones, so every preserved id would collide with the row it came from and fail
// the whole insert batch.
func (s *knowledgeService) dropUnusableExternalSeqIDs(
	ctx context.Context,
	entries []*types.FAQEntryPayload,
	replaceMode bool,
) {
	if replaceMode || s.chunkRepo == nil {
		for _, e := range entries {
			if e != nil {
				e.ID = nil
			}
		}
		return
	}
	requested := make([]int64, 0, len(entries))
	for _, e := range entries {
		if e == nil || e.ID == nil {
			continue
		}
		if *e.ID <= 0 || *e.ID >= types.FAQExternalSeqIDLimit {
			e.ID = nil
			continue
		}
		requested = append(requested, *e.ID)
	}
	if len(requested) == 0 {
		return
	}
	inUse, err := s.chunkRepo.SeqIDsInUse(ctx, requested)
	if err != nil {
		logger.Warnf(ctx, "Could not check client-supplied seq ids, letting the database assign them: %v", err)
		for _, e := range entries {
			if e != nil {
				e.ID = nil
			}
		}
		return
	}
	seen := make(map[int64]bool, len(requested))
	cleared := 0
	for _, e := range entries {
		if e == nil || e.ID == nil {
			continue
		}
		if inUse[*e.ID] || seen[*e.ID] {
			e.ID = nil
			cleared++
			continue
		}
		seen[*e.ID] = true
	}
	if cleared > 0 {
		logger.Infof(ctx, "Dropped %d client-supplied seq ids that were taken or duplicated", cleared)
	}
}

// unionStrings 合并两个字符串切片并去重（完全匹配）
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	result := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, s := range b {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// validateFAQEntryPayloadBasic 验证 FAQ 条目的基本格式
func validateFAQEntryPayloadBasic(entry *types.FAQEntryPayload) error {
	if entry == nil {
		return fmt.Errorf("条目不能为空")
	}
	standardQ := strings.TrimSpace(entry.StandardQuestion)
	if standardQ == "" {
		return fmt.Errorf("标准问不能为空")
	}
	if len(entry.Answers) == 0 {
		return fmt.Errorf("答案不能为空")
	}
	hasValidAnswer := false
	for _, a := range entry.Answers {
		if strings.TrimSpace(a) != "" {
			hasValidAnswer = true
			break
		}
	}
	if !hasValidAnswer {
		return fmt.Errorf("答案不能全为空")
	}
	// The per-entry shape bounds belong here, not only in the sanitiser that
	// runs later while planning: an entry rejected there is dropped as
	// "skipped" after the validation count has already been fixed, so an
	// oversized entry used to be reported as imported. Rejecting it at
	// validation puts it in the failure record where the caller can see it.
	if err := secutils.CheckFAQEntryShape(standardQ, entry.SimilarQuestions, entry.NegativeQuestions, entry.Answers); err != nil {
		return err
	}
	return nil
}

type faqMergeOperation struct {
	Entry         types.FAQEntryPayload
	ExistingChunk *types.Chunk
	OldMeta       *types.FAQChunkMetadata
	MergedMeta    *types.FAQChunkMetadata
	Detail        types.FAQMergeDetail
}

// calculateAppendOperations 计算 Append 模式下的操作（支持智能合并）。
// 如果 entry 的标准问在 KB 中已存在，则视为合并操作（相似问 / 反例并集，
// 答案以新条目为准；策略仅在条目显式给出时以新条目为准，省略则沿用库内
// 已存策略）；否则视为新建。无变化（hash 一致且操作位
// 也未变）的合并目标会被 skip 掉，避免无效写库 / 重建索引。
//
// 内部 master 行为，对应开源版仅 "重复 = 失败" 的简化逻辑。FAQ 导入的
// 常见使用方式是"导出修改后重新 append"，需要这种合并语义才能正确叠加
// 新的相似问而不丢历史数据。
func (s *knowledgeService) calculateAppendOperations(ctx context.Context,
	tenantID uint64, kbID string, entries []types.FAQEntryPayload,
) (newEntries []types.FAQEntryPayload, newEntryIndices []int, mergeOps []faqMergeOperation, skippedCount int, err error) {
	if len(entries) == 0 {
		return nil, nil, nil, 0, nil
	}

	existingChunks, err := s.chunkRepo.ListAllFAQChunksWithMetadataByKnowledgeBaseID(ctx, tenantID, kbID)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to list existing FAQ chunks: %w", err)
	}

	existingStdQToChunk := make(map[string]*types.Chunk)
	existingQuestions := make(map[string]bool)
	for _, chunk := range existingChunks {
		meta, cErr := chunk.FAQMetadata()
		if cErr != nil || meta == nil {
			continue
		}
		if meta.StandardQuestion != "" {
			existingStdQToChunk[meta.StandardQuestion] = chunk
			existingQuestions[meta.StandardQuestion] = true
		}
		for _, q := range meta.SimilarQuestions {
			if q != "" {
				existingQuestions[q] = true
			}
		}
	}

	batchQuestions := make(map[string]bool)
	newEntries = make([]types.FAQEntryPayload, 0, len(entries))
	newEntryIndices = make([]int, 0, len(entries))
	mergeOps = make([]faqMergeOperation, 0)

	for entryIdx, entry := range entries {
		// A payload that omits the strategy -- an exported spreadsheet without
		// that column, or an API client that never sent it -- means "leave the
		// stored strategy alone", not "reset to all". sanitizeFAQEntryPayload
		// defaults a nil strategy to all, so the original has to be captured
		// before the merge below decides whether anything changed.
		strategySpecified := entry.AnswerStrategy != nil && *entry.AnswerStrategy != ""
		meta, sErr := sanitizeFAQEntryPayload(&entry)
		if sErr != nil {
			skippedCount++
			logger.Warnf(ctx, "Skipping invalid FAQ entry: %v", sErr)
			continue
		}

		// 检查标准问是否在 KB 中作为标准问存在 → 合并候选
		existingChunk, isMergeCandidate := existingStdQToChunk[meta.StandardQuestion]
		if isMergeCandidate {
			existingMeta, mErr := existingChunk.FAQMetadata()
			if mErr != nil || existingMeta == nil {
				isMergeCandidate = false
			} else {
				mergedSimilar := unionStrings(existingMeta.SimilarQuestions, meta.SimilarQuestions)
				mergedNegative := unionStrings(existingMeta.NegativeQuestions, meta.NegativeQuestions)

				// Rows written before the strategy was stored carry an empty
				// value; the runtime treats that as all, so compare against the
				// effective value or an unchanged row would look changed and be
				// rewritten and re-indexed on every import.
				storedStrategy := existingMeta.AnswerStrategy
				if storedStrategy == "" {
					storedStrategy = types.AnswerStrategyAll
				}
				answerStrategy := meta.AnswerStrategy
				if !strategySpecified {
					answerStrategy = storedStrategy
				}

				mergedMeta := &types.FAQChunkMetadata{
					StandardQuestion:  existingMeta.StandardQuestion,
					SimilarQuestions:  mergedSimilar,
					NegativeQuestions: mergedNegative,
					Answers:           meta.Answers,
					AnswerStrategy:    answerStrategy,
					Version:           existingMeta.Version + 1,
					Source:            existingMeta.Source,
				}

				// The union can push two individually valid entries past the
				// per-entry limits. Validation records that as a failure; this
				// is the defensive copy for a plan that skipped validation.
				if shapeErr := secutils.CheckFAQEntryShape(
					mergedMeta.StandardQuestion, mergedMeta.SimilarQuestions, mergedMeta.NegativeQuestions, mergedMeta.Answers,
				); shapeErr != nil {
					skippedCount++
					logger.Warnf(ctx, "Skipping merge for FAQ entry %q: merged entry exceeds the per-entry limits: %v",
						meta.StandardQuestion, shapeErr)
					continue
				}

				newHash := types.CalculateFAQContentHash(mergedMeta)
				enabledChanged := entry.IsEnabled != nil && *entry.IsEnabled != existingChunk.IsEnabled
				recommendedChanged := entry.IsRecommended != nil &&
					*entry.IsRecommended != existingChunk.Flags.HasFlag(types.ChunkFlagRecommended)
				answerStrategyChanged := answerStrategy != storedStrategy
				if existingChunk.ContentHash == newHash && !enabledChanged && !recommendedChanged && !answerStrategyChanged {
					skippedCount++
					logger.Infof(ctx, "Skipping merge for unchanged FAQ entry: %s", meta.StandardQuestion)
					continue
				}

				oldAnswerStr := strings.Join(existingMeta.Answers, "##")
				newAnswerStr := strings.Join(meta.Answers, "##")
				oldSimilarSet := make(map[string]bool, len(existingMeta.SimilarQuestions))
				for _, q := range existingMeta.SimilarQuestions {
					oldSimilarSet[q] = true
				}
				oldNegativeSet := make(map[string]bool, len(existingMeta.NegativeQuestions))
				for _, q := range existingMeta.NegativeQuestions {
					oldNegativeSet[q] = true
				}
				newSimilarCount := 0
				for _, q := range mergedSimilar {
					if !oldSimilarSet[q] {
						newSimilarCount++
					}
				}
				newNegativeCount := 0
				for _, q := range mergedNegative {
					if !oldNegativeSet[q] {
						newNegativeCount++
					}
				}

				mergeOps = append(mergeOps, faqMergeOperation{
					Entry:         entry,
					ExistingChunk: existingChunk,
					OldMeta:       existingMeta,
					MergedMeta:    mergedMeta,
					Detail: types.FAQMergeDetail{
						Index:            entryIdx,
						StandardQuestion: meta.StandardQuestion,
						AnswerChanged:    oldAnswerStr != newAnswerStr,
						NewSimilarCount:  newSimilarCount,
						NewNegativeCount: newNegativeCount,
					},
				})
				continue
			}
		}

		// 走到这里说明既不是合并候选，又跟现有 KB / 当前批次的标准问 / 相似问冲突。
		// 正常情况下这些冲突应在 executeFAQDryRunValidation 阶段就被拦截，这里
		// 仅作为防御性兜底；用 Warn 级别打点便于排查验证层漏检。
		if existingQuestions[meta.StandardQuestion] || batchQuestions[meta.StandardQuestion] {
			skippedCount++
			logger.Warnf(ctx,
				"calculateAppendOperations: dropping FAQ entry with duplicate standard question %q "+
					"(should have been filtered at validation); entry_idx=%d",
				meta.StandardQuestion, entryIdx)
			continue
		}

		batchQuestions[meta.StandardQuestion] = true
		for _, q := range meta.SimilarQuestions {
			batchQuestions[q] = true
		}
		newEntries = append(newEntries, entry)
		newEntryIndices = append(newEntryIndices, entryIdx)
	}

	return newEntries, newEntryIndices, mergeOps, skippedCount, nil
}

// calculateReplaceOperations 计算Replace模式下需要删除、创建、更新的条目
// 同时过滤掉同批次内标准问或相似问重复的条目
func (s *knowledgeService) calculateReplaceOperations(ctx context.Context,
	tenantID uint64, knowledgeID string, newEntries []types.FAQEntryPayload,
) (entriesToProcess []types.FAQEntryPayload, entryIndices []int, chunksToDelete []*types.Chunk, skippedCount int, err error) {
	// 获取 kbID 用于解析 tag
	var kbID string
	if len(newEntries) > 0 {
		// 从 knowledgeID 获取 kbID
		knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("failed to get knowledge: %w", err)
		}
		if knowledge != nil {
			kbID = knowledge.KnowledgeBaseID
		}
	}

	// 计算所有新条目的 content hash，并同时构建 hash 到 entry 的映射
	type entryWithHash struct {
		entry types.FAQEntryPayload
		hash  string
		meta  *types.FAQChunkMetadata
		index int
	}
	entriesWithHash := make([]entryWithHash, 0, len(newEntries))
	newHashSet := make(map[string]bool)
	// 用于批次内标准问和相似问去重
	batchQuestions := make(map[string]bool)
	batchSkippedCount := 0

	for entryIdx, entry := range newEntries {
		meta, err := sanitizeFAQEntryPayload(&entry)
		if err != nil {
			batchSkippedCount++
			logger.Warnf(ctx, "Skipping invalid FAQ entry in replace mode: %v", err)
			continue
		}

		// 检查标准问是否在同批次中重复
		if batchQuestions[meta.StandardQuestion] {
			batchSkippedCount++
			logger.Infof(ctx, "Skipping FAQ entry with duplicate standard question in batch: %s", meta.StandardQuestion)
			continue
		}

		// 检查相似问是否在同批次中重复
		hasDuplicateSimilar := false
		for _, q := range meta.SimilarQuestions {
			if batchQuestions[q] {
				hasDuplicateSimilar = true
				logger.Infof(ctx, "Skipping FAQ entry with duplicate similar question in batch: %s (standard: %s)", q, meta.StandardQuestion)
				break
			}
		}
		if hasDuplicateSimilar {
			batchSkippedCount++
			continue
		}

		// 将当前条目的标准问和相似问加入批次集合
		batchQuestions[meta.StandardQuestion] = true
		for _, q := range meta.SimilarQuestions {
			batchQuestions[q] = true
		}

		hash := types.CalculateFAQContentHash(meta)
		if hash != "" {
			entriesWithHash = append(entriesWithHash, entryWithHash{entry: entry, hash: hash, meta: meta, index: entryIdx})
			newHashSet[hash] = true
		}
	}

	// 查询所有已存在的chunks
	allExistingChunks, err := s.chunkRepo.ListAllFAQChunksByKnowledgeID(ctx, tenantID, knowledgeID)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to list existing chunks: %w", err)
	}

	// 在内存中过滤出匹配新条目hash的chunks，并构建map。
	// 同一 hash 出现多个 chunk 时只保留 SeqID 最大（最新创建）的一个作为匹配
	// 目标，其余列入删除：这是「先建后删」在创建成功、删除失败后被重试时留下
	// 的新旧并存状态，必须收敛，否则每重试一次就多一份重复数据。
	existingHashMap := make(map[string]*types.Chunk)
	for _, chunk := range allExistingChunks {
		if chunk.ContentHash == "" || !newHashSet[chunk.ContentHash] {
			continue
		}
		best, seen := existingHashMap[chunk.ContentHash]
		if !seen {
			existingHashMap[chunk.ContentHash] = chunk
			continue
		}
		if chunk.SeqID > best.SeqID {
			existingHashMap[chunk.ContentHash] = chunk
			chunksToDelete = append(chunksToDelete, best)
		} else {
			chunksToDelete = append(chunksToDelete, chunk)
		}
	}

	// 计算需要删除的chunks（数据库中有但新批次中没有的，或hash不匹配的）
	for _, chunk := range allExistingChunks {
		if chunk.ContentHash == "" {
			// 如果没有hash，需要删除（可能是旧数据）
			chunksToDelete = append(chunksToDelete, chunk)
		} else if !newHashSet[chunk.ContentHash] {
			// hash不在新条目中，需要删除
			chunksToDelete = append(chunksToDelete, chunk)
		}
	}

	// 批量预加载 tag 信息，避免循环内逐条查询数据库
	// 收集所有需要查询的 tag_id 和 tag_name
	tagSeqIDSet := make(map[int64]bool)
	tagNameSet := make(map[string]bool)
	for _, ewh := range entriesWithHash {
		if ewh.entry.TagID != 0 {
			tagSeqIDSet[ewh.entry.TagID] = true
		} else if ewh.entry.TagName != "" {
			tagNameSet[ewh.entry.TagName] = true
		} else {
			tagNameSet[types.UntaggedTagName] = true
		}
	}

	// 批量查询 tag by seq_id
	tagSeqIDToUUID := make(map[int64]string)
	if len(tagSeqIDSet) > 0 {
		seqIDs := make([]int64, 0, len(tagSeqIDSet))
		for seqID := range tagSeqIDSet {
			seqIDs = append(seqIDs, seqID)
		}
		tags, err := s.tagRepo.GetBySeqIDs(ctx, tenantID, seqIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to batch load tags by seq_ids: %v", err)
		} else {
			for _, tag := range tags {
				tagSeqIDToUUID[tag.SeqID] = tag.ID
			}
		}
	}

	// 批量查询 tag by name
	tagNameToUUID := make(map[string]string)
	if len(tagNameSet) > 0 && kbID != "" {
		for name := range tagNameSet {
			if tag, err := s.tagRepo.GetByName(ctx, tenantID, kbID, name); err == nil && tag != nil {
				tagNameToUUID[name] = tag.ID
			}
		}
	}

	logger.Infof(ctx, "Preloaded %d tags by seq_id, %d tags by name for %d entries",
		len(tagSeqIDToUUID), len(tagNameToUUID), len(entriesWithHash))

	// resolveTagIDFromCache 从缓存中解析 tag ID，缓存未命中时回退到数据库查询
	resolveTagIDFromCache := func(entry *types.FAQEntryPayload) (string, error) {
		if entry.TagID != 0 {
			if uuid, ok := tagSeqIDToUUID[entry.TagID]; ok {
				return uuid, nil
			}
			// 缓存未命中，回退到数据库查询（可能需要创建）
			return s.resolveTagID(ctx, kbID, entry)
		}
		tagName := entry.TagName
		if tagName == "" {
			tagName = types.UntaggedTagName
		}
		if uuid, ok := tagNameToUUID[tagName]; ok {
			return uuid, nil
		}
		// 缓存未命中，回退到数据库查询（可能需要创建）
		return s.resolveTagID(ctx, kbID, entry)
	}

	// 计算需要创建的条目（利用已经计算好的hash，避免重复计算）
	entriesToProcess = make([]types.FAQEntryPayload, 0, len(entriesWithHash))
	entryIndices = make([]int, 0, len(entriesWithHash))
	skippedCount = batchSkippedCount

	for idx, ewh := range entriesWithHash {
		// 每处理 1000 条打印一次进度日志
		if idx > 0 && idx%1000 == 0 {
			logger.Infof(ctx, "calculateReplaceOperations progress: %d/%d entries processed", idx, len(entriesWithHash))
		}

		existingChunk := existingHashMap[ewh.hash]
		if existingChunk != nil {
			// hash 匹配，检查 tag 是否变化
			newTagID, err := resolveTagIDFromCache(&ewh.entry)
			if err != nil {
				logger.Warnf(ctx, "Failed to resolve tag for entry, treating as new: %v", err)
				entriesToProcess = append(entriesToProcess, ewh.entry)
				entryIndices = append(entryIndices, ewh.index)
				continue
			}

			enabledChanged := ewh.entry.IsEnabled != nil && *ewh.entry.IsEnabled != existingChunk.IsEnabled
			recommendedChanged := ewh.entry.IsRecommended != nil &&
				*ewh.entry.IsRecommended != existingChunk.Flags.HasFlag(types.ChunkFlagRecommended)
			answerStrategyChanged := false
			if existingMeta, metaErr := existingChunk.FAQMetadata(); metaErr == nil && existingMeta != nil {
				if ewh.entry.AnswerStrategy != nil && *ewh.entry.AnswerStrategy != "" {
					answerStrategyChanged = ewh.meta.AnswerStrategy != existingMeta.AnswerStrategy
				} else if existingMeta.AnswerStrategy != "" {
					// An omitted strategy is "leave it as stored". The entry
					// below is re-created when its tag or operational flags
					// changed, and that rebuild sanitizes the payload -- which
					// would otherwise default the rebuilt row to all and reset
					// a stored random strategy as a side effect.
					ewh.entry.AnswerStrategy = &existingMeta.AnswerStrategy
				}
			}

			if existingChunk.TagID != newTagID || enabledChanged || recommendedChanged || answerStrategyChanged {
				if existingChunk.TagID != newTagID {
					logger.Infof(ctx, "FAQ entry tag changed from %s to %s, will update", existingChunk.TagID, newTagID)
				}
				if enabledChanged || recommendedChanged || answerStrategyChanged {
					logger.Infof(ctx, "FAQ entry operational fields changed (enabled: %v->%v, recommended: %v->%v, answerStrategy changed: %v), will update",
						existingChunk.IsEnabled, ewh.entry.IsEnabled,
						existingChunk.Flags.HasFlag(types.ChunkFlagRecommended), ewh.entry.IsRecommended,
						answerStrategyChanged)
				}
				chunksToDelete = append(chunksToDelete, existingChunk)
				entriesToProcess = append(entriesToProcess, ewh.entry)
				entryIndices = append(entryIndices, ewh.index)
			} else {
				// hash、tag、运营状态都相同，跳过
				skippedCount++
			}
			continue
		}

		// hash 不匹配或不存在，需要创建
		entriesToProcess = append(entriesToProcess, ewh.entry)
		entryIndices = append(entryIndices, ewh.index)
	}

	return entriesToProcess, entryIndices, chunksToDelete, skippedCount, nil
}

// faqPlannedEntry 是执行阶段的有序计划项，与 payload.Entries 按下标一一对应。
// entry 与 merge 互斥；两者都为 nil 表示该条目在计算阶段被跳过（非法、批内
// 重复或无变化），但仍占位以保持游标连续。
type faqPlannedEntry struct {
	entry *types.FAQEntryPayload
	merge *faqMergeOperation
}

// executeFAQImport 执行实际的FAQ导入逻辑。
//
// payload.Entries 必须是完整的有效条目列表（validEntries），startIndex 是
// 重试游标 progress.NextEntryIndex，即上一批已经处理到的位置。
// 失败可恢复的关键点：
//   - 每批成功后推进 NextEntryIndex，重试从该位置继续，依赖 content hash /
//     标准问匹配实现幂等；
//   - Replace 模式先创建新块并建索引，全部成功后才删除被替换的旧块与旧向量，
//     因此创建阶段失败不会丢已有数据；
//   - 本函数不会把任务置为 Completed，失败由调用方标记为 failed 并记录原因。
func (s *knowledgeService) executeFAQImport(ctx context.Context, taskID string, kbID string,
	payload *types.FAQBatchUpsertPayload, tenantID uint64, startIndex int,
	enqueuedAt int64, progress *types.FAQImportProgress,
) (err error) {
	// 保存知识库和embedding模型信息，用于清理索引
	var kb *types.KnowledgeBase
	var embeddingModel embedding.Embedder
	// 展示用总数：进度条分母，不作为切片游标。
	totalEntries := progress.Total
	if totalEntries <= 0 {
		totalEntries = len(payload.Entries) + progress.FailedCount
	}
	if startIndex < 0 {
		startIndex = 0
	}
	if startIndex > len(payload.Entries) {
		startIndex = len(payload.Entries)
	}

	// Recovery机制：只把 panic 转成错误，不做数据回滚。已创建的块留在库中，
	// 重试通过游标续跑与 hash 幂等收敛，不会产生重复数据。
	defer func() {
		// 捕获panic
		if r := recover(); r != nil {
			buf := make([]byte, 8192)
			n := runtime.Stack(buf, false)
			stack := string(buf[:n])
			logger.Errorf(ctx, "FAQ import task %s panicked: %v\n%s", taskID, r, stack)
			err = fmt.Errorf("panic during FAQ import: %v", r)
		}
	}()

	kb, ctx, err = s.writableFAQKnowledgeBase(ctx, kbID)
	if err != nil {
		return err
	}

	kb.EnsureDefaults()

	// 获取embedding模型，用于后续清理索引
	embeddingModel, err = s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return fmt.Errorf("failed to get embedding model: %w", err)
	}
	faqKnowledge, err := s.ensureFAQKnowledge(ctx, tenantID, kb)
	if err != nil {
		return err
	}

	// 获取索引模式
	indexMode := types.FAQIndexModeQuestionOnly
	if kb.FAQConfig != nil && kb.FAQConfig.IndexMode != "" {
		indexMode = kb.FAQConfig.IndexMode
	}

	// 增量更新逻辑：按 validEntries 的顺序构建有序计划。每个下标对应
	// payload.Entries 的一项：要么创建、要么合并、要么跳过（两者都 nil）。
	// 计划保留原始下标，重试才能从 NextEntryIndex 精确续跑——旧实现用
	// "已处理计数"当切片前缀，计数被 skip/merge 污染后导致漏导。
	plan := make([]faqPlannedEntry, len(payload.Entries))
	var chunksToDelete []*types.Chunk
	var skippedCount int

	if payload.Mode == types.FAQBatchModeReplace {
		// Replace模式：计算需要创建/更新的条目与其原始下标。
		// 旧块的删除推迟到所有新块创建并索引成功之后（先建后删）。
		var entriesToProcess []types.FAQEntryPayload
		var entryIndices []int
		entriesToProcess, entryIndices, chunksToDelete, skippedCount, err = s.calculateReplaceOperations(
			ctx,
			tenantID,
			faqKnowledge.ID,
			payload.Entries,
		)
		if err != nil {
			return fmt.Errorf("failed to calculate replace operations: %w", err)
		}
		for i := range entriesToProcess {
			plan[entryIndices[i]].entry = &entriesToProcess[i]
		}
	} else {
		// Append 模式（智能合并）：标准问已存在的条目走 merge ops，其余作为新建。
		var newEntries []types.FAQEntryPayload
		var newEntryIndices []int
		var mergeOps []faqMergeOperation
		newEntries, newEntryIndices, mergeOps, skippedCount, err = s.calculateAppendOperations(ctx, tenantID, kb.ID, payload.Entries)
		if err != nil {
			return fmt.Errorf("failed to calculate append operations: %w", err)
		}
		for i := range newEntries {
			plan[newEntryIndices[i]].entry = &newEntries[i]
		}
		for i := range mergeOps {
			plan[mergeOps[i].Detail.Index].merge = &mergeOps[i]
		}
	}

	// A client-supplied seq id is written into a globally unique column, so it
	// can only be honoured where it cannot collide. Clear the rest before the
	// batches run; see dropUnusableExternalSeqIDs.
	plannedEntries := make([]*types.FAQEntryPayload, 0, len(plan))
	for i := range plan {
		if plan[i].entry != nil {
			plannedEntries = append(plannedEntries, plan[i].entry)
		}
	}
	s.dropUnusableExternalSeqIDs(ctx, plannedEntries, payload.Mode == types.FAQBatchModeReplace)

	createPlanned := 0
	for i := startIndex; i < len(plan); i++ {
		if plan[i].entry != nil {
			createPlanned++
		}
	}
	logger.Infof(
		ctx,
		"FAQ import task %s: total entries: %d, resume from index: %d, to create: %d, skipped: %d, pending old-chunk deletes: %d",
		taskID,
		len(payload.Entries),
		startIndex,
		createPlanned,
		skippedCount,
		len(chunksToDelete),
	)

	// 游标之前的下标在上一轮已经完成，从 startIndex 继续是重试幂等的前提。
	if startIndex >= len(plan) {
		logger.Infof(ctx, "FAQ import task %s: nothing left to create from index %d", taskID, startIndex)
		if payload.Mode == types.FAQBatchModeReplace && len(chunksToDelete) > 0 {
			if err := s.ensureFAQImportLeaseForWriteStrict(ctx, kbID, taskID, progress.InstanceID, enqueuedAt); err != nil {
				return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
			}
			return s.deleteReplacedFAQChunks(ctx, taskID, tenantID, kb, faqKnowledge, chunksToDelete)
		}
		return nil
	}

	totalStartTime := time.Now()
	processedDisplay := startIndex + progress.FailedCount

	logger.Infof(
		ctx,
		"FAQ import task %s: starting batch processing from index %d of %d, total entries: %d, batch size: %d",
		taskID,
		startIndex,
		len(plan),
		totalEntries,
		faqImportBatchSize,
	)

	for i := startIndex; i < len(plan); i += faqImportBatchSize {
		batchStartTime := time.Now()
		end := i + faqImportBatchSize
		if end > len(plan) {
			end = len(plan)
		}

		// One batch can outlive the ten-minute lease (a slow embedding backend,
		// a large merge), and the progress save at its end is the next lease
		// check -- too late to stop before this batch's writes. Check ownership
		// at the batch start, before anything in it is persisted.
		if err := s.ensureFAQImportLeaseForWrite(ctx, kbID, taskID, progress.InstanceID, enqueuedAt); err != nil {
			return faqImportLeaseGateError(err)
		}

		// 1. 本批内的合并操作（Append 模式的智能合并）
		mergeOpsInBatch := make([]faqMergeOperation, 0, end-i)
		for j := i; j < end; j++ {
			if plan[j].merge != nil {
				mergeOpsInBatch = append(mergeOpsInBatch, *plan[j].merge)
			}
		}
		if len(mergeOpsInBatch) > 0 {
			mergedCount, mergeErr := s.executeFAQMergeOperations(ctx, taskID, kb, faqKnowledge, embeddingModel, indexMode, mergeOpsInBatch, progress)
			if mergeErr != nil {
				return fmt.Errorf("failed to execute merge operations: %w", mergeErr)
			}
			logger.Infof(ctx, "FAQ import task %s: merged %d entries in batch %d-%d", taskID, mergedCount, i+1, end)
			progress.MergedCount += mergedCount
			for _, op := range mergeOpsInBatch {
				progress.MergeDetails = append(progress.MergeDetails, op.Detail)
			}
		}

		// 2. 本批内的新建条目。batchIndices 记录每一项的原始下标：
		// 计划里的跳过项会让批内偏移与原下标错位，进度与报错必须用原下标。
		batch := make([]*types.FAQEntryPayload, 0, end-i)
		batchIndices := make([]int, 0, end-i)
		for j := i; j < end; j++ {
			if plan[j].entry != nil {
				batch = append(batch, plan[j].entry)
				batchIndices = append(batchIndices, j)
			}
		}
		logger.Infof(ctx, "FAQ import task %s: processing batch %d-%d (%d creates, %d merges)",
			taskID, i+1, end, len(batch), len(mergeOpsInBatch))

		// 构建chunks
		buildStartTime := time.Now()
		chunks := make([]*types.Chunk, 0, len(batch))
		chunkIds := make([]string, 0, len(batch))
		for idx, entryRef := range batch {
			entry := *entryRef
			meta, err := sanitizeFAQEntryPayload(&entry)
			if err != nil {
				logger.ErrorWithFields(ctx, err, map[string]interface{}{
					"entry":   entry,
					"task_id": taskID,
				})
				return fmt.Errorf("failed to sanitize entry at index %d: %w", batchIndices[idx], err)
			}

			// 解析 TagID
			tagID, err := s.resolveTagID(ctx, kbID, &entry)
			if err != nil {
				logger.ErrorWithFields(ctx, err, map[string]interface{}{
					"entry":   entry,
					"task_id": taskID,
				})
				return fmt.Errorf("failed to resolve tag for entry at index %d: %w", batchIndices[idx], err)
			}

			isEnabled := true
			if entry.IsEnabled != nil {
				isEnabled = *entry.IsEnabled
			}
			// 默认可推荐，与 CreateFAQEntry 保持一致；显式 false 落 0。
			// 必须显式写入：Flags 带 gorm default:1，零值会被默认值覆盖。
			flags := types.ChunkFlagRecommended
			if entry.IsRecommended != nil && !*entry.IsRecommended {
				flags = 0
			}
			chunk := &types.Chunk{
				ID:              uuid.New().String(),
				TenantID:        tenantID,
				KnowledgeID:     faqKnowledge.ID,
				KnowledgeBaseID: kb.ID,
				Content:         buildFAQChunkContent(meta, indexMode),
				IsEnabled:       isEnabled,
				Flags:           flags,
				ChunkType:       types.ChunkTypeFAQ,
				TagID:           tagID,                        // 使用解析后的 TagID
				Status:          int(types.ChunkStatusStored), // store but not indexed
			}
			// 如果指定了 ID（用于数据迁移），设置 SeqID
			if entry.ID != nil && *entry.ID > 0 {
				chunk.SeqID = *entry.ID
			}
			if err := chunk.SetFAQMetadata(meta); err != nil {
				return fmt.Errorf("failed to set FAQ metadata: %w", err)
			}
			chunks = append(chunks, chunk)
			chunkIds = append(chunkIds, chunk.ID)
		}
		buildDuration := time.Since(buildStartTime)
		logger.Debugf(ctx, "FAQ import task %s: batch %d-%d built %d chunks in %v, chunk IDs: %v",
			taskID, i+1, end, len(chunks), buildDuration, chunkIds)
		// 创建chunks
		createStartTime := time.Now()
		if err := s.chunkService.CreateChunks(ctx, chunks); err != nil {
			return fmt.Errorf("failed to create chunks: %w", err)
		}
		createDuration := time.Since(createStartTime)
		logger.Infof(
			ctx,
			"FAQ import task %s: batch %d-%d created %d chunks in %v",
			taskID,
			i+1,
			end,
			len(chunks),
			createDuration,
		)

		// 索引chunks
		indexStartTime := time.Now()
		// 索引失败直接返回错误，不删除本批已建的 chunk：它们没有可用向量，
		// 检索不到；重试会从 NextEntryIndex 续跑，由 content hash 收敛避免重复。
		if err := s.indexFAQChunks(ctx, kb, faqKnowledge, chunks, embeddingModel, true, false); err != nil {
			return fmt.Errorf("failed to index chunks: %w", err)
		}
		indexDuration := time.Since(indexStartTime)
		logger.Infof(
			ctx,
			"FAQ import task %s: batch %d-%d indexed %d chunks in %v",
			taskID,
			i+1,
			end,
			len(chunks),
			indexDuration,
		)

		// 更新chunks的Status为已索引：所有行写同一个值，一条 UPDATE ... WHERE id IN 即可，
		// 不需要把 content 等字段再回传一遍。
		for _, chunk := range chunks {
			chunk.Status = int(types.ChunkStatusIndexed) // indexed
		}
		if err := s.chunkRepo.UpdateChunkFieldsByIDs(ctx, tenantID, chunkIds, map[string]interface{}{
			"status": int(types.ChunkStatusIndexed),
		}); err != nil {
			return fmt.Errorf("failed to update chunks status: %w", err)
		}

		// 收集成功条目信息（tag 信息按批一次查出，避免每条一次查询）
		tagsByID := s.loadFAQTagsForChunks(ctx, tenantID, chunks)
		for idx, chunk := range chunks {
			entryIdx := batchIndices[idx] // 原始条目索引（展示用）
			meta, _ := chunk.FAQMetadata()
			standardQ := ""
			if meta != nil {
				standardQ = meta.StandardQuestion
			}
			tagID, tagName := faqTagInfo(tagsByID, chunk.TagID)
			progress.SuccessEntries = append(progress.SuccessEntries, types.FAQSuccessEntry{
				Index:            entryIdx,
				SeqID:            chunk.SeqID,
				TagID:            tagID,
				TagName:          tagName,
				StandardQuestion: standardQ,
			})
		}

		// 本批全部成功，推进游标。NextEntryIndex 是唯一的重试游标，下面的
		// 计数只用于进度展示。
		progress.NextEntryIndex = end
		processedDisplay = end + progress.FailedCount
		displayPercent := 0
		if totalEntries > 0 {
			displayPercent = int(float64(processedDisplay) / float64(totalEntries) * 100)
		}
		if saveErr := s.saveFAQImportProgressOrStop(ctx, progress); saveErr != nil {
			return saveErr
		}
		if err := s.updateFAQImportProgressStatus(ctx, taskID, progress.InstanceID, progress.EnqueuedAt, types.FAQImportStatusProcessing, displayPercent, totalEntries, processedDisplay, fmt.Sprintf("正在处理第 %d/%d 条", processedDisplay, totalEntries), ""); err != nil {
			logger.Errorf(ctx, "Failed to update task progress: %v", err)
		}

		batchDuration := time.Since(batchStartTime)
		logger.Infof(
			ctx,
			"FAQ import task %s: batch %d-%d completed in %v, cursor: %d/%d (%d%%)",
			taskID,
			i+1,
			end,
			batchDuration,
			processedDisplay,
			totalEntries,
			displayPercent,
		)
	}

	// Replace 模式：新块全部创建并索引成功后才删除被替换的旧块。
	// Deletion is the one irreversible step of a replace import, so ownership
	// is re-checked immediately before it.
	if payload.Mode == types.FAQBatchModeReplace && len(chunksToDelete) > 0 {
		if err := s.ensureFAQImportLeaseForWriteStrict(ctx, kbID, taskID, progress.InstanceID, enqueuedAt); err != nil {
			return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		if err := s.deleteReplacedFAQChunks(ctx, taskID, tenantID, kb, faqKnowledge, chunksToDelete); err != nil {
			return err
		}
	}

	totalDuration := time.Since(totalStartTime)
	var avgPerEntry time.Duration
	if processedDisplay > 0 {
		avgPerEntry = totalDuration / time.Duration(processedDisplay)
	}
	logger.Infof(
		ctx,
		"FAQ import task %s: all batches completed, processed: %d entries (skipped: %d) in %v, avg: %v per entry",
		taskID,
		processedDisplay,
		skippedCount,
		totalDuration,
		avgPerEntry,
	)

	return nil
}

// deleteReplacedFAQChunks removes the chunks replaced by a replace-mode import.
// The plan was computed before the caller re-checked ownership, so each row is
// deleted only while it still carries the seq id and content hash the plan
// picked it by: a row another writer rewrote in between belongs to that writer,
// not to this import.
//
// Rows go first, and their vectors second, only for the rows this call actually
// soft-deleted. Deleting vectors first would strip the vectors of a row that
// changed between the identity check and the delete -- a live entry another
// writer just reindexed -- which no retry restores. With this order a failed
// vector delete leaves vectors behind for rows that are already unreachable;
// that cannot corrupt a live entry, but it cannot be left to the import's own
// retry either: the retried plan is recomputed from live rows and no longer
// sees the soft-deleted ones, so the vectors would leak for good. They are
// handed to the durable index-delete task instead (see
// enqueueFAQIndexDeleteFallback); only the storage refund is skipped, which
// errs toward not under-billing the tenant.
func (s *knowledgeService) deleteReplacedFAQChunks(ctx context.Context, taskID string,
	tenantID uint64, kb *types.KnowledgeBase, faqKnowledge *types.Knowledge, chunksToDelete []*types.Chunk,
) error {
	if len(chunksToDelete) == 0 {
		return nil
	}
	unchanged, err := s.chunkRepo.UnchangedChunks(ctx, tenantID, chunksToDelete)
	if err != nil {
		return fmt.Errorf("failed to re-read replaced chunks: %w", err)
	}
	if skipped := len(chunksToDelete) - len(unchanged); skipped > 0 {
		logger.Infof(ctx,
			"FAQ import task %s: %d replaced chunks changed since the plan and are left alone",
			taskID, skipped)
	}
	if len(unchanged) == 0 {
		return nil
	}
	deletedIDs, err := s.chunkRepo.DeleteChunksIfUnchanged(ctx, tenantID, unchanged)
	if err != nil {
		return fmt.Errorf("failed to delete chunks: %w", err)
	}
	if len(deletedIDs) == 0 {
		logger.Infof(ctx, "FAQ import task %s: every replaced chunk changed before the delete; nothing to remove", taskID)
		return nil
	}
	deleted := make(map[string]struct{}, len(deletedIDs))
	for _, id := range deletedIDs {
		deleted[id] = struct{}{}
	}
	removedChunks := make([]*types.Chunk, 0, len(deletedIDs))
	for _, chunk := range unchanged {
		if _, ok := deleted[chunk.ID]; ok {
			removedChunks = append(removedChunks, chunk)
		}
	}
	if err := s.deleteFAQChunkVectors(ctx, kb, faqKnowledge, removedChunks); err != nil {
		deletedChunkIDs := make([]string, 0, len(removedChunks))
		for _, chunk := range removedChunks {
			deletedChunkIDs = append(deletedChunkIDs, chunk.ID)
		}
		s.enqueueFAQIndexDeleteFallback(ctx, tenantID, kb, deletedChunkIDs)
		return fmt.Errorf("failed to delete chunk vectors for %d deleted chunks: %w", len(removedChunks), err)
	}
	logger.Infof(ctx, "FAQ import task %s: deleted %d replaced chunks and their vectors (including updates)",
		taskID, len(removedChunks))
	return nil
}

// enqueueFAQIndexDeleteFallback hands chunk ids whose rows are already
// soft-deleted to the durable index-delete task. The import's own retry cannot
// finish the job: it recomputes its plan from live rows, and these rows are
// gone. The async task deletes by chunk id, retries on its own, and is safe to
// run against ids the synchronous attempt already removed.
func (s *knowledgeService) enqueueFAQIndexDeleteFallback(
	ctx context.Context, tenantID uint64, kb *types.KnowledgeBase, chunkIDs []string,
) {
	if len(chunkIDs) == 0 || s.task == nil {
		return
	}
	tenantInfo, ok := types.TenantInfoFromContext(ctx)
	if !ok {
		logger.Warnf(ctx, "FAQ import: cannot hand %d orphaned vectors to the index delete task: tenant info missing", len(chunkIDs))
		return
	}
	payload := types.IndexDeletePayload{
		TenantID:         tenantID,
		KnowledgeBaseID:  kb.ID,
		EmbeddingModelID: kb.EmbeddingModelID,
		KBType:           types.KnowledgeTypeFAQ,
		ChunkIDs:         chunkIDs,
		EffectiveEngines: tenantInfo.GetEffectiveEngines(),
		VectorStoreID:    kb.VectorStoreID,
	}
	langfuse.InjectTracing(ctx, &payload)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		logger.Errorf(ctx, "FAQ import: cannot marshal index delete fallback: %v", err)
		return
	}
	task := asynq.NewTask(types.TypeIndexDelete, payloadBytes,
		asynq.Queue(types.QueueMaintenance), asynq.MaxRetry(10), asynq.Timeout(time.Hour))
	if _, err := s.task.Enqueue(task); err != nil {
		logger.Errorf(ctx, "FAQ import: cannot enqueue index delete fallback for %d orphaned vectors: %v", len(chunkIDs), err)
		return
	}
	logger.Infof(ctx, "FAQ import: handed %d orphaned vectors to the index delete task", len(chunkIDs))
}

// updateFAQImportProgressStatus updates the FAQ import progress in Redis
func (s *knowledgeService) updateFAQImportProgressStatus(
	ctx context.Context,
	taskID string,
	instanceID string,
	enqueuedAt int64,
	status types.FAQImportTaskStatus,
	progress, total, processed int,
	message, errorMsg string,
) error {
	// Get existing progress from Redis
	existingProgress, err := s.GetFAQImportProgress(ctx, taskID)
	if err != nil {
		// If not found, create a new progress entry
		existingProgress = &types.FAQImportProgress{
			TaskID:     taskID,
			InstanceID: instanceID,
			CreatedAt:  time.Now().Unix(),
			TenantID:   tenantIDFromContext(ctx),
		}
	}

	// Update progress fields
	existingProgress.Status = status
	existingProgress.Progress = progress
	existingProgress.Total = total
	existingProgress.Processed = processed
	if message != "" {
		existingProgress.Message = message
	}
	existingProgress.Error = errorMsg
	if status == types.FAQImportStatusCompleted {
		existingProgress.Error = ""
	}

	// 任务完成或失败时，清除 running key。必须在最后一次进度保存之后再清：
	// 保存会续约（租约已过期时还会被本任务收回重建），先清后存会把刚删掉的
	// 标记又写回来，知识库看起来仍有一个正在运行的导入。
	clearing := status == types.FAQImportStatusCompleted || status == types.FAQImportStatusFailed
	if err := s.saveFAQImportProgress(ctx, existingProgress); err != nil {
		// A failed run whose lease was taken by another task still owns its own
		// progress key (the taker uses a different task id). Writing it blind is
		// safe and is the only way the initiator sees the run end instead of
		// polling "processing" until the record expires. A newer instance of the
		// same task id owns this record instead, so it is left alone.
		if status == types.FAQImportStatusFailed && errors.Is(err, errFAQImportLeaseTaken) &&
			!s.faqImportLeaseHeldByAnotherInstanceOfSameTask(ctx, existingProgress.KBID, taskID, instanceID) {
			if writeErr := s.writeFAQImportProgress(context.WithoutCancel(ctx), existingProgress); writeErr != nil {
				logger.Warnf(ctx, "Failed to record the end of taken-over FAQ import task %s: %v", taskID, writeErr)
			}
		} else {
			return err
		}
	}
	if clearing && existingProgress.KBID != "" {
		if clearErr := s.clearRunningFAQImportInfoIfMatches(context.WithoutCancel(ctx), existingProgress.KBID, taskID, instanceID, enqueuedAt); clearErr != nil {
			logger.Errorf(ctx, "Failed to clear running FAQ import task ID: %v", clearErr)
		}
	}
	return nil
}

// cleanupFAQEntriesFileOnFinalFailure 在任务最终失败时清理对象存储中的 entries 文件
// 只有当 retryCount >= maxRetry 时才执行清理，否则重试时还需要使用这个文件
func (s *knowledgeService) cleanupFAQEntriesFileOnFinalFailure(ctx context.Context, entriesURL string, retryCount, maxRetry int) {
	if entriesURL == "" || retryCount < maxRetry {
		return
	}
	if err := s.fileSvc.DeleteFile(ctx, entriesURL); err != nil {
		logger.Warnf(ctx, "Failed to delete FAQ entries file from object storage on final failure: %v", err)
	} else {
		logger.Infof(ctx, "Deleted FAQ entries file from object storage on final failure: %s", entriesURL)
	}
}

// runningFAQImportInfo stores the task ID and enqueued timestamp for uniquely identifying a task instance
type runningFAQImportInfo struct {
	TaskID     string `json:"task_id"`
	EnqueuedAt int64  `json:"enqueued_at"`
	InstanceID string `json:"instance_id,omitempty"`
	// ExpiresAt is the lease deadline in unix nanoseconds, used only by the
	// in-memory path (Lite, no Redis). Redis expires the key itself; the map
	// has no TTL, and without a deadline a worker that died without clearing
	// its marker blocked the knowledge base forever.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// memRunningImportInfo returns the in-memory marker for kbID, dropping it when
// its lease has lapsed.
func (s *knowledgeService) memRunningImportInfo(kbID string) *runningFAQImportInfo {
	v, ok := s.memFAQRunningImport.Load(kbID)
	if !ok {
		return nil
	}
	info, _ := v.(*runningFAQImportInfo)
	if info == nil {
		return nil
	}
	if info.ExpiresAt > 0 && time.Now().UnixNano() >= info.ExpiresAt {
		s.memFAQRunningImport.CompareAndDelete(kbID, info)
		return nil
	}
	return info
}

// sweepMemRunningImports drops markers whose lease has expired. Lazy expiry in
// memRunningImportInfo only runs when the same knowledge base is touched again,
// so markers of knowledge bases that are never imported again would otherwise
// stay in the map for the life of the process.
func (s *knowledgeService) sweepMemRunningImports() {
	now := time.Now().UnixNano()
	s.memFAQRunningImport.Range(func(key, value any) bool {
		info, _ := value.(*runningFAQImportInfo)
		if info == nil || (info.ExpiresAt > 0 && now >= info.ExpiresAt) {
			s.memFAQRunningImport.CompareAndDelete(key, value)
		}
		return true
	})
}

// getRunningFAQImportInfo checks if there's a running FAQ import task for the given KB
// Returns the task info if found, nil otherwise
func (s *knowledgeService) getRunningFAQImportInfo(ctx context.Context, kbID string) (*runningFAQImportInfo, error) {
	if s.redisClient == nil {
		return s.memRunningImportInfo(kbID), nil
	}
	key := getFAQImportRunningKey(kbID)
	data, err := s.redisClient.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get running FAQ import task: %w", err)
	}

	// Try to parse as JSON first (new format)
	var info runningFAQImportInfo
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		// Fallback: old format was just taskID string
		return &runningFAQImportInfo{TaskID: data, EnqueuedAt: 0}, nil
	}
	return &info, nil
}

// getRunningFAQImportTaskID checks if there's a running FAQ import task for the given KB
// Returns the task ID if found, empty string otherwise (for backward compatibility)
func (s *knowledgeService) getRunningFAQImportTaskID(ctx context.Context, kbID string) (string, error) {
	info, err := s.getRunningFAQImportInfo(ctx, kbID)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	return info.TaskID, nil
}

// claimFAQImportLease takes the per-KB import lease atomically.
//
// The previous check was GET-then-SET: two requests could both read "free" and
// both write, so two imports ran against the same knowledge base and the
// replace path could delete rows the other had just created. SET NX is one
// round trip and only one caller can win it.
//
// A Redis error is reported as an error rather than treated as "free". Failing
// open would let the very concurrency this lock exists to prevent through at
// exactly the moment the lock cannot be trusted; the caller turns it into a
// refusal the operator can retry.
//
// The lease is finite so a worker that dies without clearing it does not block
// the knowledge base forever. Every progress save refreshes it, which is what
// makes the lease a liveness signal rather than a deadline on the import.
func (s *knowledgeService) claimFAQImportLease(ctx context.Context, kbID string, info *runningFAQImportInfo) error {
	if s.redisClient == nil {
		// Single process: LoadOrStore is the same atomicity as SET NX. An
		// expired marker is dropped first -- the map has no TTL, and the lease
		// must be reclaimable after a crash just as Redis makes it expire.
		// Claims are rare, so this is also where expired markers of knowledge
		// bases that are never imported again get swept.
		s.sweepMemRunningImports()
		info.ExpiresAt = time.Now().Add(faqImportLeaseTTL).UnixNano()
		for {
			existing, loaded := s.memFAQRunningImport.LoadOrStore(kbID, info)
			if !loaded {
				return nil
			}
			prev, _ := existing.(*runningFAQImportInfo)
			if prev == nil {
				if s.memFAQRunningImport.CompareAndDelete(kbID, existing) {
					continue
				}
				return &faqImportLeaseHeldError{}
			}
			if prev.ExpiresAt > 0 && time.Now().UnixNano() >= prev.ExpiresAt {
				if s.memFAQRunningImport.CompareAndDelete(kbID, prev) {
					continue
				}
			}
			taskID := prev.TaskID
			return &faqImportLeaseHeldError{TaskID: taskID}
		}
	}
	key := getFAQImportRunningKey(kbID)
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal running info: %w", err)
	}
	ok, err := s.redisClient.SetNX(ctx, key, data, faqImportLeaseTTL).Result()
	if err != nil {
		return fmt.Errorf("failed to claim FAQ import lease: %w", err)
	}
	if !ok {
		held, _ := s.getRunningFAQImportInfo(ctx, kbID)
		taskID := ""
		if held != nil {
			taskID = held.TaskID
		}
		return &faqImportLeaseHeldError{TaskID: taskID}
	}
	return nil
}

// faqImportLeaseHeldError reports that another import already holds the lease.
// It is a refusal, not a failure: the caller returns a 400 that names the task.
type faqImportLeaseHeldError struct{ TaskID string }

func (e *faqImportLeaseHeldError) Error() string {
	return "FAQ import lease already held"
}

// faqImportTaskTenant is the tenant stamped into an FAQ import's task id: the
// caller's, so the caller can poll the progress their own submission returned.
// The KB's effective tenant is the fallback for call paths without a caller
// tenant, and is what the task itself runs as either way.
func faqImportTaskTenant(callerTenantID, kbTenantID uint64) uint64 {
	if callerTenantID != 0 {
		return callerTenantID
	}
	return kbTenantID
}

// errFAQImportLeaseTaken reports that another import took the KB's lease, so
// this one is no longer the single writer it was admitted as.
var errFAQImportLeaseTaken = errors.New("another FAQ import holds the knowledge base lease")

// renewFAQImportLeaseScript extends the lease only when it still belongs to
// this run, atomically. The previous GET-then-Expire left a window: the key
// could lapse after the GET and be claimed by a new task, and the Expire then
// refreshed the new task's marker -- two imports writing the same knowledge
// base. the instance id (when both sides have one) is part of identity, so a
// new instance that reused the task id is not mistaken for this one.
const renewFAQImportLeaseScript = `
local v = redis.call('GET', KEYS[1])
if not v then return 'missing' end
local ok, info = pcall(cjson.decode, v)
if ok and type(info) == 'table' then
  if tostring(info['task_id'] or '') ~= ARGV[1] then return 'taken' end
  local iid = tostring(info['instance_id'] or '')
  -- A modern marker belongs to exactly one instance; a legacy run cannot renew
  -- it merely because the task id matches.
  if iid ~= '' and (ARGV[2] == '' or iid ~= ARGV[2]) then return 'taken' end
else
  -- Legacy marker: the raw value was the task id.
  if v ~= ARGV[1] then return 'taken' end
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 'renewed'
`

// errFAQImportLeaseMissing reports that there was no lease to renew and the
// caller asked not to reclaim one. The heartbeat uses it: a marker that is gone
// was either cleared because the run finished or expired and belongs to nobody,
// and the heartbeat must not resurrect it -- doing so after finalization would
// leave an orphan lease blocking the knowledge base for a whole TTL.
var errFAQImportLeaseMissing = errors.New("FAQ import lease is gone")

// errFAQImportLeaseUnverifiable reports that the lease store could not be
// reached. Ordinary renewals log and continue -- a transient error should not
// fail an import -- but the gates in front of an irreversible write refuse to
// act on a lease they could not verify.
var errFAQImportLeaseUnverifiable = errors.New("FAQ import lease could not be verified")

// renewFAQImportLease extends the lease while the import makes progress, so a
// long import is not mistaken for an abandoned one.
//
// It only extends a lease this run owns: an unconditional Expire would refresh
// whatever task holds the key, and a renewal after the key lapsed would be a
// silent no-op, letting a second import be admitted while this one keeps
// writing. When the key is gone, nobody holds it and it is taken back for this
// run; when it names another task (or another instance of the same task id),
// errFAQImportLeaseTaken stops this import.
//
// This is the progress-save renewal only. A Redis error is logged and the save
// goes ahead: the record is what the initiator polls, and a transient error must
// not freeze it. The gates in front of row writes do not use this and do not
// fail open; see ensureFAQImportLeaseForWrite.
func (s *knowledgeService) renewFAQImportLease(
	ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64,
) error {
	err := s.renewFAQImportLeaseAllow(ctx, kbID, taskID, instanceID, enqueuedAt, true)
	if errors.Is(err, errFAQImportLeaseUnverifiable) {
		return nil
	}
	return err
}

// renewFAQImportLeaseAllow is renewFAQImportLease with control over reclaiming
// a missing key. allowReclaim=false is for periodic renewals (the heartbeat):
// those must never recreate a marker that was cleared, or a run that has just
// finished would leave the knowledge base blocked until the lease expires.
// enqueuedAt identifies this run's marker so the timestamp leg of later
// ownership checks survives a reclaim; 0 is only used by callers (legacy
// batch-progress paths) that genuinely do not know it.
func (s *knowledgeService) renewFAQImportLeaseAllow(
	ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64, allowReclaim bool,
) error {
	if kbID == "" || taskID == "" {
		return nil
	}
	if s.redisClient == nil {
		deadline := time.Now().Add(faqImportLeaseTTL).UnixNano()
		for {
			info := s.memRunningImportInfo(kbID)
			if info == nil {
				if !allowReclaim {
					return errFAQImportLeaseMissing
				}
				// Mirror the Redis reclaim: the map entry has expired, so this
				// run takes the lease back rather than renewing nothing.
				reclaim := &runningFAQImportInfo{TaskID: taskID, InstanceID: instanceID, EnqueuedAt: enqueuedAt, ExpiresAt: deadline}
				existing, loaded := s.memFAQRunningImport.LoadOrStore(kbID, reclaim)
				if !loaded {
					return nil
				}
				info, _ = existing.(*runningFAQImportInfo)
				if info == nil {
					return nil
				}
			}
			if info.TaskID != "" && info.TaskID != taskID {
				return errFAQImportLeaseTaken
			}
			if info.InstanceID != "" && info.InstanceID != instanceID {
				return errFAQImportLeaseTaken
			}
			// Publish a fresh copy instead of mutating the stored one: readers
			// hold the same pointer, so an in-place deadline update would race
			// with them. CompareAndSwap keeps a takeover that happened between
			// the load and here from being overwritten by this stale renewal.
			refreshed := &runningFAQImportInfo{
				TaskID: info.TaskID, EnqueuedAt: info.EnqueuedAt,
				InstanceID: info.InstanceID, ExpiresAt: deadline,
			}
			if s.memFAQRunningImport.CompareAndSwap(kbID, info, refreshed) {
				return nil
			}
		}
	}
	key := getFAQImportRunningKey(kbID)
	state, err := s.redisClient.Eval(ctx, renewFAQImportLeaseScript, []string{key},
		taskID, instanceID, faqImportLeaseTTL.Milliseconds()).Text()
	if err != nil {
		logger.Warnf(ctx, "Failed to renew FAQ import lease for KB %s: %v", kbID, err)
		return errFAQImportLeaseUnverifiable
	}
	if state == "taken" {
		return errFAQImportLeaseTaken
	}
	if state != "missing" {
		return nil
	}
	if !allowReclaim {
		return errFAQImportLeaseMissing
	}
	// The key lapsed between two renewals (a queue wait or a long batch). Take
	// it back with a SetNX so a simultaneous re-claim still has one winner.
	// A failed SetNX says nothing about who may hold the key: the reclaim is
	// unproven, not a success, and callers that gate irreversible writes must
	// see it as unverifiable.
	reclaim := &runningFAQImportInfo{TaskID: taskID, InstanceID: instanceID, EnqueuedAt: enqueuedAt}
	reclaimData, _ := json.Marshal(reclaim)
	claimed, setErr := s.redisClient.SetNX(ctx, key, reclaimData, faqImportLeaseTTL).Result()
	if setErr != nil {
		logger.Warnf(ctx, "Failed to reclaim FAQ import lease for KB %s: %v", kbID, setErr)
		return errFAQImportLeaseUnverifiable
	}
	if !claimed {
		return errFAQImportLeaseTaken
	}
	return nil
}

// ensureFAQImportLeaseForWrite is the gate in front of a batch's row writes.
// On a takeover it closes out this run's record (unless the taker is a newer
// instance of the same task id) and reports it, so the caller stops instead of
// writing beside the new owner.
//
// Unlike the progress-save renewal it does not fail open when the lease store
// cannot be reached. The caller is about to write rows, and a store that stops
// answering is exactly how a partitioned run keeps writing after its lease has
// lapsed and a new import has been admitted. The error is reported instead;
// callers treat it as retryable because nothing of the batch has been written
// yet, and the retry resumes from the progress cursor.
func (s *knowledgeService) ensureFAQImportLeaseForWrite(ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64) error {
	err := s.renewFAQImportLeaseAllow(ctx, kbID, taskID, instanceID, enqueuedAt, true)
	if err == nil {
		return nil
	}
	if errors.Is(err, errFAQImportLeaseTaken) &&
		!s.faqImportLeaseHeldByAnotherInstanceOfSameTask(ctx, kbID, taskID, instanceID) {
		s.markFAQImportStopped(ctx, taskID, instanceID, errFAQImportStoppedMessage)
	}
	return err
}

// faqImportLeaseGateError picks the asynq disposition for a failed write gate.
// A takeover is final for this attempt, while an unreadable lease store is
// retried: nothing of the batch has been written yet, and the retry resumes
// from the progress cursor instead of being archived as failed.
func faqImportLeaseGateError(err error) error {
	if errors.Is(err, errFAQImportLeaseUnverifiable) {
		return fmt.Errorf("FAQ import lease could not be verified: %w", err)
	}
	return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
}

// ensureFAQImportLeaseForWriteStrict is the gate for an irreversible write (the
// replace-mode deletion, the result write at finalization). Unlike the ordinary
// gate it also stops the run when the lease store cannot be reached: acting on
// an unverified lease is exactly how a partitioned process writes beside a new
// owner. A run that stops here will never be retried (callers wrap SkipRetry),
// so the record has to close on the way out -- an archived task stuck at
// "processing" is indistinguishable from one still importing.
func (s *knowledgeService) ensureFAQImportLeaseForWriteStrict(ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64) error {
	err := s.renewFAQImportLeaseAllow(ctx, kbID, taskID, instanceID, enqueuedAt, true)
	if err == nil {
		return nil
	}
	if errors.Is(err, errFAQImportLeaseUnverifiable) {
		s.markFAQImportStopped(ctx, taskID, instanceID, errFAQImportUnverifiableMessage)
		return err
	}
	if errors.Is(err, errFAQImportLeaseTaken) &&
		!s.faqImportLeaseHeldByAnotherInstanceOfSameTask(ctx, kbID, taskID, instanceID) {
		s.markFAQImportStopped(ctx, taskID, instanceID, errFAQImportStoppedMessage)
	}
	return err
}

// startFAQImportLeaseHeartbeat keeps the lease fresh while this run works.
// Progress saves renew too, but only once per batch: a batch that embeds and
// indexes slowly could outlive the lease, and a new submission would then be
// admitted beside the running import. The heartbeat ties the lease to the
// process's liveness instead of the batch cadence. On a takeover it calls
// onLost so the caller can cancel the run; a missing lease only stops the
// heartbeat, because cancelling then would abort a run that is finishing and
// has just cleared its own marker. The returned stop function is safe to call
// more than once.
func (s *knowledgeService) startFAQImportLeaseHeartbeat(
	ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64, onLost func(),
) func() {
	if kbID == "" || taskID == "" || faqImportLeaseHeartbeatInterval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		ticker := time.NewTicker(faqImportLeaseHeartbeatInterval)
		defer ticker.Stop()
		// Unverifiable renewals are tolerated only as long as they cannot have
		// outlasted the lease itself: every tick without a verified renewal eats
		// into the window in which a new import could be admitted beside this
		// one. Past one full TTL of them, this run is no more provable than a
		// taken lease, so it stops the same way.
		var unverifiableSince time.Time
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// WithoutCancel: a request-scoped cancellation must not abort
				// the renewal of a lease the work may still be using. Reclaim is
				// off: a marker that is gone was cleared or expired, and the
				// heartbeat must not recreate it -- that would leave an orphan
				// lease blocking the KB after this import has finished.
				err := s.renewFAQImportLeaseAllow(
					context.WithoutCancel(ctx), kbID, taskID, instanceID, enqueuedAt, false)
				if err == nil {
					unverifiableSince = time.Time{}
					continue
				}
				if errors.Is(err, errFAQImportLeaseMissing) {
					logger.Infof(ctx, "FAQ import %s lease is gone; heartbeat stops", taskID)
				} else if errors.Is(err, errFAQImportLeaseUnverifiable) {
					// The store is unreachable, not the lease lost; keep trying
					// until the outage could have outlasted the lease.
					if unverifiableSince.IsZero() {
						unverifiableSince = time.Now()
					}
					if time.Since(unverifiableSince) > faqImportLeaseTTL {
						logger.Warnf(ctx,
							"FAQ import %s could not verify its lease for a full TTL; stopping the run", taskID)
						if onLost != nil {
							onLost()
						}
						return
					}
					logger.Warnf(ctx, "FAQ import %s lease could not be verified: %v", taskID, err)
					continue
				} else {
					logger.Warnf(ctx, "FAQ import %s lost its lease while running: %v", taskID, err)
					if onLost != nil {
						onLost()
					}
				}
				return
			}
		}
	}()
	// Waiting for the goroutine keeps a renewal that is already in flight from
	// landing after the caller has cleared the marker.
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// faqImportLeaseHeldByAnotherInstanceOfSameTask reports whether the KB lease
// now names a different instance of this same task id. That newer instance
// owns the progress record for the id, so a superseded instance must not
// finish it with a failure of its own. Another *task* holding the lease does
// not block that: the record under this task id is this run's to close out.
// A read failure returns false ("not proven").
func (s *knowledgeService) faqImportLeaseHeldByAnotherInstanceOfSameTask(
	ctx context.Context, kbID, taskID, instanceID string,
) bool {
	if kbID == "" || taskID == "" {
		return false
	}
	info, err := s.getRunningFAQImportInfo(ctx, kbID)
	if err != nil || info == nil || info.TaskID != taskID {
		return false
	}
	return info.InstanceID != "" && instanceID != "" && info.InstanceID != instanceID
}

// setRunningFAQImportInfo sets the running task info for a KB
func (s *knowledgeService) setRunningFAQImportInfo(ctx context.Context, kbID string, info *runningFAQImportInfo) error {
	if s.redisClient == nil {
		if info.ExpiresAt == 0 {
			info.ExpiresAt = time.Now().Add(faqImportLeaseTTL).UnixNano()
		}
		s.memFAQRunningImport.Store(kbID, info)
		return nil
	}
	key := getFAQImportRunningKey(kbID)
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal running info: %w", err)
	}
	return s.redisClient.Set(ctx, key, data, faqImportProgressTTL).Err()
}

// clearRunningFAQImportTaskID clears the running task ID for a KB
func (s *knowledgeService) clearRunningFAQImportTaskID(ctx context.Context, kbID string) error {
	if s.redisClient == nil {
		s.memFAQRunningImport.Delete(kbID)
		return nil
	}
	key := getFAQImportRunningKey(kbID)
	return s.redisClient.Del(ctx, key).Err()
}

// clearRunningFAQImportInfoIfMatchesScript deletes the marker only if it still
// names this run. The previous GET-then-DEL let the key lapse in between and be
// re-claimed by a new instance, which the DEL then removed: the new import ran
// without a lease and a third submission could be admitted beside it.
const clearRunningFAQImportInfoIfMatchesScript = `
local v = redis.call('GET', KEYS[1])
if not v then return 0 end
local ok, info = pcall(cjson.decode, v)
if ok and type(info) == 'table' then
  if tostring(info['task_id'] or '') ~= ARGV[1] then return 0 end
  local iid = tostring(info['instance_id'] or '')
  local want_iid = ARGV[2]
  if iid ~= '' then
    -- A modern marker names one instance; a legacy run (no instance id) must
    -- not clear a lease it cannot prove it owns. Legacy markers fall back to
    -- the raw task id / timestamp below.
    if want_iid == '' or iid ~= want_iid then return 0 end
  else
    local want = tonumber(ARGV[3]) or 0
    if want ~= 0 then
      local t = tonumber(info['enqueued_at']) or 0
      if t ~= 0 and t ~= want then return 0 end
    end
  end
  redis.call('DEL', KEYS[1])
  return 1
end
if v == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`

func (s *knowledgeService) clearRunningFAQImportInfoIfMatches(ctx context.Context, kbID, taskID, instanceID string, enqueuedAt int64) error {
	if s.redisClient == nil {
		if v, ok := s.memFAQRunningImport.Load(kbID); ok {
			info, _ := v.(*runningFAQImportInfo)
			if runningFAQImportInfoMatches(info, taskID, instanceID, enqueuedAt) {
				// CompareAndDelete: a claim that landed after the load above
				// stored a different marker, and deleting by key would drop it.
				s.memFAQRunningImport.CompareAndDelete(kbID, info)
			}
		}
		return nil
	}

	key := getFAQImportRunningKey(kbID)
	if err := s.redisClient.Eval(ctx, clearRunningFAQImportInfoIfMatchesScript, []string{key},
		taskID, instanceID, strconv.FormatInt(enqueuedAt, 10)).Err(); err != nil {
		return fmt.Errorf("failed to clear running FAQ import info: %w", err)
	}
	return nil
}

func runningFAQImportInfoMatches(info *runningFAQImportInfo, taskID, instanceID string, enqueuedAt int64) bool {
	if info == nil || info.TaskID != taskID {
		return false
	}
	// A modern marker names one instance, and only that instance matches. A
	// legacy marker (no instance id) is identified by task id and timestamp,
	// as it always was.
	if info.InstanceID != "" {
		return info.InstanceID == instanceID
	}
	return enqueuedAt == 0 || info.EnqueuedAt == 0 || info.EnqueuedAt == enqueuedAt
}

// faqEntryIndicesInRange reports whether every cached entry index addresses a
// position in a payload of the given length, and that the indices are strictly
// increasing as verification produced them.
func faqEntryIndicesInRange(indices []int, entries int) bool {
	prev := -1
	for _, idx := range indices {
		if idx < 0 || idx >= entries || idx <= prev {
			return false
		}
		prev = idx
	}
	return true
}

// readFAQEntriesPayload reads an import file from object storage under the
// same byte budget the HTTP handler applies to inline payloads.
//
// This is the one path a payload enters memory without that budget: tasks
// queued before the budget existed, and retries of them, carry an object URL
// instead of inline entries. Reading them whole would restore the unbounded
// allocation the budget removed, so an oversized object is refused rather
// than retried -- it will be oversized on every attempt.
func readFAQEntriesPayload(ctx context.Context, files interfaces.FileService, path string) ([]byte, error) {
	reader, err := files.GetFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to download entries: %w", err)
	}
	defer reader.Close()

	budget := secutils.GetFAQImportMaxJSONBytes()
	data, err := io.ReadAll(io.LimitReader(reader, budget+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read entries data: %w", err)
	}
	if int64(len(data)) > budget {
		return nil, fmt.Errorf("%w: FAQ entries file exceeds the %d byte budget", asynq.SkipRetry, budget)
	}
	return data, nil
}

// faqIndexOperationalState 是索引记录里随条目一起落库的运营字段快照。
// 启用状态、推荐标记和标签不参与内容哈希，但会写进向量库的元数据，
// 因此只改这些字段时索引也必须重建。
type faqIndexOperationalState struct {
	TagID         string
	IsEnabled     bool
	IsRecommended bool
}

func faqOperationalStateOf(chunk *types.Chunk) faqIndexOperationalState {
	return faqIndexOperationalState{
		TagID:         chunk.TagID,
		IsEnabled:     chunk.IsEnabled,
		IsRecommended: chunk.Flags.HasFlag(types.ChunkFlagRecommended),
	}
}

// incrementalIndexFAQEntry 增量更新FAQ条目的索引
// 只对内容变化的部分进行embedding计算和索引更新，跳过未变化的部分
func (s *knowledgeService) incrementalIndexFAQEntry(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	chunk *types.Chunk,
	embeddingModel embedding.Embedder,
	oldStandardQuestion string,
	oldSimilarQuestions []string,
	oldAnswers []string,
	oldOperational faqIndexOperationalState,
	newMeta *types.FAQChunkMetadata,
) error {
	indexStartTime := time.Now()
	logger.Debugf(ctx, "incrementalIndexFAQEntry: starting for chunk=%s, oldSimilarQuestions=%d, newSimilarQuestions=%d",
		chunk.ID, len(oldSimilarQuestions), len(newMeta.SimilarQuestions))

	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, types.MustTenantIDFromContext(ctx), kb.VectorStoreID)
	if err != nil {
		return err
	}

	indexMode := types.FAQIndexModeQuestionAnswer
	if kb.FAQConfig != nil && kb.FAQConfig.IndexMode != "" {
		indexMode = kb.FAQConfig.IndexMode
	}

	// 对新旧数据进行归一化处理，确保与 buildFAQIndexInfoList 的行为一致
	// 旧数据归一化
	oldStandardQuestion = types.NormalizeQuestion(oldStandardQuestion)
	normalizedOldSimilarQuestions := make([]string, 0, len(oldSimilarQuestions))
	for _, q := range oldSimilarQuestions {
		if nq := types.NormalizeQuestion(q); nq != "" {
			normalizedOldSimilarQuestions = append(normalizedOldSimilarQuestions, nq)
		}
	}
	oldSimilarQuestions = normalizedOldSimilarQuestions
	oldAnswers = types.SanitizeStrings(oldAnswers)
	// 新数据归一化
	normalizedNewMeta := newMeta.Normalize()

	// 构建索引内容
	buildContent := func(question string, answers []string) string {
		if indexMode == types.FAQIndexModeQuestionAnswer && len(answers) > 0 {
			var builder strings.Builder
			builder.WriteString(question)
			for _, ans := range answers {
				builder.WriteString("\n")
				builder.WriteString(ans)
			}
			return builder.String()
		}
		return question
	}

	// 检查答案是否变化（仅在 QuestionAnswer 模式下才影响索引）
	answersChanged := indexMode == types.FAQIndexModeQuestionAnswer && !slices.Equal(oldAnswers, normalizedNewMeta.Answers)
	logger.Debugf(ctx, "incrementalIndexFAQEntry: answersChanged=%v (indexMode=%s), oldAnswers=%d, newAnswers=%d",
		answersChanged, indexMode, len(oldAnswers), len(normalizedNewMeta.Answers))

	// 运营字段（启用/推荐/标签）写在索引元数据里，内容不变时也必须重建。
	newOperational := faqOperationalStateOf(chunk)
	operationalChanged := oldOperational != newOperational
	if operationalChanged {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: operational fields changed, tagID %q->%q, enabled %v->%v, recommended %v->%v",
			oldOperational.TagID, newOperational.TagID,
			oldOperational.IsEnabled, newOperational.IsEnabled,
			oldOperational.IsRecommended, newOperational.IsRecommended)
	}

	// 收集需要更新的索引项
	var indexInfoToUpdate []*types.IndexInfo

	// 1. 检查标准问是否需要更新
	oldStdContent := buildContent(oldStandardQuestion, oldAnswers)
	newStdContent := buildContent(normalizedNewMeta.StandardQuestion, normalizedNewMeta.Answers)
	stdQuestionChanged := oldStdContent != newStdContent
	if stdQuestionChanged || operationalChanged {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: standard question changed=%v, sourceID=%s", stdQuestionChanged, chunk.ID)
		indexInfoToUpdate = append(indexInfoToUpdate, &types.IndexInfo{
			Content:         newStdContent,
			SourceID:        chunk.ID,
			SourceType:      types.ChunkSourceType,
			ChunkID:         chunk.ID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
			KnowledgeType:   types.KnowledgeTypeFAQ,
			TagID:           chunk.TagID,
			IsEnabled:       chunk.IsEnabled,
			IsRecommended:   chunk.Flags.HasFlag(types.ChunkFlagRecommended),
		})
	}

	// 2. 基于内容哈希处理相似问的增删改
	// 构建旧问题集合 (问题 -> 是否存在)
	oldQuestionsSet := make(map[string]struct{}, len(oldSimilarQuestions))
	for _, q := range oldSimilarQuestions {
		oldQuestionsSet[q] = struct{}{}
	}

	// 构建新问题集合
	newQuestionsSet := make(map[string]struct{}, len(normalizedNewMeta.SimilarQuestions))
	for _, q := range normalizedNewMeta.SimilarQuestions {
		newQuestionsSet[q] = struct{}{}
	}

	// 找出需要删除的问题（在旧集合中但不在新集合中）
	var sourceIDsToDelete []string
	var deletedQuestions []string
	for oldQ := range oldQuestionsSet {
		if _, exists := newQuestionsSet[oldQ]; !exists {
			sourceID := fmt.Sprintf("%s-%s", chunk.ID, hashQuestion(oldQ))
			sourceIDsToDelete = append(sourceIDsToDelete, sourceID)
			deletedQuestions = append(deletedQuestions, oldQ)
		}
	}
	// 兼容存量数据：旧实现按「下标」生成问题向量 SourceID（chunk.ID-i），
	// 无法反推出被删问题对应的旧下标，因此把 chunk 下所有下标形式的向量一并清理，
	// 再重新索引当前问题，避免被删问题的旧向量残留。
	for i := 0; i < len(oldSimilarQuestions); i++ {
		sourceIDsToDelete = append(sourceIDsToDelete, fmt.Sprintf("%s-%d", chunk.ID, i))
	}

	// 找出需要新增或更新的问题
	var addedQuestions, updatedQuestions []string
	for newQ := range newQuestionsSet {
		_, existedBefore := oldQuestionsSet[newQ]
		// 需要更新的条件：
		// 1. 新问题（之前不存在）
		// 2. 答案变化（需要重新embedding）
		// 3. 运营字段变化（元数据需要重写）
		if !existedBefore || answersChanged || operationalChanged {
			sourceID := fmt.Sprintf("%s-%s", chunk.ID, hashQuestion(newQ))
			indexInfoToUpdate = append(indexInfoToUpdate, &types.IndexInfo{
				Content:         buildContent(newQ, normalizedNewMeta.Answers),
				SourceID:        sourceID,
				SourceType:      types.ChunkSourceType,
				ChunkID:         chunk.ID,
				KnowledgeID:     chunk.KnowledgeID,
				KnowledgeBaseID: chunk.KnowledgeBaseID,
				KnowledgeType:   types.KnowledgeTypeFAQ,
				TagID:           chunk.TagID,
				IsEnabled:       chunk.IsEnabled,
				IsRecommended:   chunk.Flags.HasFlag(types.ChunkFlagRecommended),
			})
			if !existedBefore {
				addedQuestions = append(addedQuestions, newQ)
			} else {
				updatedQuestions = append(updatedQuestions, newQ)
			}
		}
	}

	// 输出详细的变化日志
	if len(deletedQuestions) > 0 {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: deleted similar questions: %v", deletedQuestions)
	}
	if len(addedQuestions) > 0 {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: added similar questions: %v", addedQuestions)
	}
	if len(updatedQuestions) > 0 {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: updated similar questions (answers or operational fields changed): %v", updatedQuestions)
	}

	// 3. 删除不再存在的相似问索引，以及即将重建的旧索引。
	// 不能依赖 BatchIndex 覆盖同一 SourceID：ES v8、Qdrant 等引擎每次写入
	// 生成新文档 ID，不先删会留下内容过期的重复条目。
	for _, info := range indexInfoToUpdate {
		sourceIDsToDelete = append(sourceIDsToDelete, info.SourceID)
	}
	if len(sourceIDsToDelete) > 0 {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: deleting %d obsolete sourceIDs: %v", len(sourceIDsToDelete), sourceIDsToDelete)
		if delErr := retrieveEngine.DeleteBySourceIDList(ctx, sourceIDsToDelete, embeddingModel.GetDimensions(), types.KnowledgeTypeFAQ); delErr != nil {
			logger.Warnf(ctx, "incrementalIndexFAQEntry: failed to delete obsolete source IDs: %v", delErr)
		}
	}

	// 4. 批量索引需要更新的内容
	newCount := len(normalizedNewMeta.SimilarQuestions)
	if len(indexInfoToUpdate) > 0 {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: updating %d index entries (skipped %d unchanged)",
			len(indexInfoToUpdate), 1+newCount-len(indexInfoToUpdate))
		if err := retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfoToUpdate); err != nil {
			return err
		}
	} else {
		logger.Debugf(ctx, "incrementalIndexFAQEntry: all %d entries unchanged (content and operational fields), skipping index update", 1+newCount)
	}

	// 5. 更新 knowledge 记录
	now := time.Now()
	knowledge.UpdatedAt = now
	knowledge.ProcessedAt = &now
	if err := s.repo.UpdateKnowledge(ctx, knowledge); err != nil {
		return err
	}

	totalDuration := time.Since(indexStartTime)
	logger.Debugf(ctx, "incrementalIndexFAQEntry: completed in %v, updated %d/%d entries",
		totalDuration, len(indexInfoToUpdate), 1+newCount)

	return nil
}

func (s *knowledgeService) indexFAQChunks(ctx context.Context,
	kb *types.KnowledgeBase, knowledge *types.Knowledge,
	chunks []*types.Chunk, embeddingModel embedding.Embedder,
	adjustStorage bool, needDelete bool,
) error {
	if len(chunks) == 0 {
		return nil
	}
	indexStartTime := time.Now()
	logger.Debugf(ctx, "indexFAQChunks: starting to index %d chunks", len(chunks))

	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
	if err != nil {
		return err
	}

	// 构建索引信息
	buildIndexInfoStartTime := time.Now()
	indexInfo := make([]*types.IndexInfo, 0)
	chunkIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		infoList, err := s.buildFAQIndexInfoList(ctx, kb, chunk)
		if err != nil {
			return err
		}
		indexInfo = append(indexInfo, infoList...)
		chunkIDs = append(chunkIDs, chunk.ID)
	}
	buildIndexInfoDuration := time.Since(buildIndexInfoStartTime)
	logger.Debugf(
		ctx,
		"indexFAQChunks: built %d index info entries for %d chunks in %v",
		len(indexInfo),
		len(chunks),
		buildIndexInfoDuration,
	)

	var size int64
	var reserved int64
	if adjustStorage {
		estimateStartTime := time.Now()
		size = retrieveEngine.EstimateStorageSize(ctx, embeddingModel, indexInfo)
		estimateDuration := time.Since(estimateStartTime)
		logger.Debugf(ctx, "indexFAQChunks: estimated storage size %d bytes in %v", size, estimateDuration)
		// Reserve before writing rather than checking now and adjusting after:
		// between the two, another import reads the same StorageUsed and both
		// pass a check whose sum exceeds the quota. The reservation is taken
		// under the row lock and released again if indexing fails below.
		if size > 0 {
			ok, err := s.tenantRepo.ReserveStorageQuota(ctx, tenantInfo.ID, size)
			if err != nil {
				return fmt.Errorf("failed to reserve storage quota: %w", err)
			}
			if !ok {
				return types.NewStorageQuotaExceededError()
			}
			reserved = size
			defer func() {
				if reserved == 0 {
					return
				}
				// Indexing failed (or was refused later): give the reservation
				// back so a failed import does not consume the tenant's quota.
				// The refund must not inherit the request's cancellation -- a
				// cancelled context is exactly when the caller went away, and
				// skipping the refund there would leak the reservation until
				// the tenant's quota is reset.
				refundCtx := context.WithoutCancel(ctx)
				if err := s.tenantRepo.AdjustStorageUsed(refundCtx, tenantInfo.ID, -reserved); err != nil {
					logger.Errorf(ctx, "Failed to release FAQ storage reservation of %d bytes: %v", reserved, err)
				}
			}()
		}
	}

	// 删除旧向量
	var deleteDuration time.Duration
	if needDelete {
		deleteStartTime := time.Now()
		if err := retrieveEngine.DeleteByChunkIDList(ctx, chunkIDs, embeddingModel.GetDimensions(), types.KnowledgeTypeFAQ); err != nil {
			logger.Warnf(ctx, "Delete FAQ vectors failed: %v", err)
		}
		deleteDuration = time.Since(deleteStartTime)
		if deleteDuration > 100*time.Millisecond {
			logger.Debugf(ctx, "indexFAQChunks: deleted old vectors for %d chunks in %v", len(chunkIDs), deleteDuration)
		}
	}

	// 批量索引（这里可能是性能瓶颈）
	batchIndexStartTime := time.Now()
	if err := retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfo); err != nil {
		return err
	}
	batchIndexDuration := time.Since(batchIndexStartTime)
	var avgPerEntry time.Duration
	if len(indexInfo) > 0 {
		avgPerEntry = batchIndexDuration / time.Duration(len(indexInfo))
	}
	logger.Debugf(ctx, "indexFAQChunks: batch indexed %d index info entries in %v (avg: %v per entry)",
		len(indexInfo), batchIndexDuration, avgPerEntry)

	if adjustStorage && size > 0 {
		adjustStartTime := time.Now()
		// The bytes were already reserved before indexing; the write above
		// completes that reservation, so it must not be adjusted again. The
		// reservation is only marked complete once the knowledge row that
		// carries the new StorageSize is persisted: until then a failure has
		// to refund it, or the retry would reserve the same bytes again.
		tenantInfo.StorageUsed += size
		knowledge.StorageSize += size
		adjustDuration := time.Since(adjustStartTime)
		if adjustDuration > 50*time.Millisecond {
			logger.Debugf(ctx, "indexFAQChunks: adjusted storage in %v", adjustDuration)
		}
	}

	updateStartTime := time.Now()
	now := time.Now()
	knowledge.UpdatedAt = now
	knowledge.ProcessedAt = &now
	err = s.repo.UpdateKnowledge(ctx, knowledge)
	updateDuration := time.Since(updateStartTime)
	if updateDuration > 50*time.Millisecond {
		logger.Debugf(ctx, "indexFAQChunks: updated knowledge in %v", updateDuration)
	}
	if err == nil && adjustStorage && size > 0 {
		// Persisted: the reservation is the tenant's real usage now.
		reserved = 0
	}

	totalDuration := time.Since(indexStartTime)
	logger.Debugf(
		ctx,
		"indexFAQChunks: completed indexing %d chunks in %v (build: %v, delete: %v, batchIndex: %v, update: %v)",
		len(chunks),
		totalDuration,
		buildIndexInfoDuration,
		deleteDuration,
		batchIndexDuration,
		updateDuration,
	)

	return err
}

func (s *knowledgeService) deleteFAQChunkVectors(ctx context.Context,
	kb *types.KnowledgeBase, knowledge *types.Knowledge, chunks []*types.Chunk,
) error {
	if len(chunks) == 0 {
		return nil
	}
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return err
	}
	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
	if err != nil {
		return err
	}

	indexInfo := make([]*types.IndexInfo, 0)
	chunkIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		infoList, err := s.buildFAQIndexInfoList(ctx, kb, chunk)
		if err != nil {
			return err
		}
		indexInfo = append(indexInfo, infoList...)
		chunkIDs = append(chunkIDs, chunk.ID)
	}

	size := retrieveEngine.EstimateStorageSize(ctx, embeddingModel, indexInfo)
	if err := retrieveEngine.DeleteByChunkIDList(ctx, chunkIDs, embeddingModel.GetDimensions(), types.KnowledgeTypeFAQ); err != nil {
		return err
	}
	if size > 0 {
		// Refunding a deletion must not be skipped because the caller went
		// away: the vectors are gone either way.
		if err := s.tenantRepo.AdjustStorageUsed(context.WithoutCancel(ctx), tenantInfo.ID, -size); err == nil {
			tenantInfo.StorageUsed -= size
			if tenantInfo.StorageUsed < 0 {
				tenantInfo.StorageUsed = 0
			}
		}
		if knowledge.StorageSize >= size {
			knowledge.StorageSize -= size
		} else {
			knowledge.StorageSize = 0
		}
	}
	knowledge.UpdatedAt = time.Now()
	return s.repo.UpdateKnowledge(ctx, knowledge)
}

// faqImportCompletedOutcome classifies a finished import for the activity log.
//
// A partially failed entry still wrote content, so it makes the outcome
// "partial" rather than "failed" even when nothing landed intact; leaving it
// out of the arguments made a batch that lost every entry's questions read as
// a clean success.
func faqImportCompletedOutcome(successCount, partialFailedCount, failedCount, skippedCount int) types.AuditOutcome {
	if successCount > 0 && (partialFailedCount > 0 || failedCount > 0 || skippedCount > 0) {
		return types.AuditOutcomePartial
	}
	if successCount > 0 {
		return types.AuditOutcomeSuccess
	}
	if partialFailedCount > 0 {
		return types.AuditOutcomePartial
	}
	if failedCount > 0 && skippedCount > 0 {
		return types.AuditOutcomePartial
	}
	if failedCount > 0 || skippedCount > 0 {
		return types.AuditOutcomeFailed
	}
	return types.AuditOutcomeSuccess
}

func faqImportActivityDetails(payload *types.FAQImportPayload, progress *types.FAQImportProgress, totalEntries int) map[string]any {
	details := map[string]any{"mode": payload.Mode}
	if progress == nil {
		return details
	}
	total := totalEntries
	if total <= 0 {
		total = progress.Total
	}
	if total > 0 {
		details["total"] = total
	}
	details["count"] = progress.SuccessCount
	if progress.PartialFailedCount > 0 {
		details["partial_failed"] = progress.PartialFailedCount
	}
	if progress.FailedCount > 0 {
		details["failed"] = progress.FailedCount
	}
	skipped := progress.SkippedCount
	if skipped <= 0 && total > 0 {
		// The remainder has to subtract partial failures too: they are not
		// successes, so leaving them out would report each one as skipped.
		skipped = total - progress.SuccessCount - progress.PartialFailedCount - progress.FailedCount
		if skipped < 0 {
			skipped = 0
		}
	}
	if skipped > 0 {
		details["skipped"] = skipped
	}
	return details
}

func (s *knowledgeService) recordFAQImportKBActivity(
	ctx context.Context,
	payload *types.FAQImportPayload,
	progress *types.FAQImportProgress,
	totalEntries int,
	action types.AuditAction,
	outcome types.AuditOutcome,
) {
	if s == nil || payload == nil || payload.DryRun || payload.KBID == "" {
		return
	}
	recordKBActivity(ctx, s.audit, payload.TenantID, payload.KBID, action,
		"faq_entry", payload.KnowledgeID, outcome, faqImportActivityDetails(payload, progress, totalEntries))
}

// ProcessFAQImport handles Asynq FAQ import tasks (including dry run mode)
func (s *knowledgeService) ProcessFAQImport(ctx context.Context, t *asynq.Task) error {
	var payload types.FAQImportPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "failed to unmarshal FAQ import task payload: %v", err)
		return fmt.Errorf("failed to unmarshal task payload: %w", err)
	}
	ctx = payload.Initiator.Apply(ctx)
	ctx = withKBActivityTask(ctx, payload.TaskID, kbActivityTrigger(ctx))

	ctx = logger.WithRequestID(ctx, uuid.New().String())
	ctx = logger.WithField(ctx, "faq_import", payload.TaskID)
	ctx = types.WithExecutionTenant(ctx, payload.TenantID)
	kb, err := s.validateFAQKnowledgeBase(ctx, payload.KBID)
	if err != nil {
		if errors.Is(err, repository.ErrKnowledgeBaseNotFound) {
			return fmt.Errorf("%w: FAQ task KB no longer exists", asynq.SkipRetry)
		}
		return err
	}
	ctx, err = access.WithKBTaskWrite(ctx, kb, payload.TenantID)
	if err != nil {
		return fmt.Errorf("%w: FAQ task KB does not belong to its tenant", asynq.SkipRetry)
	}
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		if errors.Is(err, repository.ErrKnowledgeNotFound) {
			return fmt.Errorf("%w: FAQ task document no longer exists", asynq.SkipRetry)
		}
		return err
	}
	if knowledge == nil || knowledge.TenantID != payload.TenantID || knowledge.KnowledgeBaseID != payload.KBID ||
		knowledge.Type != types.KnowledgeTypeFAQ {
		return fmt.Errorf("%w: FAQ task document does not belong to its KB", asynq.SkipRetry)
	}

	// A replace retry can arrive after the lease lapsed, a newer import was
	// admitted, ran and finished. Its result is persisted on this knowledge
	// row, and the marker it used is gone, so the running-instance check below
	// sees nothing. Without this, a stale replace retry would compute its
	// deletion set from its own old payload and delete everything the newer
	// import added. Append-only retries are not fenced here: they never delete,
	// and refusing them would also reject safe recovery of the same import.
	// The comparison is against the enqueue time, which a retry preserves:
	// an import that completed at or after this task was accepted makes it
	// stale. A result written by this same instance short-circuits first;
	// results from older builds carry no instance id and fall through to the
	// timestamp rule.
	if result := faqImportStaleResult(knowledge, &payload); result != nil {
		return s.refuseStaleFAQImport(ctx, &payload, result)
	}

	// A replay must not start writing progress for another accepted import.
	// Missing markers can expire before a manual retry; retain that recovery
	// path and the existing timestamp fallback for legacy task instances.
	running, err := s.getRunningFAQImportInfo(ctx, payload.KBID)
	if err != nil {
		return fmt.Errorf("failed to check FAQ import instance: %w", err)
	}
	if running != nil {
		if err := secutils.ValidateTaskID(running.TaskID); err != nil {
			return fmt.Errorf("invalid running FAQ import task ID: %w", err)
		}
		if !runningFAQImportInfoMatches(running, payload.TaskID, payload.InstanceID, payload.EnqueuedAt) {
			// Only a different task's record is ours to finish. A reused task id
			// (this task id, another instance) means the progress record belongs
			// to the newer instance, and marking it failed would report that
			// instance as stopped while it is about to run.
			if running.TaskID != payload.TaskID {
				s.markFAQImportStopped(ctx, payload.TaskID, payload.InstanceID, errFAQImportStoppedMessage)
			}
			return fmt.Errorf("%w: another FAQ import instance owns this KB", asynq.SkipRetry)
		}
	}
	// The lease can lapse while the task waits for a worker slot. Re-assert it
	// now that this attempt has passed the ownership check: with the key gone
	// this takes it back, so the rest of the run is not unleased.
	if err := s.ensureFAQImportLeaseForWrite(ctx, payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); err != nil {
		return faqImportLeaseGateError(err)
	}

	// The check above ran before this attempt held the lease; an import that
	// finished in between is invisible to it. Re-read the row under the lease so
	// a replace retry cannot delete entries the newer import just added.
	fresh, freshErr := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if freshErr != nil {
		logger.Warnf(ctx, "FAQ import %s: the lease-time staleness re-check could not read the row: %v",
			payload.TaskID, freshErr)
	} else if fresh != nil {
		if result := faqImportStaleResult(fresh, &payload); result != nil {
			return s.refuseStaleFAQImport(ctx, &payload, result)
		}
	}
	// This attempt may now run for longer than the lease TTL between two
	// progress saves (a slow batch); the heartbeat keeps the lease alive for as
	// long as this process is actually working. If the lease moves to another
	// import, it cancels this run so the rest of the batch stops writing.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	ctx = runCtx
	stopHeartbeat := s.startFAQImportLeaseHeartbeat(
		ctx, payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt, cancelRun)
	defer stopHeartbeat()

	// 获取任务重试信息，用于判断是否是最后一次重试
	retryCount, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	isLastRetry := retryCount >= maxRetry

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "failed to get tenant: %v", err)
		if errors.Is(err, repository.ErrTenantNotFound) {
			return fmt.Errorf("%w: FAQ task tenant no longer exists", asynq.SkipRetry)
		}
		return fmt.Errorf("failed to get FAQ import tenant: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	// 如果 entries 存储在对象存储中，先下载
	if payload.EntriesURL != "" && len(payload.Entries) == 0 {
		logger.Infof(ctx, "Downloading FAQ entries from object storage: %s", payload.EntriesURL)
		entriesData, err := readFAQEntriesPayload(ctx, s.fileSvc, payload.EntriesURL)
		if err != nil {
			logger.Errorf(ctx, "Failed to read FAQ entries file: %v", err)
			if errors.Is(err, asynq.SkipRetry) {
				// Deterministic: the file stays over budget on every attempt.
				// Close the record, release the lease and drop the file, as the
				// entry-cap refusal does, instead of leaving the KB blocked for
				// a task that will never run.
				message := fmt.Sprintf("导入文件超过服务端单次上限（%d 字节），请拆分后重新导入",
					secutils.GetFAQImportMaxJSONBytes())
				s.refuseFAQImportTask(ctx, &payload, message, payload.EntriesURL)
				return fmt.Errorf("%w: %s", asynq.SkipRetry, message)
			}
			// A transient download failure is worth retrying with the lease kept.
			return err
		}

		var entries []types.FAQEntryPayload
		if err := json.Unmarshal(entriesData, &entries); err != nil {
			logger.Errorf(ctx, "Failed to unmarshal FAQ entries: %v", err)
			// The file was written before its task was enqueued, so it is
			// corrupt rather than half-written; retrying cannot repair it.
			const message = "导入文件无法解析，请检查文件后重新导入"
			s.refuseFAQImportTask(ctx, &payload, message, payload.EntriesURL)
			return fmt.Errorf("%w: %s", asynq.SkipRetry, message)
		}

		payload.Entries = entries
		logger.Infof(ctx, "Downloaded %d FAQ entries from object storage", len(entries))
	}

	logger.Infof(ctx, "Processing FAQ import task: task_id=%s, kb_id=%s, total_entries=%d, dry_run=%v, retry=%d/%d",
		payload.TaskID, payload.KBID, len(payload.Entries), payload.DryRun, retryCount, maxRetry)
	if err := s.validateFAQImportTags(ctx, kb, payload.Entries); err != nil {
		return err
	}

	// 保存原始总数量
	originalTotalEntries := len(payload.Entries)

	// 初始化进度
	// 检查是否已有验证结果（用于重试时跳过验证）
	// 注意：必须在保存新 progress 之前查询，否则会被覆盖
	existingProgress, _ := s.GetFAQImportProgress(ctx, payload.TaskID)

	// A task id may be reused by a later submission. The record then belongs to
	// the newer instance, and this stale run must neither resume its cursor nor
	// overwrite it: its ValidEntryIndices index a different payload, and its
	// counts describe different entries. A payload without an instance id is a
	// legacy enqueue that predates the field; it is matched on the task id alone,
	// as it was before.
	if existingProgress != nil && existingProgress.InstanceID != "" && payload.InstanceID != "" &&
		existingProgress.InstanceID != payload.InstanceID {
		// The lease may have lapsed while this stale task waited, and the
		// re-assert above has just taken it back in this instance's name.
		// Hand it back before leaving: compare-and-delete only removes a marker
		// that names this very instance, so the newer instance's own lease is
		// never touched. Without this the knowledge base stays blocked for a
		// task that is about to stop.
		if clearErr := s.clearRunningFAQImportInfoIfMatches(
			context.WithoutCancel(ctx), payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); clearErr != nil {
			logger.Warnf(ctx, "Failed to release FAQ import lease held by a superseded instance: %v", clearErr)
		}
		switch existingProgress.Status {
		case types.FAQImportStatusCompleted, types.FAQImportStatusFailed:
			logger.Infof(ctx,
				"FAQ import: task %s was superseded by instance %s (%s); stale instance %s exits",
				payload.TaskID, existingProgress.InstanceID, existingProgress.Status, payload.InstanceID)
			return nil
		default:
			logger.Warnf(ctx,
				"FAQ import: task %s is owned by instance %s; stale instance %s stops without writing",
				payload.TaskID, existingProgress.InstanceID, payload.InstanceID)
			return fmt.Errorf("%w: another instance of this task id is running", asynq.SkipRetry)
		}
	}

	// The submission endpoint already caps the entry count, but a task can
	// reach the worker with more: its file was written before the cap was
	// lowered, or the payload carries only EntriesURL. Downstream validation,
	// merging and indexing all scale with this array, so the cap is enforced
	// here as well rather than trusted from the entry point.
	if maxEntries := secutils.GetFAQImportMaxEntries(); len(payload.Entries) > maxEntries {
		// Worded by the same helper as the submission entry points: the advice
		// differs by mode, and telling a replace caller to split the file would
		// delete every entry outside each batch.
		message := secutils.FAQTooManyEntriesMessage(maxEntries, len(payload.Entries), payload.Mode, true)
		s.refuseFAQImportTask(ctx, &payload, message, payload.EntriesURL)
		return fmt.Errorf("%w: %s", asynq.SkipRetry, message)
	}

	progress := &types.FAQImportProgress{
		TaskID:         payload.TaskID,
		KBID:           payload.KBID,
		KnowledgeID:    payload.KnowledgeID,
		TenantID:       payload.TenantID,
		InstanceID:     payload.InstanceID,
		EnqueuedAt:     payload.EnqueuedAt,
		Status:         types.FAQImportStatusProcessing,
		Progress:       0,
		Total:          originalTotalEntries,
		Processed:      0,
		SuccessCount:   0,
		FailedCount:    0,
		FailedEntries:  make([]types.FAQFailedEntry, 0),
		SuccessEntries: make([]types.FAQSuccessEntry, 0),
		Message:        "正在验证条目...",
		CreatedAt:      time.Now().Unix(),
		UpdatedAt:      time.Now().Unix(),
		DryRun:         payload.DryRun,
	}
	if err := s.saveFAQImportProgressOrStop(ctx, progress); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	// 缓存的索引只对同一份 payload 有效：同一 task id 的旧记录可能带着超出
	// 当前条目数的索引，直接拿来遍历 payload.Entries 会越界。
	reusableValidation := existingProgress != nil &&
		len(existingProgress.ValidEntryIndices) > 0 &&
		faqEntryIndicesInRange(existingProgress.ValidEntryIndices, len(payload.Entries))
	if existingProgress != nil && len(existingProgress.ValidEntryIndices) > 0 && !reusableValidation {
		logger.Warnf(ctx,
			"FAQ import: cached validation indices for task %s do not fit %d entries; re-validating",
			payload.TaskID, len(payload.Entries))
	}
	var validEntryIndices []int
	if reusableValidation {
		// 重试时直接使用之前的验证结果
		validEntryIndices = existingProgress.ValidEntryIndices
		// 必须写回新 progress 并落库：NextEntryIndex 是相对 validEntries 的
		// 偏移，一旦后续重试丢掉验证结果、重新算出不同的有效条目集合，
		// 游标就会指向另一条数据。
		progress.ValidEntryIndices = validEntryIndices
		progress.FailedCount = existingProgress.FailedCount
		progress.FailedEntries = existingProgress.FailedEntries
		// 部分失败与已合并的计数也属于“已经发生的验证结果”：丢掉它们，
		// 完成时的 SuccessCount = 有效数 - PartialFailedCount 会把部分失败
		// 算成完全成功，AddedCount = SuccessCount - MergedCount 也会把重试
		// 前已合并的条目算成新增。
		progress.PartialFailedCount = existingProgress.PartialFailedCount
		progress.MergedCount = existingProgress.MergedCount
		if len(existingProgress.MergeDetails) > 0 {
			progress.MergeDetails = existingProgress.MergeDetails
		}
		logger.Infof(ctx, "Reusing previous validation result: valid=%d, failed=%d, partial_failed=%d, merged=%d",
			len(validEntryIndices), progress.FailedCount, progress.PartialFailedCount, progress.MergedCount)
	} else {
		// 第一步：执行验证（无论是 dry run 还是 import 模式都需要验证）
		validEntryIndices = s.executeFAQDryRunValidation(ctx, &payload, progress)
		// 保存验证通过的索引，用于重试时跳过验证
		progress.ValidEntryIndices = validEntryIndices
		if err := s.saveFAQImportProgressOrStop(ctx, progress); err != nil {
			return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		logger.Infof(ctx, "FAQ validation completed: total=%d, valid=%d, failed=%d",
			originalTotalEntries, len(validEntryIndices), progress.FailedCount)
	}

	// Dry run 模式：验证完成后直接返回结果
	if payload.DryRun {
		return s.finalizeFAQValidation(ctx, &payload, progress, originalTotalEntries)
	}

	// Import 模式：检查是否有有效条目需要导入
	if len(validEntryIndices) == 0 {
		// 没有有效条目，直接完成
		return s.finalizeFAQValidation(ctx, &payload, progress, originalTotalEntries)
	}

	// 提取有效的条目
	validEntries := make([]types.FAQEntryPayload, 0, len(validEntryIndices))
	for _, idx := range validEntryIndices {
		validEntries = append(validEntries, payload.Entries[idx])
	}

	// 更新进度消息
	progress.Message = fmt.Sprintf("验证完成，开始导入 %d 条有效数据...", len(validEntries))
	progress.UpdatedAt = time.Now().Unix()
	if err := s.saveFAQImportProgressOrStop(ctx, progress); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	// 检查任务状态 - 幂等性处理（复用之前获取的 existingProgress）
	startIndex := 0
	if existingProgress != nil {
		if existingProgress.Status == types.FAQImportStatusCompleted {
			logger.Infof(ctx, "FAQ import already completed, skipping: %s", payload.TaskID)
			if clearErr := s.clearRunningFAQImportInfoIfMatches(context.WithoutCancel(ctx), payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); clearErr != nil {
				logger.Warnf(ctx, "Failed to clear running FAQ import info for completed task: %v", clearErr)
			}
			return nil // 幂等：已完成的任务直接返回
		}
		// NextEntryIndex 是唯一的重试游标，记录 validEntries 中已完成到的位置。
		// 不能用 Processed 反推——那是展示用计数，会被 skip/merge 污染；拿它
		// 当切片偏移会静默跳过未处理的条目（重试后仍报 100% 完成）。
		startIndex = existingProgress.NextEntryIndex
		if startIndex < 0 {
			startIndex = 0
		}
		if startIndex > len(validEntries) {
			startIndex = len(validEntries)
		}
		logger.Infof(ctx, "Resuming FAQ import from cursor: %d/%d (valid entries)", startIndex, len(validEntries))
	}

	// 幂等性处理：清理可能已部分处理的chunks和索引数据
	chunksDeleted, err := s.chunkRepo.DeleteUnindexedChunks(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "Failed to delete unindexed chunks: %v", err)
		// 如果是最后一次重试，更新状态为失败
		if isLastRetry {
			if updateErr := s.updateFAQImportProgressStatus(ctx, payload.TaskID, payload.InstanceID, payload.EnqueuedAt, types.FAQImportStatusFailed, 0, originalTotalEntries, 0, "清理未索引数据失败", err.Error()); updateErr != nil {
				logger.Errorf(ctx, "Failed to update task status to failed: %v", updateErr)
			}
			s.recordFAQImportKBActivity(ctx, &payload, progress, originalTotalEntries, types.AuditActionFAQImportFailed, types.AuditOutcomeFailed)
		}
		s.cleanupFAQEntriesFileOnFinalFailure(ctx, payload.EntriesURL, retryCount, maxRetry)
		return fmt.Errorf("failed to delete unindexed chunks: %w", err)
	}
	if len(chunksDeleted) > 0 {
		logger.Infof(ctx, "Deleted unindexed chunks: %d", len(chunksDeleted))

		// 删除索引数据
		embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
		if err == nil {
			retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
				ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
			if err == nil {
				chunkIDs := make([]string, 0, len(chunksDeleted))
				for _, chunk := range chunksDeleted {
					chunkIDs = append(chunkIDs, chunk.ID)
				}
				if err := retrieveEngine.DeleteByChunkIDList(ctx, chunkIDs, embeddingModel.GetDimensions(), types.KnowledgeTypeFAQ); err != nil {
					logger.Warnf(ctx, "Failed to delete index data for chunks (may not exist): %v", err)
				} else {
					logger.Infof(ctx, "Successfully deleted index data for %d chunks", len(chunksDeleted))
				}
			}
		}
	}

	// 始终传入完整的有效条目列表，执行阶段自己按 NextEntryIndex 跳过已完成的
	// 部分。这样重试的计划与首次一致，Replace 模式不必降级成 Append（先建后删
	// 下重试安全），被替换的旧块也才能在成功后被正确清理。
	entriesToImport := validEntries
	importMode := payload.Mode
	if startIndex > 0 {
		logger.Infof(ctx, "Continuing FAQ import from entry %d, remaining: %d entries", startIndex, len(validEntries)-startIndex)
	}

	// 构建FAQBatchUpsertPayload（使用验证通过的有效条目）
	faqPayload := &types.FAQBatchUpsertPayload{
		Entries: entriesToImport,
		Mode:    importMode,
	}

	// 执行FAQ导入（传入已处理的偏移量，用于进度计算）
	if err := s.executeFAQImport(ctx, payload.TaskID, payload.KBID, faqPayload, payload.TenantID, startIndex, payload.EnqueuedAt, progress); err != nil {
		logger.Errorf(ctx, "FAQ import task failed: %s, error: %v", payload.TaskID, err)
		if errors.Is(err, errFAQImportLeaseTaken) {
			// Another import took the knowledge base mid-run. Retrying would only
			// be refused again; stop now and let the entries file go, since no
			// later attempt of this task will read it.
			s.cleanupFAQEntriesFileOnFinalFailure(ctx, payload.EntriesURL, maxRetry, maxRetry)
			return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		// 如果是最后一次重试，更新状态为失败
		if isLastRetry {
			if updateErr := s.updateFAQImportProgressStatus(ctx, payload.TaskID, payload.InstanceID, payload.EnqueuedAt, types.FAQImportStatusFailed, 0, originalTotalEntries, len(validEntries), "导入失败", err.Error()); updateErr != nil {
				logger.Errorf(ctx, "Failed to update task status to failed: %v", updateErr)
			}
			s.recordFAQImportKBActivity(ctx, &payload, progress, originalTotalEntries, types.AuditActionFAQImportFailed, types.AuditOutcomeFailed)
		}
		s.cleanupFAQEntriesFileOnFinalFailure(ctx, payload.EntriesURL, retryCount, maxRetry)
		return fmt.Errorf("FAQ import failed: %w", err)
	}

	// 任务成功完成
	logger.Infof(ctx, "FAQ import task completed: %s, imported: %d, failed: %d",
		payload.TaskID, len(progress.SuccessEntries), progress.FailedCount)

	// 最终完成处理（生成失败条目 CSV 等）
	return s.finalizeFAQValidation(ctx, &payload, progress, originalTotalEntries)
}

// finalizeFAQValidation 完成 FAQ 验证/导入任务，生成失败条目 CSV（如果有）
func (s *knowledgeService) finalizeFAQValidation(ctx context.Context, payload *types.FAQImportPayload,
	progress *types.FAQImportProgress, originalTotalEntries int,
) error {
	// 清理对象存储中的 entries 文件（如果有）
	if payload.EntriesURL != "" {
		if err := s.fileSvc.DeleteFile(ctx, payload.EntriesURL); err != nil {
			logger.Warnf(ctx, "Failed to delete FAQ entries file from object storage: %v", err)
		} else {
			logger.Infof(ctx, "Deleted FAQ entries file from object storage: %s", payload.EntriesURL)
		}
	}
	progress.UpdatedAt = time.Now().Unix()

	// 如果有失败条目，生成 CSV 文件
	if len(progress.FailedEntries) > 0 {
		csvURL, err := s.generateFailedEntriesCSV(ctx, payload.TenantID, payload.TaskID, progress.FailedEntries)
		if err != nil {
			logger.Warnf(ctx, "Failed to generate failed entries CSV: %v", err)
		} else {
			progress.FailedEntriesURL = csvURL
			progress.FailedEntries = nil // 清空内联数据，使用 URL
			progress.Message += " (失败记录已导出为CSV)"
		}
	}

	// 必须在 saveFAQImportResultToDatabase 之前计算最终统计。
	progress.Status = types.FAQImportStatusCompleted
	progress.Progress = 100
	progress.Processed = originalTotalEntries

	if len(progress.ValidEntryIndices) > 0 {
		progress.SuccessCount = len(progress.ValidEntryIndices) - progress.PartialFailedCount
	} else if len(progress.SuccessEntries) > 0 {
		progress.SuccessCount = len(progress.SuccessEntries) - progress.PartialFailedCount
	} else {
		progress.SuccessCount = originalTotalEntries - progress.FailedCount - progress.PartialFailedCount
	}
	if progress.SuccessCount < 0 {
		progress.SuccessCount = 0
	}

	if progress.AddedCount == 0 && progress.MergedCount > 0 {
		progress.AddedCount = progress.SuccessCount - progress.MergedCount
		if progress.AddedCount < 0 {
			progress.AddedCount = 0
		}
	} else if progress.AddedCount == 0 {
		progress.AddedCount = progress.SuccessCount
	}

	skippedCount := originalTotalEntries - progress.SuccessCount - progress.PartialFailedCount - progress.FailedCount
	if skippedCount < 0 {
		skippedCount = 0
	}
	progress.SkippedCount = skippedCount

	if payload.DryRun {
		progress.Message = s.buildFAQImportResultMessage("验证完成", progress)
	} else {
		progress.Message = s.buildFAQImportResultMessage("导入完成", progress)
	}

	// 如果不是 dry run 模式，保存导入结果统计到数据库
	if !payload.DryRun {
		// 写库之前再确认一次租约仍属于本实例：租约在导入末期被另一个导入
		// 接管时，旧任务的结果不能落到库里，把新导入的统计覆盖掉。
		if err := s.ensureFAQImportLeaseForWriteStrict(ctx, payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); err != nil {
			return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		if err := s.saveFAQImportResultToDatabase(ctx, payload, progress, originalTotalEntries); err != nil {
			// The chunks are already written, so a retry resumes here and
			// rewrites the same result. Failing instead of logging matters
			// because this row is also what a stale replace retry is compared
			// against: a silently missing result would let that retry delete
			// what this import added.
			return fmt.Errorf("failed to save FAQ import result: %w", err)
		}

		// 只有 replace 模式才清理未使用的 Tag
		// append 模式不应删除用户预先创建的空标签
		if payload.Mode == types.FAQBatchModeReplace {
			deletedTags, err := s.tagRepo.DeleteUnusedTags(ctx, payload.TenantID, payload.KBID)
			if err != nil {
				logger.Warnf(ctx, "FAQ import task %s: failed to cleanup unused tags: %v", payload.TaskID, err)
			} else if deletedTags > 0 {
				logger.Infof(ctx, "FAQ import task %s: cleaned up %d unused tags after replace import", payload.TaskID, deletedTags)
			}
		}
	}

	// 使用 updateFAQImportProgressStatus 来确保正确清理 running key
	// 但是需要先保存其他字段，因为 updateFAQImportProgressStatus 不会保存所有字段
	// 租约接管同样要终止这一步：否则本任务会被确认完成，而记录留给新导入。
	if err := s.saveFAQImportProgressOrStop(ctx, progress); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	// 然后调用状态更新来清理 running key
	if err := s.updateFAQImportProgressStatus(ctx, payload.TaskID, payload.InstanceID, payload.EnqueuedAt, types.FAQImportStatusCompleted,
		100, originalTotalEntries, originalTotalEntries, progress.Message, ""); err != nil {
		logger.Warnf(ctx, "Failed to update final FAQ import status: %v", err)
	}

	logger.Infof(ctx, "FAQ task completed: %s, dry_run=%v, success: %d, added: %d, merged: %d, failed: %d, partial_failed: %d",
		payload.TaskID, payload.DryRun, progress.SuccessCount, progress.AddedCount, progress.MergedCount, progress.FailedCount, progress.PartialFailedCount)

	if !payload.DryRun {
		outcome := faqImportCompletedOutcome(
			progress.SuccessCount, progress.PartialFailedCount, progress.FailedCount, progress.SkippedCount)
		s.recordFAQImportKBActivity(ctx, payload, progress, originalTotalEntries,
			types.AuditActionFAQImportCompleted, outcome)
	}

	return nil
}

// executeFAQMergeOperations 批量执行 append-mode 合并操作：更新已有 chunk
// 的 metadata / content / 索引。使用 ListChunksByID 批量加载 + SaveChunks 事
// 务批量保存，减少 DB 往返。任一批次失败立即返回，由 executeFAQImport
// 的 defer recovery 决定是否回滚整次导入。
//
// 必要时这里逐条 fan-out 索引（EFPutDocument），而不是合并 chunks 批量
// 重建：因为索引底层是按 SourceID 覆盖写入，按合并后的最终内容直接 put
// 即可。
func (s *knowledgeService) executeFAQMergeOperations(
	ctx context.Context,
	taskID string,
	kb *types.KnowledgeBase,
	faqKnowledge *types.Knowledge,
	embeddingModel embedding.Embedder,
	indexMode types.FAQIndexMode,
	mergeOps []faqMergeOperation,
	progress *types.FAQImportProgress,
) (int, error) {
	if len(mergeOps) == 0 {
		return 0, nil
	}

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	mergedCount := 0

	for batchStart := 0; batchStart < len(mergeOps); batchStart += faqImportBatchSize {
		batchEnd := batchStart + faqImportBatchSize
		if batchEnd > len(mergeOps) {
			batchEnd = len(mergeOps)
		}
		batch := mergeOps[batchStart:batchEnd]

		// 1. 批量加载完整 chunk（calculateAppendOperations 中只加载了部分字段，
		//    缺少 status/is_enabled/flags/seq_id 等，直接更新会将这些字段覆盖为零值）
		chunkIDs := make([]string, len(batch))
		for i, op := range batch {
			chunkIDs[i] = op.ExistingChunk.ID
		}
		fullChunks, err := s.chunkRepo.ListChunksByID(ctx, tenantID, chunkIDs)
		if err != nil {
			logger.Errorf(ctx, "FAQ import task %s: failed to batch reload chunks for merge: %v", taskID, err)
			return mergedCount, fmt.Errorf("failed to batch reload chunks for merge: %w", err)
		}
		chunkMap := make(map[string]*types.Chunk, len(fullChunks))
		for _, c := range fullChunks {
			chunkMap[c.ID] = c
		}

		// 2. 逐条应用合并数据
		mergedChunks := make([]*types.Chunk, 0, len(batch))
		for _, op := range batch {
			fullChunk, ok := chunkMap[op.ExistingChunk.ID]
			if !ok {
				logger.Errorf(ctx, "FAQ import task %s: chunk %s not found during batch reload", taskID, op.ExistingChunk.ID)
				return mergedCount, fmt.Errorf("chunk %s not found during batch reload", op.ExistingChunk.ID)
			}

			if err := fullChunk.SetFAQMetadata(op.MergedMeta); err != nil {
				logger.Errorf(ctx, "FAQ import task %s: failed to set merged metadata for chunk %s: %v", taskID, fullChunk.ID, err)
				return mergedCount, fmt.Errorf("failed to set merged FAQ metadata: %w", err)
			}

			fullChunk.Content = buildFAQChunkContent(op.MergedMeta, indexMode)
			fullChunk.ContentHash = types.CalculateFAQContentHash(op.MergedMeta)
			fullChunk.UpdatedAt = time.Now()

			// 用新值覆盖运营状态
			if op.Entry.IsEnabled != nil {
				fullChunk.IsEnabled = *op.Entry.IsEnabled
			}
			if op.Entry.IsRecommended != nil {
				if *op.Entry.IsRecommended {
					fullChunk.Flags = fullChunk.Flags.SetFlag(types.ChunkFlagRecommended)
				} else {
					fullChunk.Flags = fullChunk.Flags.ClearFlag(types.ChunkFlagRecommended)
				}
			}

			mergedChunks = append(mergedChunks, fullChunk)
		}

		// 3. 事务批量保存（GORM Save 全字段更新，确保 metadata/content_hash 持久化）
		if err := s.chunkRepo.SaveChunks(ctx, mergedChunks); err != nil {
			logger.Errorf(ctx, "FAQ import task %s: failed to batch save merged chunks: %v", taskID, err)
			return mergedCount, fmt.Errorf("failed to batch save merged chunks: %w", err)
		}

		// 4. 重建索引。先删旧索引：ES v8、Qdrant 等引擎不会覆盖相同 SourceID。
		if err := s.indexFAQChunks(ctx, kb, faqKnowledge, mergedChunks, embeddingModel, false, true); err != nil {
			return mergedCount, fmt.Errorf("failed to re-index merged chunks: %w", err)
		}

		// 5. 收集成功条目信息
		tagsByID := s.loadFAQTagsForChunks(ctx, tenantID, mergedChunks)
		for i, op := range batch {
			chunk := mergedChunks[i]
			meta := op.MergedMeta
			tagID, tagName := faqTagInfo(tagsByID, chunk.TagID)
			progress.SuccessEntries = append(progress.SuccessEntries, types.FAQSuccessEntry{
				Index:            op.Detail.Index,
				SeqID:            chunk.SeqID,
				TagID:            tagID,
				TagName:          tagName,
				StandardQuestion: meta.StandardQuestion,
			})
		}

		mergedCount += len(batch)

		logger.Infof(ctx, "FAQ import task %s: merged batch %d-%d (%d chunks)", taskID, batchStart+1, batchEnd, len(mergedChunks))
	}

	return mergedCount, nil
}

// loadFAQTagsForChunks resolves every distinct tag referenced by chunks with a
// single query. Lookup failures are logged and yield an empty map so the
// import result degrades to "no tag info" instead of aborting the batch.
func (s *knowledgeService) loadFAQTagsForChunks(
	ctx context.Context, tenantID uint64, chunks []*types.Chunk,
) map[string]*types.KnowledgeTag {
	tagsByID := make(map[string]*types.KnowledgeTag)
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, chunk := range chunks {
		if chunk == nil || chunk.TagID == "" {
			continue
		}
		if _, ok := seen[chunk.TagID]; ok {
			continue
		}
		seen[chunk.TagID] = struct{}{}
		ids = append(ids, chunk.TagID)
	}
	if len(ids) == 0 {
		return tagsByID
	}
	tags, err := s.tagRepo.GetByIDs(ctx, tenantID, ids)
	if err != nil {
		logger.Warnf(ctx, "Failed to load FAQ tags for import result: %v", err)
		return tagsByID
	}
	for _, tag := range tags {
		if tag != nil {
			tagsByID[tag.ID] = tag
		}
	}
	return tagsByID
}

// faqTagInfo returns the external (seq_id, name) pair for tagID, or zero values
// when the chunk has no tag or the tag could not be loaded.
func faqTagInfo(tagsByID map[string]*types.KnowledgeTag, tagID string) (int64, string) {
	if tagID == "" {
		return 0, ""
	}
	if tag, ok := tagsByID[tagID]; ok && tag != nil {
		return tag.SeqID, tag.Name
	}
	return 0, ""
}

// buildFAQImportResultMessage 构建 FAQ 导入 / 验证最终结果的人类可读消息。
// 前端会把它直接展示在 toast / 任务列表里，所以保持简单清晰：
//   - 默认形态："导入完成 / 上传 N 条 / 成功 X 条 [/ 失败 Y 条] [/ 部分失败 Z 条]"
//   - 当 MergedCount > 0 时改用拆分形态："/ 新增 X 条 / 合并更新 Y 条"，
//     让用户在 append 模式下看到合并了多少条历史 FAQ 而不是只看到总成功数。
//
// 内部 master 原始实现；HEAD 版本之前没有，所有完成消息只有 "正在处理第 N/M 条"。
func (s *knowledgeService) buildFAQImportResultMessage(prefix string, progress *types.FAQImportProgress) string {
	parts := []string{prefix}
	parts = append(parts, fmt.Sprintf("上传 %d 条", progress.Total))

	if progress.MergedCount > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d 条", progress.AddedCount))
		parts = append(parts, fmt.Sprintf("合并更新 %d 条", progress.MergedCount))
	} else {
		parts = append(parts, fmt.Sprintf("成功 %d 条", progress.SuccessCount))
	}

	if progress.FailedCount > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d 条", progress.FailedCount))
	}
	if progress.PartialFailedCount > 0 {
		parts = append(parts, fmt.Sprintf("部分失败 %d 条", progress.PartialFailedCount))
	}

	return strings.Join(parts, " / ")
}

const (
	faqImportProgressKeyPrefix = "faq_import_progress:"
	faqImportRunningKeyPrefix  = "faq_import_running:"
	faqImportProgressTTL       = 3 * time.Hour
	// faqImportLeaseTTL is how long the knowledge base stays reserved without a
	// renewal: long enough that a crash is not mistaken for a slow import, and
	// short enough that a crashed worker frees the knowledge base without an
	// operator. A running import heartbeats at a third of this
	// (faqImportLeaseHeartbeatInterval), so an import that is alive keeps the
	// lease through a slow batch; renewals are ownership-checked, so a stale
	// lease cannot be silently extended by a different import.
)

// faqImportLeaseTTL is how long the knowledge base stays reserved without a
// renewal: long enough that a crash is not mistaken for a slow import, and
// short enough that a crashed worker frees the knowledge base without an
// operator. A running import heartbeats at a third of this
// (faqImportLeaseHeartbeatInterval). A variable so tests (and gated
// boundary probes) can shorten the window.
var faqImportLeaseTTL = 10 * time.Minute

// faqImportLeaseHeartbeatInterval is how often a running import refreshes its
// lease while it works. Progress saves also renew, but they happen once per
// batch: a batch that embeds and indexes slowly could outlive the lease, and
// another submission would then be admitted beside it. The heartbeat makes the
// lease follow the process's liveness instead of the batch cadence. A variable
// so tests can shorten it.
var faqImportLeaseHeartbeatInterval = faqImportLeaseTTL / 3

// getFAQImportProgressKey returns the Redis key for storing FAQ import progress
func getFAQImportProgressKey(taskID string) string {
	return faqImportProgressKeyPrefix + taskID
}

// getFAQImportRunningKey returns the Redis key for storing running task ID by KB ID
func getFAQImportRunningKey(kbID string) string {
	return faqImportRunningKeyPrefix + kbID
}

// saveFAQImportProgressOrStop saves progress, treating a lost lease as a reason
// to stop the import: once another task owns the knowledge base, continuing to
// write beside it is exactly what the lease exists to prevent. Other save
// failures stay non-fatal, as before -- progress is a liveness signal, and a
// transient Redis error should not fail an import that is otherwise fine.
func (s *knowledgeService) saveFAQImportProgressOrStop(
	ctx context.Context, progress *types.FAQImportProgress,
) error {
	if err := s.saveFAQImportProgress(ctx, progress); err != nil {
		if errors.Is(err, errFAQImportLeaseTaken) {
			// This attempt is over; asynq will archive it without retrying, so
			// finish the record here or the initiator keeps polling "processing".
			// Not when the taker is a newer instance of this same task id: that
			// record now belongs to it, and marking it would stop the run that
			// is actually still importing.
			if !s.faqImportLeaseHeldByAnotherInstanceOfSameTask(
				ctx, progress.KBID, progress.TaskID, progress.InstanceID) {
				s.markFAQImportStopped(ctx, progress.TaskID, progress.InstanceID, errFAQImportStoppedMessage)
			}
			return err
		}
		logger.Warnf(ctx, "Failed to save FAQ import progress: %v", err)
	}
	return nil
}

// faqImportStaleResult returns the completed-import result that makes this
// retry stale, or nil when the run may proceed.
func faqImportStaleResult(knowledge *types.Knowledge, payload *types.FAQImportPayload) *types.FAQImportResult {
	if knowledge == nil {
		return nil
	}
	result, err := knowledge.GetLastFAQImportResult()
	if err != nil || !faqImportRetryIsStale(payload, result) {
		return nil
	}
	return result
}

// refuseStaleFAQImport ends a replace retry that a newer import has outdated:
// it records a terminal state for the initiator and gives back its lease.
func (s *knowledgeService) refuseStaleFAQImport(
	ctx context.Context, payload *types.FAQImportPayload, result *types.FAQImportResult,
) error {
	const message = "该知识库在本任务入队后已完成另一次导入，本次重试已过期，请重新发起导入"
	logger.Warnf(ctx,
		"FAQ import task %s enqueued at %d is stale: import %s completed at %s",
		payload.TaskID, payload.EnqueuedAt, result.TaskID, result.ImportedAt)
	s.markFAQImportStopped(ctx, payload.TaskID, payload.InstanceID, message)
	if clearErr := s.clearRunningFAQImportInfoIfMatches(
		context.WithoutCancel(ctx), payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); clearErr != nil {
		logger.Warnf(ctx, "Failed to clear stale FAQ import running info: %v", clearErr)
	}
	// The task will never run, so its entries file is unreachable. The object
	// name carries the instance id, so this cannot be another submission's file.
	if payload.EntriesURL != "" && s.fileSvc != nil {
		if err := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), payload.EntriesURL); err != nil {
			logger.Warnf(ctx, "Failed to delete stale FAQ entries file %s: %v", payload.EntriesURL, err)
		}
	}
	return fmt.Errorf("%w: %s", asynq.SkipRetry, message)
}

// faqImportRetryIsStale reports whether a queued replace retry predates the
// last completed import. A retry preserves its enqueue time, so a completion at
// or after that second means the knowledge base has since been replaced by
// another import and this attempt must not delete anything. Append retries are
// never stale: they do not delete, and a completed import does not make their
// additions unsafe. A nil result or a zero timestamp says nothing.
func faqImportRetryIsStale(payload *types.FAQImportPayload, result *types.FAQImportResult) bool {
	if payload == nil || payload.DryRun || payload.Mode != types.FAQBatchModeReplace {
		return false
	}
	if result == nil {
		return false
	}
	// A retry that crashed after writing its own result resumes idempotently:
	// the stored numbers are this instance's, and re-running only rewrites the
	// same values. Fencing it would report "another import completed" over
	// data this very task imported. The identity must match on both legs
	// (task and instance); a result from before instance ids, or from a newer
	// instance of a reused task id, keeps the timestamp rule below.
	if payload.TaskID != "" && result.TaskID == payload.TaskID &&
		payload.InstanceID != "" && result.InstanceID == payload.InstanceID {
		return false
	}
	// Prefer milliseconds, which separate imports accepted and completed inside
	// one second. Results and payloads written by older builds only have
	// seconds, and a second-resolution comparison treats a completion in the
	// same second as stale -- the safe direction for a replace.
	enqueued := payload.EnqueuedAtMillis
	if enqueued <= 0 && payload.EnqueuedAt > 0 {
		enqueued = payload.EnqueuedAt * 1000
	}
	if enqueued <= 0 {
		return false
	}
	completed := result.ImportedAtMillis
	if completed <= 0 {
		if result.ImportedAt.IsZero() {
			return false
		}
		completed = result.ImportedAt.UnixMilli()
	}
	return completed >= enqueued
}

// errFAQImportStoppedMessage is shown to the initiator when another import
// took the knowledge base and this task stopped without finishing.
const errFAQImportStoppedMessage = "该知识库已由另一个导入任务接管，本任务已停止；请在当前导入结束后重新发起"

// errFAQImportUnverifiableMessage closes a run the strict gate could not
// verify: the task is archived without retry, so the initiator has to be told
// why its record stopped moving.
const errFAQImportUnverifiableMessage = "无法确认导入任务的所有权（租约存储暂时不可达），本任务已停止且不会重试；请确认 Redis 服务恢复后重新发起导入"

// saveFAQImportProgress saves the FAQ import progress to Redis.
//
// The lease is renewed *before* the record is written. The reverse order let a
// run that had already lost the knowledge base overwrite the record of the run
// that now owns it, and then discover the takeover: the new run would read its
// own task id paired with the stale instance, stop on the instance gate, and
// the initiator would watch a record that never moves. Ordering it this way, a
// losing run writes nothing.
func (s *knowledgeService) saveFAQImportProgress(ctx context.Context, progress *types.FAQImportProgress) error {
	// Every progress save is evidence the import is alive, which is what the
	// admission lease needs to distinguish a slow import from an abandoned one.
	// It is also the point where a takeover is noticed: continuing to write
	// after another import was admitted would defeat the lease entirely.
	if err := s.renewFAQImportLease(ctx, progress.KBID, progress.TaskID, progress.InstanceID, progress.EnqueuedAt); err != nil {
		return err
	}
	return s.writeFAQImportProgress(ctx, progress)
}

// writeFAQImportProgress stores the record without renewing the lease. Kept
// separate so a run that has already lost the lease can still finish its own
// record: the initiator polls that record, and leaving it in "processing" is
// what makes a superseded task wait forever.
func (s *knowledgeService) writeFAQImportProgress(ctx context.Context, progress *types.FAQImportProgress) error {
	progress.UpdatedAt = time.Now().Unix()
	if s.redisClient == nil {
		// Store a snapshot: the writer keeps mutating its own struct between
		// batches, and readers (poll handlers, the sweep) hold the map value.
		// sync.Map protects the container, not the fields behind the pointer.
		s.memFAQProgress.Store(progress.TaskID, cloneFAQImportProgress(progress))
		s.sweepMemFAQProgress()
		return nil
	}
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("failed to marshal FAQ import progress: %w", err)
	}
	return s.redisClient.Set(ctx, getFAQImportProgressKey(progress.TaskID), data, faqImportProgressTTL).Err()
}

// faqImportProgressExpired mirrors the TTL the Redis key gets. UpdatedAt is set
// on every write, so a record that is being polled or updated never expires.
// cloneFAQImportProgress copies a progress record for the in-memory map. The
// top-level fields and slice headers are what readers touch concurrently; the
// element values themselves are appended once and never mutated afterwards, so
// element-wise deep copying is not required.
func cloneFAQImportProgress(p *types.FAQImportProgress) *types.FAQImportProgress {
	if p == nil {
		return nil
	}
	c := *p
	// FAQFailedEntry carries its own string slices; copying only the outer slice
	// would leave the writer and a reader sharing the backing arrays.
	c.FailedEntries = cloneFAQFailedEntries(p.FailedEntries)
	c.SuccessEntries = slices.Clone(p.SuccessEntries)
	c.ValidEntryIndices = slices.Clone(p.ValidEntryIndices)
	c.MergeEntryIndices = slices.Clone(p.MergeEntryIndices)
	c.MergeDetails = slices.Clone(p.MergeDetails)
	return &c
}

func cloneFAQFailedEntries(in []types.FAQFailedEntry) []types.FAQFailedEntry {
	if in == nil {
		return nil
	}
	out := make([]types.FAQFailedEntry, len(in))
	copy(out, in)
	for i := range out {
		out[i].SimilarQuestions = slices.Clone(in[i].SimilarQuestions)
		out[i].NegativeQuestions = slices.Clone(in[i].NegativeQuestions)
		out[i].Answers = slices.Clone(in[i].Answers)
		out[i].RemovedSimilarQuestions = slices.Clone(in[i].RemovedSimilarQuestions)
		out[i].RemovedNegativeQuestions = slices.Clone(in[i].RemovedNegativeQuestions)
	}
	return out
}

func faqImportProgressExpired(progress *types.FAQImportProgress, now time.Time) bool {
	if progress.UpdatedAt <= 0 {
		return false
	}
	return now.Sub(time.Unix(progress.UpdatedAt, 0)) > faqImportProgressTTL
}

// sweepMemFAQProgress drops records that expired without being read. Lazy
// expiry in GetFAQImportProgress is not enough: an initiator that stops
// polling would otherwise leave its record for the life of the process.
func (s *knowledgeService) sweepMemFAQProgress() {
	now := time.Now()
	s.memFAQProgress.Range(func(key, value any) bool {
		progress, _ := value.(*types.FAQImportProgress)
		if progress == nil || faqImportProgressExpired(progress, now) {
			s.memFAQProgress.CompareAndDelete(key, value)
		}
		return true
	})
}

// tenantIDFromContext reads the tenant a task runs under. Both the worker
// context and an HTTP request carry it in the same key.
func tenantIDFromContext(ctx context.Context) uint64 {
	id, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	return id
}

// markFAQImportStopped ends this task's record with a failure. Used once the
// KB belongs to another import: this task will not run again, so its initiator
// has to see a terminal state rather than a progress record that never moves.
// The running marker is deliberately left alone -- it names the other task.
// instanceID identifies the caller's import instance. A task id is reusable
// across submissions, and the record of another instance of the same task id
// belongs to the run that is actually importing: it is left untouched.
func (s *knowledgeService) markFAQImportStopped(ctx context.Context, taskID, instanceID, message string) {
	// The initiator needs this record more than the caller needs to stay
	// cancelled: asynq cancels the task context on shutdown, and a shutdown is
	// exactly when a superseded run is most likely to be discovered.
	ctx = context.WithoutCancel(ctx)
	progress := &types.FAQImportProgress{
		TaskID: taskID, CreatedAt: time.Now().Unix(), TenantID: tenantIDFromContext(ctx),
	}
	if existing, err := s.GetFAQImportProgress(ctx, taskID); err == nil && existing != nil {
		if instanceID != "" && existing.InstanceID != "" && existing.InstanceID != instanceID {
			return
		}
		progress = existing
	}
	progress.Status = types.FAQImportStatusFailed
	progress.Message = message
	progress.Error = message
	if err := s.writeFAQImportProgress(ctx, progress); err != nil {
		logger.Warnf(ctx, "Failed to record stopped FAQ import task %s: %v", taskID, err)
	}
}

// refuseFAQImportTask ends a task that cannot succeed on a retry: it closes the
// record, gives the lease back (compare-and-delete only removes this instance's
// own marker) and drops the object-storage payload that can never be accepted.
func (s *knowledgeService) refuseFAQImportTask(
	ctx context.Context, payload *types.FAQImportPayload, message, entriesURL string,
) {
	s.markFAQImportStopped(ctx, payload.TaskID, payload.InstanceID, message)
	if clearErr := s.clearRunningFAQImportInfoIfMatches(
		context.WithoutCancel(ctx), payload.KBID, payload.TaskID, payload.InstanceID, payload.EnqueuedAt); clearErr != nil {
		logger.Warnf(ctx, "Failed to clear FAQ import running info after refusing task %s: %v", payload.TaskID, clearErr)
	}
	if entriesURL != "" && s.fileSvc != nil {
		if err := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), entriesURL); err != nil {
			logger.Warnf(ctx, "Failed to delete refused FAQ entries file %s: %v", entriesURL, err)
		}
	}
}

// GetFAQImportProgress retrieves the progress of an FAQ import task
func (s *knowledgeService) GetFAQImportProgress(ctx context.Context, taskID string) (*types.FAQImportProgress, error) {
	if s.redisClient == nil {
		if v, ok := s.memFAQProgress.Load(taskID); ok {
			progress, _ := v.(*types.FAQImportProgress)
			// Mirror the Redis key's TTL: the in-memory map has none of its own,
			// and a record nobody asks for again would live for the process.
			if progress == nil || faqImportProgressExpired(progress, time.Now()) {
				s.memFAQProgress.CompareAndDelete(taskID, v)
			} else {
				// Hand out a copy: the poller's mutations (and the JSON encoder
				// reading while the worker writes) must not touch the record.
				return cloneFAQImportProgress(progress), nil
			}
		}
		return nil, werrors.NewNotFoundError("FAQ import task not found")
	}
	key := getFAQImportProgressKey(taskID)
	data, err := s.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, werrors.NewNotFoundError("FAQ import task not found")
		}
		return nil, fmt.Errorf("failed to get FAQ import progress from Redis: %w", err)
	}

	var progress types.FAQImportProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return nil, fmt.Errorf("failed to unmarshal FAQ import progress: %w", err)
	}

	// If task is completed, enrich with persisted result fields from database.
	//
	// Only the record of the import that wrote that result may be enriched.
	// Two kinds of record must not be:
	//
	//   - a dry run, which never persists a result; copying the last real
	//     import's counts over it answers "is this file sound?" with numbers
	//     that describe something else entirely;
	//   - a completed record for any other task. The persisted result names
	//     its TaskID, and an older task's Redis record still reads "completed"
	//     for its whole TTL, so without the check a query about last week's
	//     task would return this afternoon's import.
	//
	// A result written before the TaskID field existed has no identity to
	// compare, and is still applied: its record is the only source of the
	// display fields, and refusing it would leave old imports without them.
	if progress.Status == types.FAQImportStatusCompleted && progress.KnowledgeID != "" && !progress.DryRun {
		tenantID := progress.TenantID
		if tenantID == 0 {
			// Records written before the field existed, and in-process records
			// from the same tenant, fall back to the caller's own tenant.
			tenantID, _ = ctx.Value(types.TenantIDContextKey).(uint64)
		}
		knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, progress.KnowledgeID)
		if err == nil && knowledge != nil {
			if result, err := knowledge.GetLastFAQImportResult(); err == nil && result != nil &&
				(result.TaskID == "" || result.TaskID == progress.TaskID) {
				progress.SuccessCount = result.SuccessCount
				progress.FailedCount = result.FailedCount
				progress.PartialFailedCount = result.PartialFailedCount
				progress.SkippedCount = result.SkippedCount
				progress.MergedCount = result.MergedCount
				progress.AddedCount = result.AddedCount
				progress.ImportMode = result.ImportMode
				progress.ImportedAt = result.ImportedAt
				progress.DisplayStatus = result.DisplayStatus
				progress.ProcessingTime = result.ProcessingTime
				if result.FailedEntriesURL != "" {
					progress.FailedEntriesURL = result.FailedEntriesURL
				}
			}
		}
	}

	return &progress, nil
}

// UpdateLastFAQImportResultDisplayStatus updates the display status of FAQ import result
func (s *knowledgeService) UpdateLastFAQImportResultDisplayStatus(ctx context.Context, kbID string, displayStatus string) error {
	// 验证displayStatus参数
	if displayStatus != "open" && displayStatus != "close" {
		return werrors.NewBadRequestError("invalid display status, must be 'open' or 'close'")
	}

	kb, ctx, err := s.writableFAQKnowledgeBase(ctx, kbID)
	if err != nil {
		return err
	}
	tenantID := kb.TenantID

	// 查找FAQ类型的knowledge
	knowledgeList, err := s.repo.ListKnowledgeByKnowledgeBaseID(ctx, tenantID, kbID)
	if err != nil {
		return fmt.Errorf("failed to list knowledge: %w", err)
	}

	// 查找FAQ类型的knowledge
	var faqKnowledge *types.Knowledge
	for _, k := range knowledgeList {
		if k.Type == types.KnowledgeTypeFAQ {
			faqKnowledge = k
			break
		}
	}

	if faqKnowledge == nil {
		return werrors.NewNotFoundError("FAQ knowledge not found in this knowledge base")
	}

	// 解析当前的导入结果
	result, err := faqKnowledge.GetLastFAQImportResult()
	if err != nil {
		return fmt.Errorf("failed to parse FAQ import result: %w", err)
	}

	if result == nil {
		return werrors.NewNotFoundError("no FAQ import result found")
	}

	// 更新显示状态
	result.DisplayStatus = displayStatus

	// 保存更新后的结果
	if err := faqKnowledge.SetLastFAQImportResult(result); err != nil {
		return fmt.Errorf("failed to set FAQ import result: %w", err)
	}

	// 更新数据库
	if err := s.repo.UpdateKnowledge(ctx, faqKnowledge); err != nil {
		return fmt.Errorf("failed to update knowledge: %w", err)
	}

	return nil
}
