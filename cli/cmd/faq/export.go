package faq

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	sdk "github.com/Tencent/WeKnora/client"
	"github.com/spf13/cobra"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
)

// FAQService is the narrow SDK surface these commands depend on.
type FAQService interface {
	ListFAQEntries(ctx context.Context, kbID string, page, pageSize int, tagSeqID int64, keyword, searchField, sortOrder string) (*sdk.FAQEntriesPage, error)
	UpsertFAQEntries(ctx context.Context, kbID string, payload *sdk.FAQBatchUpsertPayload) (string, error)
	GetFAQImportProgress(ctx context.Context, taskID string) (*sdk.FAQImportProgress, error)
}

// exportPageSize is the server's page size for FAQ entries. The list endpoint
// returns at most this many rows per call.
const exportPageSize = 20

type exportOptions struct {
	KB string
	// Out writes the entries to a file instead of stdout. The snapshot shape is
	// the import input shape, so the file can be fed straight back to
	// `faq import`.
	Out string
}

// NewCmdExport builds `weknora faq export`.
func NewCmdExport(f *cmdutil.Factory) *cobra.Command {
	opts := &exportOptions{}
	cmd := &cobra.Command{
		Use:   "export --kb <id>",
		Short: "Export a knowledge base's FAQ entries as an import payload",
		Long: `Write every FAQ entry of a knowledge base as a JSON array in the import
payload's entry shape (standard_question / similar_questions / answers / ...).

The output is a snapshot for backup or for feeding back to 'faq import'. It
carries no import state. Entry ids are written as the optional import "id"
field, so a snapshot can be re-imported where an id is still free; the server
drops any id that is already taken rather than failing the import.

The list endpoint pages 20 rows at a time, so a large knowledge base takes one
request per twenty entries.`,
		Example: `  weknora faq export --kb kb_abc --out faq-backup.json
  weknora faq export --kb kb_abc | jq length`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fopts, err := cmdutil.CheckFormatFlag(c)
			if err != nil {
				return err
			}
			fopts.ResolveDefault(iostreams.IO.IsStdoutTTY())
			if err := requireKB(opts.KB); err != nil {
				return err
			}
			cli, err := f.Client()
			if err != nil {
				return err
			}
			return runExport(c.Context(), opts, cli)
		},
	}
	cmd.Flags().StringVar(&opts.KB, "kb", "", "Knowledge base id (required)")
	cmd.Flags().StringVar(&opts.Out, "out", "", "Write the snapshot to this file instead of stdout")
	cmdutil.SetAgentHelp(cmd, cmdutil.AgentHelp{
		UsedFor:  "Snapshot the FAQ entries of a knowledge base before a destructive operation, or to migrate them elsewhere.",
		Examples: []string{"weknora faq export --kb kb_abc --out faq-backup.json"},
		Output:   "A JSON array of entry payloads in the import shape, so the file can be passed to 'faq import --mode replace' unchanged.",
	})
	return cmd
}

func runExport(ctx context.Context, opts *exportOptions, svc FAQService) error {
	all, err := exportEntries(ctx, svc, opts.KB)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("encode export: %v", err))
	}
	data = append(data, '\n')
	if opts.Out == "" {
		_, err = iostreams.IO.Out.Write(data)
		return err
	}
	if err := writeFileAtomic(opts.Out, data); err != nil {
		return cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("write %s: %v", opts.Out, err))
	}
	fmt.Fprintf(iostreams.IO.Err, "wrote %d entries to %s\n", len(all), opts.Out)
	return nil
}

