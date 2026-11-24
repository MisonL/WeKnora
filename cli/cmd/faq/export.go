package faq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

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

// exportPageSize is the page size the export asks for. The list endpoint
// accepts up to 1000 per call; 100 keeps a full pass to a handful of requests
// without a single response growing large enough to dominate memory.
const exportPageSize = 100

// maxExportPasses caps how many full collections the export may run looking for
// two consecutive identical ones. The list is offset-paginated, so any
// concurrent modification can shift rows across page boundaries and mix two
// states into one snapshot. Two identical consecutive passes are the strongest
// signal this endpoint offers that the knowledge base was idle throughout; the
// check is a heuristic, not a consistency guarantee -- a writer that happens to
// keep both passes in the same mixed state cannot be told apart from an idle
// knowledge base. A knowledge base that has not settled after this many reads
// is refused rather than handed back as a snapshot of nothing in particular.
const maxExportPasses = 3

// errExportUnstable marks a pass whose own pages disagreed with each other.
// Such a pass mixes two states and is useless as a snapshot, but a later full
// read still can be.
var errExportUnstable = errors.New("the knowledge base changed while it was being read")

// unstablePassError reports one mixed pass with the CLI's operation-failed code
// while staying detectable by errors.Is.
func unstablePassError(message string) error {
	e := cmdutil.NewError(cmdutil.CodeOperationFailed, message)
	e.Cause = errExportUnstable
	return e
}

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
field. The server keeps one only below its sequence start (100000000) and only
when no row already holds it, and a replace-mode import keeps none -- so the
ids survive on SQLite deployments but not on PostgreSQL, where every id this
server assigns is at or above the sequence start. Tags travel by tag_name
only: the server resolves an import's tag by tag_id first when that id still
exists in the target knowledge base, so a carried-over tag id could collide
with a different tag after a migration; names resolve back to the right tag in
a same-tenant round trip and cannot collide across tenants. Renaming a tag
between export and import is therefore not followed.

