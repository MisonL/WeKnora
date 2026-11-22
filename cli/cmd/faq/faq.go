// Package faq holds the `weknora faq` command tree: import / export /
// progress / wait. It exists because the FAQ import endpoint is asynchronous
// and has semantics a raw HTTP call does not convey: `replace` deletes
// everything outside the submitted set, the knowledge base admits one import
// at a time, and the progress record reports the last completed batch rather
// than the one just sent. Encoding that in the CLI is what lets other callers
// import FAQ without rediscovering those rules.
package faq

import (
	"github.com/spf13/cobra"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
)

// NewCmd builds the `weknora faq` parent command.
func NewCmd(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "faq",
		Short: "Import, export and track FAQ entries",
		Long: `Manage the FAQ entries of a knowledge base.

The import endpoint is asynchronous and has rules a raw POST does not show:

  * append adds entries; replace makes the submitted set the entire list, so
    every entry it does not carry is deleted.
  * Only one import may run per knowledge base at a time.
  * After a batch completes, the progress record reports that batch. A caller
    that submits several batches must wait for each before reading its result.
  * The entry count and body size per request are capped by the server; raise
    the caps or submit complete sets rather than splitting a replace.

'faq import' applies those rules itself. Prefer it over 'weknora api' for
imports.`,
	}
	cmd.AddCommand(NewCmdImport(f))
	cmd.AddCommand(NewCmdExport(f))
	cmd.AddCommand(NewCmdProgress(f))
	cmd.AddCommand(NewCmdWait(f))
	return cmd
}