// exportEntries pages through the list endpoint until it has every row.
//
// The listing is offset-paginated in (updated_at, id) order, so a knowledge
// base that changes while it is being read can shift rows across page
// boundaries: a delete in an already-read prefix moves a row that has not been
// read yet into the page before it, and the export silently misses it while
// keeping the row that was deleted. Deduplicating ids and comparing the final
// count against the final total cannot detect that -- the two numbers agree.
//
// So the total from the first page is the reference: any later page reporting a
// different total means the knowledge base changed under the read, and the
// export fails rather than hand back a snapshot that is not the state it says
// it is. The caller retries while the knowledge base is idle. A snapshot missing
// rows is worse than no snapshot: fed to `faq import --mode replace` it deletes
// the entries it does not list.
func exportEntries(ctx context.Context, svc FAQService, kbID string) ([]sdk.FAQEntryPayload, error) {
	out := make([]sdk.FAQEntryPayload, 0, exportPageSize)
	seen := make(map[int64]struct{})
	var total int64
	fail := func(format string, args ...any) error {
		return cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf(format, args...))
	}
	finish := func() ([]sdk.FAQEntryPayload, error) {
		if total > int64(len(out)) {
			return nil, fail(
				"export read %d of %d entries; the knowledge base changed while it was being read, retry while it is idle",
				len(out), total)
		}
		return out, nil
	}
	for page := 1; ; page++ {
		res, err := svc.ListFAQEntries(ctx, kbID, page, exportPageSize, 0, "", "", "asc")
		if err != nil {
			return nil, cmdutil.WrapHTTP(err, "export FAQ entries")
		}
		if page == 1 {
			total = res.Total
		} else if res.Total != total {
			// The total moved: rows were added or removed under the read, so
			// the pages are not slices of one consistent snapshot.
			return nil, fail(
				"export aborted at page %d: the knowledge base changed while it was being read (%d entries became %d); retry while it is idle",
				page, total, res.Total)
		}
		rows := res.Entries
		if len(rows) == 0 {
			return finish()
		}
		added := 0
		for _, e := range rows {
			if _, dup := seen[e.ID]; dup {
				continue
			}
			seen[e.ID] = struct{}{}
			out = append(out, payloadFromEntry(e))
			added++
		}
		// A page that adds nothing means the endpoint is not advancing (for
		// example because an entry was inserted at the front mid-export), and
		// continuing would loop over the same rows.
		if added == 0 {
			return finish()
		}
		if len(rows) < exportPageSize {
			return finish()
		}
	}
}

// payloadFromEntry converts a listed entry back into the import shape, so an
// export can be re-imported without loss.
func payloadFromEntry(e sdk.FAQEntry) sdk.FAQEntryPayload {
	enabled := e.IsEnabled
	recommended := e.IsRecommended
	p := sdk.FAQEntryPayload{
		StandardQuestion:  e.StandardQuestion,
		SimilarQuestions:  e.SimilarQuestions,
		NegativeQuestions: e.NegativeQuestions,
		Answers:           e.Answers,
		IsEnabled:         &enabled,
		IsRecommended:     &recommended,
	}
	// The client's seq id is informational for a snapshot but part of the
	// entry it came from, and the server accepts it on import where it cannot
	// collide (it drops it otherwise). Keeping it makes a round trip faithful
	// and matches what the server's own JSON export writes.
	if e.ID != 0 {
		id := e.ID
		p.ID = &id
	}
	if e.AnswerStrategy != "" {
		strategy := e.AnswerStrategy
		p.AnswerStrategy = &strategy
	}
	if e.TagID != 0 {
		p.TagID = e.TagID
	}
	if e.TagName != "" {
		p.TagName = e.TagName
	}
	return p
}

// writeFileAtomic writes through a temporary file in the same directory so an
// interrupted export never leaves a half-written snapshot where the previous
// good one was. The caller may be overwriting a backup, and a truncated backup
// is worse than no backup.
func writeFileAtomic(path string, data []byte) error {
	dir := "."
	if i := lastSlash(path); i >= 0 {
		dir = path[:i]
		if dir == "" {
			dir = "/"
		}
	}
	tmp, err := os.CreateTemp(dir, ".faq-export-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func requireKB(kb string) error {
	if kb == "" {
		return &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "--kb is required"}
	}
	return nil
}

// exitCodeFor maps a finished import's progress to the shared wait exit codes.
// A partial failure is an unsuccessful import too: the entry is searchable, but
// not with the questions the caller supplied.
func exitCodeFor(p *sdk.FAQImportProgress) int {
	if p == nil {
		return 1
	}
	switch p.Status {
	case "completed":
		if p.FailedCount > 0 || p.PartialFailedCount > 0 {
			return 1
		}
		return 0
	default:
		return 1
	}
}

// errSilentExit is how a command reports an outcome that is already on stdout.
// PrintError skips a second envelope, so callers get one partition and an exit
// code (the same contract doc wait documents).
func silentExit(code int, msg string) error {
	c := cmdutil.CodeOperationFailed
	if code == 124 {
		c = cmdutil.CodeOperationTimeout
	}
	return cmdutil.NewError(c, msg).WithSilent()
}
