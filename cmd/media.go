// SPDX-License-Identifier: AGPL-3.0-or-later

package cmd

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/asciimoo/hister/client"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"

	"github.com/spf13/cobra"
)

var mediaCmd = newMediaCommand()

func newMediaCommand() *cobra.Command {
	parent := &cobra.Command{
		Use: "media", Short: "Inspect retained media or restore explicitly approved history",
		PersistentPreRun: skipRuntimeInitialization,
	}
	reconcile := &cobra.Command{
		Use: "reconcile JOB_ID...", Short: "Produce a dry-run inventory of explicitly selected historical jobs",
		Long: "Read known Hister job request/result/manifest reports and provenance sidecars from an explicit state directory, and validate the named media files under an explicit media root. Always dry-run: does not load runtime configuration, open a database/index, contact a server, run a downloader, authorize/register artifacts, or move/delete files. Consistent evidence still requires explicit owner and pair approval plus live-source/registry checks; reports alone never prove Hister ownership.",
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
	check := &cobra.Command{
		Use:   "check",
		Short: "Read-only server preflight for an approved historical media pair",
		Long:  "Send one strict approval document to the configured server. The server revalidates the live source, retained reports, media hashes and registry without writing SQL rows. The returned confirmation hash is a stale-plan guard, not a reservation or authorization credential.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			approval, err := readMediaHistoryApproval(cmd)
			if err != nil {
				return err
			}
			c, _, err := inspectConfig()
			if err != nil {
				return err
			}
			result, err := newClientForConfig(c).CheckMediaHistory(cmd.Context(), approval)
			if err != nil {
				return err
			}
			return writeMediaHistoryJSON(cmd, result)
		},
	}
	check.Flags().String("approval", "", "Strict approval JSON file reviewed by an operator")
	_ = check.MarkFlagRequired("approval")

	apply := &cobra.Command{
		Use:   "apply",
		Short: "Register the exact historical media pair after a successful check",
		Long:  "Apply one strict approval through the configured server. Requires the exact confirmation hash from `hister media check` and --yes. The server revalidates source, evidence, bytes, roots and registry; a check is not a reservation, and stale plans fail without writing. Registration preserves the current selection and never downloads or re-indexes.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				return errors.New("--yes is required to apply a historical media approval")
			}
			confirmation, _ := cmd.Flags().GetString("confirmation")
			if !validMediaHistoryConfirmation(confirmation) {
				return errors.New("--confirmation must be the 64-character lowercase SHA-256 from a recent media check")
			}
			approval, err := readMediaHistoryApproval(cmd)
			if err != nil {
				return err
			}
			c, _, err := inspectConfig()
			if err != nil {
				return err
			}
			result, err := newClientForConfig(c).ApplyMediaHistory(cmd.Context(), approval, confirmation)
			if err != nil {
				return err
			}
			return writeMediaHistoryJSON(cmd, result)
		},
	}
	apply.Flags().String("approval", "", "Strict approval JSON file reviewed by an operator")
	apply.Flags().String("confirmation", "", "Exact confirmation SHA-256 returned by media check")
	apply.Flags().Bool("yes", false, "Confirm this checked historical registration")
	_ = apply.MarkFlagRequired("approval")
	_ = apply.MarkFlagRequired("confirmation")

	bootstrapCheck := &cobra.Command{
		Use:   "bootstrap-check",
		Short: "Read-only admin check before establishing an indexed YouTube source",
		Long:  "Verify that document ID, explicit owner ID and URL match one currently indexed YouTube document. The server does not create a source row during this check. Apply rechecks that exact indexed binding and rejects a changed source-registry plan.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			request, err := mediaSourceBootstrapRequestFlags(cmd)
			if err != nil {
				return err
			}
			c, _, err := inspectConfig()
			if err != nil {
				return err
			}
			result, err := newClientForConfig(c).CheckMediaSourceBootstrap(cmd.Context(), request)
			if err != nil {
				return err
			}
			return writeMediaHistoryJSON(cmd, result)
		},
	}
	addMediaSourceBootstrapFlags(bootstrapCheck, false)

	bootstrapApply := &cobra.Command{
		Use:   "bootstrap-apply",
		Short: "Establish the checked YouTube media source incarnation",
		Long:  "Create a fresh source incarnation only after the admin-only read-only bootstrap check. Requires the exact confirmation SHA-256 from that check and --yes. This does not import historical files or infer/modify ownership.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				return errors.New("--yes is required to establish a media source incarnation")
			}
			confirmation, _ := cmd.Flags().GetString("confirmation")
			if !validMediaHistoryConfirmation(confirmation) {
				return errors.New("--confirmation must be the 64-character lowercase SHA-256 from a recent bootstrap check")
			}
			request, err := mediaSourceBootstrapRequestFlags(cmd)
			if err != nil {
				return err
			}
			c, _, err := inspectConfig()
			if err != nil {
				return err
			}
			result, err := newClientForConfig(c).ApplyMediaSourceBootstrap(cmd.Context(), request, confirmation)
			if err != nil {
				return err
			}
			return writeMediaHistoryJSON(cmd, result)
		},
	}
	addMediaSourceBootstrapFlags(bootstrapApply, true)

	parent.AddCommand(reconcile, check, apply, bootstrapCheck, bootstrapApply)
	return parent
}

func addMediaSourceBootstrapFlags(cmd *cobra.Command, apply bool) {
	cmd.Flags().String("document-id", "", "Exact live indexed YouTube document ID")
	cmd.Flags().Uint("owner-id", 0, "Explicit owner ID from the live index (required; use 0 for a global document)")
	cmd.Flags().String("url", "", "Exact YouTube URL stored in the live indexed document")
	_ = cmd.MarkFlagRequired("document-id")
	_ = cmd.MarkFlagRequired("owner-id")
	_ = cmd.MarkFlagRequired("url")
	if apply {
		cmd.Flags().String("confirmation", "", "Exact confirmation SHA-256 returned by bootstrap-check")
		cmd.Flags().Bool("yes", false, "Confirm this checked source-incarnation creation")
		_ = cmd.MarkFlagRequired("confirmation")
	}
}

func mediaSourceBootstrapRequestFlags(cmd *cobra.Command) (client.MediaSourceBootstrapRequest, error) {
	documentID, _ := cmd.Flags().GetString("document-id")
	ownerID, _ := cmd.Flags().GetUint("owner-id")
	url, _ := cmd.Flags().GetString("url")
	if documentID == "" || url == "" || len(documentID) > 4096 || len(url) > 4096 {
		return client.MediaSourceBootstrapRequest{}, errors.New("--document-id and --url must identify an indexed source")
	}
	return client.MediaSourceBootstrapRequest{DocumentID: documentID, OwnerID: ownerID, URL: url}, nil
}

func readMediaHistoryApproval(cmd *cobra.Command) ([]byte, error) {
	path, _ := cmd.Flags().GetString("approval")
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open approval file: %w", err)
	}
	approval, decodeErr := yt.DecodeHistoryApproval(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return json.Marshal(approval)
}

func validMediaHistoryConfirmation(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeMediaHistoryJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
