// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/client"
)

func TestMediaRetentionClientSendsCutoffAndConfirmation(t *testing.T) {
	before := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"before":"2025-01-01T00:00:00Z"}` {
			t.Errorf("request body = %q, %v", body, err)
		}
		if r.Header.Get("Origin") != "hister://" || r.Header.Get("X-Access-Token") != "operator-token" {
			t.Errorf("missing CLI auth/CSRF headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/yt_downloader/retention/check":
			if r.Header.Get("X-Hister-Media-Retention-Confirmation") != "" {
				t.Error("check unexpectedly sent apply confirmation")
			}
			_, _ = io.WriteString(w, `{"before":"2025-01-01T00:00:00Z","artifacts":[],"selected_artifacts_excluded":2,"retire_artifacts":0,"files_to_remove":0,"missing_files":0,"bytes_to_remove":0,"blocked_files":0,"confirmation_sha256":"`+strings.Repeat("b", 64)+`"}`)
		case "/api/yt_downloader/retention/apply":
			if got := r.Header.Get("X-Hister-Media-Retention-Confirmation"); got != strings.Repeat("b", 64) {
				t.Errorf("confirmation header = %q", got)
			}
			_, _ = io.WriteString(w, `{"before":"2025-01-01T00:00:00Z","retired_artifacts":0,"removed_files":0,"missing_files":0,"pending_files":[]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cl := client.New(server.URL, client.WithAccessToken("operator-token"))
	checked, err := cl.CheckMediaRetention(context.Background(), before)
	if err != nil || len(checked.ConfirmationSHA256) != 64 || checked.SelectedArtifactsExcluded != 2 {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	applied, err := cl.ApplyMediaRetention(context.Background(), before, checked.ConfirmationSHA256)
	if err != nil || applied.RetiredArtifacts != 0 {
		t.Fatalf("apply = %#v, %v", applied, err)
	}
}