Offset paging is not stable under concurrent modification, so the export reads
the whole list at least twice and only writes a snapshot two consecutive reads
agreed on. A knowledge base that keeps changing under the read is refused
(exit 1) rather than exported as a mixed snapshot.`,
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

// exportEntries collects the knowledge base's FAQ entries as a snapshot,
// reading the list at least twice and only accepting content two consecutive
// full reads agree on.
//
// The listing is offset-paginated in (updated_at, id) order, so a knowledge
// base that changes while it is being read can shift rows across page
// boundaries: a delete in an already-read prefix moves a row that has not been
// read yet into the page before it, and the export silently misses it while
// keeping the row that was deleted. Deduplicating ids and comparing the final
// count against the final total cannot detect that -- the two numbers agree.
// The worst case is balanced: delete one row from the already-read prefix and
// insert one into the unread tail and even the total stays put, while the
// pages were slices of two different states.
//
// A single consistent read is therefore impossible to obtain from the endpoint
// as it stands, and the strongest available signal is repetition: collect
// everything (exportPass), then re-collect in the same order and compare entry
// by entry on normalised JSON. Two identical consecutive passes mean the
// knowledge base did not move while either was read; content is only handed
// back then. If two passes disagree the list was being modified under the
// read and the export re-collects, up to maxExportPasses reads in total; a
// knowledge base that never settles is refused -- a snapshot mixing two
// states, fed to `faq import --mode replace`, deletes the entries it does not
// list, so a wrong snapshot is worse than no snapshot. Within each pass the
// total from page 1 is still the reference: a pass whose own pages report
// different totals cannot be compared with anything and is retried like any
// other disagreeing read, but it never becomes the snapshot. The caller
// retries while the knowledge base is idle.
func exportEntries(ctx context.Context, svc FAQService, kbID string) ([]sdk.FAQEntryPayload, error) {
	var previous []sdk.FAQEntryPayload
	var unstable error
	for pass := 1; pass <= maxExportPasses; pass++ {
		current, err := exportPass(ctx, svc, kbID, pass)
		if err != nil {
			if errors.Is(err, errExportUnstable) {
				// This read mixed two states, so it cannot be a candidate. The
				// previous stable read stays as the comparison point: a later
				// full read that matches it is the same evidence two consecutive
				// passes would have given.
				unstable = err
				continue
			}
			return nil, err
		}
		if previous != nil && snapshotsEqual(previous, current) {
			return current, nil
		}
		previous = current
	}
	if unstable != nil {
		return nil, unstable
	}
	return nil, cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf(
		"export aborted after %d full reads: the knowledge base changed while it was being read (no two consecutive reads returned the same entries); retry while it is idle",
		maxExportPasses))
}

// exportPass pages through the list endpoint once, until it has every row.
// passNum only identifies the pass inside error messages.
func exportPass(ctx context.Context, svc FAQService, kbID string, passNum int) ([]sdk.FAQEntryPayload, error) {
	out := make([]sdk.FAQEntryPayload, 0, exportPageSize)
	seen := make(map[int64]struct{})
	var total int64
	fail := func(format string, args ...any) error {
		return unstablePassError(fmt.Sprintf(format, args...))
	}
	finish := func() ([]sdk.FAQEntryPayload, error) {
		// Either direction means the pages do not add up to the total the first
		// page reported: rows were removed (fewer read) or added (more read)
		// while the pass ran. Both mix two states and are retried.
		if total != int64(len(out)) {
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
				"export aborted at pass %d page %d: the knowledge base changed while it was being read (%d entries became %d); retry while it is idle",
				passNum, page, total, res.Total)
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

// snapshotsEqual compares two full collections entry by entry on normalised
// JSON. The comparison is per entry rather than of the concatenated bytes:
// an irrelevant difference in JSON key order between responses must not look
// like the knowledge base changed, while any real content difference -- and
// any reordering, since both passes page through the same (updated_at, id)
// order -- must.
func snapshotsEqual(a, b []sdk.FAQEntryPayload) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		rawA, errA := json.Marshal(a[i])
		rawB, errB := json.Marshal(b[i])
		if errA != nil || errB != nil {
			return false
		}
		equal, err := canonicalJSONEqual(rawA, rawB)
		if err != nil || !equal {
			return false
		}
	}
	return true
}

// canonicalJSONEqual reports whether two JSON values are equal ignoring
// object key order. A payload that cannot be canonicalised is reported as an
// error, which snapshotsEqual treats as a mismatch (fail closed: an export
// that cannot prove two passes agree does not hand back a snapshot).
func canonicalJSONEqual(a, b json.RawMessage) (bool, error) {
	ca, err := canonicalJSON(a)
	if err != nil {
		return false, err
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// canonicalJSON re-encodes a JSON value with every object's keys sorted, so
// two payloads that differ only in key order produce identical bytes.
// Numbers keep their original text (UseNumber), so no value is altered by the
// round trip.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, decoded); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
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
	// The client's seq id is part of the entry it came from and is kept in the
	// snapshot, but only a value below the server's sequence start can ever be
	// preserved: the server drops an id at or above FAQExternalSeqIDLimit
	// (100000000) and an id already held by a row, and a replace-mode import
	// drops every id. On PostgreSQL, where the sequence starts at that limit,
	// every id this server assigns is therefore dropped on import and the row
	// gets a fresh one; SQLite deployments (ids from 1) do keep them.
	if e.ID != 0 {
		id := e.ID
		p.ID = &id
	}
	if e.AnswerStrategy != "" {
		strategy := e.AnswerStrategy
		p.AnswerStrategy = &strategy
	}
	// Deliberately no tag_id: the server resolves an import's tag by seq_id
	// FIRST when that id exists and belongs to the target KB, using tag_name
	// only as a fallback (internal/application/service/knowledge_faq.go
	// resolveTagID). A carried-over tag_id is therefore unsafe across a
	// tenant/KB migration -- the source seq_id can collide with a
	// same-numbered but different tag in the destination and file the entry
	// under the wrong classification. Tag identity travels by name, not by
	// id: the same-tenant round trip resolves the name back to the original
	// id, and a migration cannot be hijacked by an id collision.
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
	// CreateTemp opens 0600; a snapshot replaces a backup file the caller
	// already has, so match the mode an ordinary write would have given it.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Persist the directory entry too: without it a power loss can keep the
	// old name pointing at the old content after the data blocks are on disk.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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
