// SPDX-License-Identifier: AGPL-3.0-or-later

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"

	"github.com/spf13/cobra"
)

func TestMediaReconcileIsDryRunWithoutRuntimeInitialization(t *testing.T) {
	root := t.TempDir()
	state, media := filepath.Join(root, "state"), filepath.Join(root, "media")
	for _, dir := range []string{state, media} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	brokenConfig := filepath.Join(root, "broken.yml")
	if err := os.WriteFile(brokenConfig, []byte("["), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HISTER_CONFIG", brokenConfig)
	t.Setenv("HISTER_DATA_DIR", filepath.Join(root, "must-not-create-data"))
	t.Setenv("HISTER__APP__LOG_FILE", filepath.Join(root, "must-not-create.log"))
	command := &cobra.Command{Use: "hister", PersistentPreRun: func(*cobra.Command, []string) { t.Fatal("reconciliation initialized runtime") }}
	command.AddCommand(newMediaCommand())
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"media", "reconcile", "--state-dir", state, "--media-root", media, "hister-" + strings.Repeat("a", 32) + "-video"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var plan yt.HistoryPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.OwnershipVerified || plan.RegistryChecked || len(plan.Jobs) != 1 || plan.Jobs[0].State != "blocked" || plan.Jobs[0].Code != "reports_unavailable" {
		t.Fatalf("missing reports authorized work: %s", &out)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 3 {
		t.Fatalf("dry run initialized runtime files: %v, %v", entries, err)
	}
	for _, dir := range []string{state, media} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("dry run changed existing directory: %v, %v", entries, err)
		}
	}
	registered, _, err := rootCmd.Find([]string{"media", "reconcile"})
	if err != nil || registered.CommandPath() != "hister media reconcile" {
		t.Fatalf("media command is not registered: %v", err)
	}
}

func TestMediaReconcileRejectsApplyAndUnscopedInputs(t *testing.T) {
	for _, args := range [][]string{
		{"reconcile"},
		{"reconcile", "hister-" + strings.Repeat("a", 32) + "-video"},
		{"reconcile", "--apply"},
		{"reconcile", "--owner-id", "0"},
	} {
		command := newMediaCommand()
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&out)
		command.SetArgs(args)
		if err := command.Execute(); err == nil {
			t.Fatalf("accepted incomplete/mutating command: %v", args)
		}
	}
}
