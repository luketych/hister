// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asciimoo/hister/server/document"
)

func historyApprovalFixture(t *testing.T) (string, string, *HistoryApproval) {
	t.Helper()
	state, root := historyRoots(t)
	video := "hister-" + strings.Repeat("a", 32) + "-video"
	audio := "hister-" + strings.Repeat("b", 32) + "-audio"
	historyFixture(t, state, root, video)
	historyFixture(t, state, root, audio)
	plan, err := PlanHistory(context.Background(), state, root, []string{video, audio})
	if err != nil {
		t.Fatal(err)
	}
	owner := uint(0)
	approval := &HistoryApproval{
		SchemaVersion: 1, ApprovalID: "reviewed-pair-1", Action: "retain_history",
		Source: HistoryApprovalSource{SourceID: "existing-incarnation", DocumentID: historyTestURL, OwnerID: &owner, URL: historyTestURL},
	}
	for _, job := range plan.Jobs {
		data, err := os.ReadFile(filepath.Join(root, job.File.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		expected := HistoryApprovalJob{JobID: job.JobID, EvidenceSHA256: job.EvidenceSHA256, MediaSHA256: hex.EncodeToString(hash[:])}
		if job.Role == "video" {
			approval.Video = expected
		} else {
			approval.Audio = expected
		}
	}
	return state, root, approval
}

func approvalJSON(t *testing.T, approval *HistoryApproval) []byte {
	t.Helper()
	data, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHistoryApprovalStrictExplicitContract(t *testing.T) {
	_, _, approval := historyApprovalFixture(t)
	data := approvalJSON(t, approval)
	decoded, err := DecodeHistoryApproval(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(approval, decoded) {
		t.Fatalf("valid approval = %#v, %v", decoded, err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"null", []byte(`null`)},
		{"array", []byte(`[]`)},
		{"inventory", []byte(`{"schema_version":1,"dry_run":true}`)},
		{"unknown actor", bytes.Replace(data, []byte(`"schema_version":1`), []byte(`"approved_by":123,"schema_version":1`), 1)},
		{"unknown executable", bytes.Replace(data, []byte(`"action":"retain_history"`), []byte(`"binary":"secret-command","action":"retain_history"`), 1)},
		{"missing owner", bytes.Replace(data, []byte(`"owner_id":0,`), nil, 1)},
		{"null owner", bytes.Replace(data, []byte(`"owner_id":0`), []byte(`"owner_id":null`), 1)},
		{"duplicate owner", bytes.Replace(data, []byte(`"owner_id":0`), []byte(`"owner_id":0,"owner_id":22`), 1)},
		{"negative owner", bytes.Replace(data, []byte(`"owner_id":0`), []byte(`"owner_id":-1`), 1)},
		{"case-variant duplicate owner", bytes.Replace(data, []byte(`"owner_id":0`), []byte(`"owner_id":0,"Owner_ID":22`), 1)},
		{"case-variant owner", bytes.Replace(data, []byte(`"owner_id":0`), []byte(`"Owner_ID":0`), 1)},
		{"case-variant source", bytes.Replace(data, []byte(`"source":`), []byte(`"Source":`), 1)},
		{"case-variant duplicate source", bytes.Replace(data, []byte(`"source":`), []byte(`"Source":{},"source":`), 1)},
		{"extra JSON", append(append([]byte{}, data...), []byte(`{}`)...)},
		{"oversize", []byte(strings.Repeat(" ", maxHistoryApprovalBytes+1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeHistoryApproval(bytes.NewReader(test.data))
			if !errors.Is(err, ErrHistoryApproval) || err.Error() != ErrHistoryApproval.Error() {
				t.Fatalf("bad contract accepted or private input echoed: %v", err)
			}
		})
	}
	for _, mutate := range []func(*HistoryApproval){
		func(a *HistoryApproval) { a.SchemaVersion = 2 },
		func(a *HistoryApproval) { a.Action = "select_current" },
		func(a *HistoryApproval) { a.ApprovalID = "../approval" },
		func(a *HistoryApproval) { a.Source.SourceID = "" },
		func(a *HistoryApproval) { a.Source.DocumentID = "another-document" },
		func(a *HistoryApproval) { a.Source.OwnerID = new(uint(22)) },
		func(a *HistoryApproval) { a.Video = a.Audio },
		func(a *HistoryApproval) { a.Video.EvidenceSHA256 = "not-a-digest" },
		func(a *HistoryApproval) { a.Audio.MediaSHA256 = strings.Repeat("A", 64) },
	} {
		fresh, err := DecodeHistoryApproval(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		mutate(fresh)
		if err := fresh.Validate(); !errors.Is(err, ErrHistoryApproval) {
			t.Fatalf("invalid typed approval bypassed validation: %#v, %v", fresh, err)
		}
	}
	// A nonzero owner requires the derived owner-scoped document identity.
	approval.Source.OwnerID = new(uint(22))
	approval.Source.DocumentID = document.GetDocID(22, approval.Source.URL)
	if _, err := DecodeHistoryApproval(bytes.NewReader(approvalJSON(t, approval))); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryApprovalRevalidatesEvidenceAndActualMediaBytes(t *testing.T) {
	for _, mutation := range []string{"same_size_bytes", "report_change", "wrong_digest", "missing", "symlink", "wrong_source"} {
		t.Run(mutation, func(t *testing.T) {
			state, root, approval := historyApprovalFixture(t)
			path := filepath.Join(root, approval.Video.JobID, "nested", "clip.webm")
			switch mutation {
			case "same_size_bytes":
				if err := os.WriteFile(path, []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "report_change":
				report := filepath.Join(state, "artifacts", approval.Video.JobID, "jobs", approval.Video.JobID, "result.json")
				file, err := os.OpenFile(report, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("\n"); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "wrong_digest":
				approval.Video.MediaSHA256 = strings.Repeat("0", 64)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, approval.Audio.JobID, "nested", "clip.mp3"), path); err != nil {
					t.Fatal(err)
				}
			case "wrong_source":
				approval.Source.URL = "https://www.youtube.com/watch?v=lmnopqrstuv"
				approval.Source.DocumentID = approval.Source.URL
			}
			prepared, err := PrepareHistoryApproval(context.Background(), state, root, approval)
			if prepared != nil {
				_ = prepared.Close()
				t.Fatal("stale evidence was prepared")
			}
			if !errors.Is(err, ErrHistoryEvidence) {
				t.Fatalf("evidence mismatch = %v", err)
			}
		})
	}
}

func TestHistoryApprovalPinsFilesAndRejectsChangesBeforeCommit(t *testing.T) {
	for _, mutation := range []string{"none", "replacement", "in_place", "closed"} {
		t.Run(mutation, func(t *testing.T) {
			state, root, approval := historyApprovalFixture(t)
			before := historySnapshot(t, filepath.Dir(state))
			prepared, err := PrepareHistoryApproval(context.Background(), state, root, approval)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = prepared.Close() }()
			input := prepared.Input(91)
			if input.ApprovedBy != 91 || input.UserID != 0 || input.SourceID != approval.Source.SourceID || len(input.Pair) != 2 {
				t.Fatalf("prepared input = %#v", input)
			}
			*approval.Source.OwnerID = 99
			approval.Source.URL = "changed"
			input.Pair[0].RelativePath = "changed"
			if prepared.Input(92).Pair[0].RelativePath == "changed" || prepared.Input(92).UserID != 0 || prepared.SourceURL() != historyTestURL {
				t.Fatal("caller mutation changed prepared authority")
			}
			path := filepath.Join(root, approval.Video.JobID, "nested", "clip.webm")
			switch mutation {
			case "replacement":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "in_place":
				if err := os.WriteFile(path, []byte("changed bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := prepared.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err = prepared.Recheck(context.Background())
			if mutation == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if after := historySnapshot(t, filepath.Dir(state)); !reflect.DeepEqual(before, after) {
					t.Fatal("preparing approval changed evidence/files")
				}
			} else if !errors.Is(err, ErrHistoryEvidence) {
				t.Fatalf("changed/closed evidence passed final check: %v", err)
			}
		})
	}
	state, root, approval := historyApprovalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareHistoryApproval(ctx, state, root, approval); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preparation = %v", err)
	}
}
