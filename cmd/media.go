// SPDX-License-Identifier: AGPL-3.0-or-later

package cmd

import (
	"encoding/json"
	"errors"

	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"

	"github.com/spf13/cobra"
)

var mediaCmd = newMediaCommand()

func newMediaCommand() *cobra.Command {
	parent := &cobra.Command{
		Use: "media", Short: "Inspect retained media evidence without changing data",
		PersistentPreRun: skipRuntimeInitialization,
	}
	reconcile := &cobra.Command{
		Use: "reconcile JOB_ID...", Short: "Produce a dry-run inventory of explicitly selected historical jobs",
		Long: "Read known Hister job request/result/manifest reports and provenance sidecars from an explicit state directory, and validate the named media files under an explicit media root. Always dry-run: does not load runtime configuration, open a database/index, contact a server, run a downloader, authorize/register artifacts, or move/delete files. Consistent evidence still requires explicit owner and pair approval plus live-source/registry checks; reports alone never prove Hister ownership. There is no apply mode.",
		Args: cobra.RangeArgs(1, 1000),
		RunE: func(cmd *cobra.Command, ids []string) error {
			cmd.SilenceUsage = true
			state, _ := cmd.Flags().GetString("state-dir")
			root, _ := cmd.Flags().GetString("media-root")
			if state == "" || root == "" {
				return errors.New("--state-dir and --media-root are required; no runtime paths are guessed")
			}
			plan, err := yt.PlanHistory(cmd.Context(), state, root, ids)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(plan)
		},
	}
	reconcile.Flags().String("state-dir", "", "Existing Hister yt_video_downloader cache directory containing artifacts/<job>/jobs/<job> reports")
	reconcile.Flags().String("media-root", "", "Existing configured media output root containing the named job directories")
	parent.AddCommand(reconcile)
	return parent
}
