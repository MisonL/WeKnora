package im

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	searchMaxResults    = 5
	searchContentMaxLen = 200 // runes shown per result
	// searchTimeout bounds a single /search execution: it hits the embedding
	// pipeline and must not run unbounded inside the synchronous command path.
	searchTimeout = 30 * time.Second
	// searchMaxConcurrency caps concurrent /search executions process-wide so
	// a burst of commands cannot saturate the embedding backend the way the
	// queued QA pipeline (worker pool) is protected.
	searchMaxConcurrency = 2
)

// searchSlots is the process-wide concurrency gate for /search executions.
var searchSlots = make(chan struct{}, searchMaxConcurrency)

// SearchCommand implements /search <query>.
//
// It runs a hybrid search (vector + keywords) against the user's selected
// knowledge bases—or the bot-level defaults when no override is active—and
// returns the raw matching passages without AI summarisation. This is useful
// when the user needs to inspect source text directly.
type SearchCommand struct {
	sessionService interfaces.SessionService
	kbService      interfaces.KnowledgeBaseService
}

func newSearchCommand(sessionService interfaces.SessionService, kbService interfaces.KnowledgeBaseService) *SearchCommand {
	return &SearchCommand{sessionService: sessionService, kbService: kbService}
}

func (c *SearchCommand) Name() string { return "search" }
func (c *SearchCommand) Description() string {
	return "直接检索知识库原文（不经 AI 总结），例如：/search 退款政策"
}

func (c *SearchCommand) Execute(ctx context.Context, cmdCtx *CommandContext, args []string) (*CommandResult, error) {
	if len(args) == 0 {
		return &CommandResult{
			Content: "请输入搜索内容，例如：`/search 退款政策`",
		}, nil
	}

	query := strings.Join(args, " ")

	// Bound time and concurrency: /search runs synchronously in the message
	// path (outside the QA queue), so without a gate one command could occupy
	// unbounded embedding capacity.
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	select {
	case searchSlots <- struct{}{}:
		defer func() { <-searchSlots }()
	default:
		return &CommandResult{
			Content: "当前检索请求较多，请稍后再试。",
		}, nil
	}

	// Resolve which KBs to search, mirroring the logic in the QA pipeline's
	// resolveKnowledgeBasesFromAgent so that /search covers the same scope.
	var kbIDs []string
	if cmdCtx.CustomAgent != nil {
		switch cmdCtx.CustomAgent.Config.KBSelectionMode {
		case "all":
			allKBs, err := c.kbService.ListKnowledgeBases(ctx)
			if err == nil {
				// Same capability filter as the QA pipeline's
				// resolveKnowledgeBasesFromAgent (`all` branch) so `/search`
				// agrees with what the agent's tools can actually reach.
				// Agent-mode aware: quick-answer enforces RAG-only KBs.
				agentMode := cmdCtx.CustomAgent.Config.AgentMode
				allowed := cmdCtx.CustomAgent.Config.AllowedTools
				filter := tools.DeriveKBFilterForAgent(agentMode, allowed)
				skipped := 0
				for _, kb := range allKBs {
					if !filter.IsEmpty() &&
						!tools.KBSatisfiesAgentRequirements(kb.Capabilities(), agentMode, allowed) {
						skipped++
						continue
					}
					kbIDs = append(kbIDs, kb.ID)
				}
				if skipped > 0 {
					logger.Infof(ctx,
						"/search(agent=%s, mode=all): capability filter removed %d of %d KBs",
						cmdCtx.CustomAgent.ID, skipped, len(allKBs))
				}
			}
		case "none":
			// No knowledge bases configured — will return empty results.
		case "selected":
			kbIDs = cmdCtx.CustomAgent.Config.KnowledgeBases
		default:
			// Backward compatibility: fall back to configured list.
			kbIDs = cmdCtx.CustomAgent.Config.KnowledgeBases
		}
	}

	retrieval, err := c.sessionService.SearchKnowledge(ctx, kbIDs, nil, nil, query, nil)
	if err != nil {
		if ctx.Err() != nil {
			// 区分两种取消：/stop 是用户主动中止，报“超时”会让人以为系统出错。
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return &CommandResult{
					Content: fmt.Sprintf("检索超过 %s 未返回，请稍后再试。", searchTimeout),
				}, nil
			}
			return &CommandResult{Content: "已取消检索。"}, nil
		}
		return nil, fmt.Errorf("search knowledge: %w", err)
	}
	results := retrieval.Results

	if len(results) == 0 {
		return &CommandResult{
			Content: fmt.Sprintf("未在知识库中找到与「%s」相关的内容。", query),
		}, nil
	}

	// Cap the number of results shown in IM (wall of text is unhelpful).
	shown := results
	if len(shown) > searchMaxResults {
		shown = shown[:searchMaxResults]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 **搜索「%s」** — 找到 %d 条结果\n\n", query, len(results)))

	for i, r := range shown {
		// Trim content to a readable length.
		content := []rune(r.Content)
		suffix := ""
		if len(content) > searchContentMaxLen {
			content = content[:searchContentMaxLen]
			suffix = "…"
		}

		// Source label: prefer title, fall back to filename.
		source := r.KnowledgeTitle
		if source == "" {
			source = r.KnowledgeFilename
		}

		sb.WriteString(fmt.Sprintf("**[%d]** %s\n> %s%s\n", i+1, source, string(content), suffix))

		if r.Score > 0 {
			sb.WriteString(fmt.Sprintf("匹配度：%.0f%%\n", r.Score*100))
		}
		sb.WriteString("\n")
	}

	if len(results) > searchMaxResults {
		sb.WriteString(fmt.Sprintf("_（仅显示前 %d 条，共 %d 条）_", searchMaxResults, len(results)))
	}

	return &CommandResult{Content: sb.String()}, nil
}
