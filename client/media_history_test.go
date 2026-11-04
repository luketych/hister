// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asciimoo/hister/client"
)

func TestMediaHistoryClientSendsApprovalAndConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"approval":true}` {
			t.Errorf("request body = %q, %v", body, err)
		}
		if r.Header.Get("Origin") != "hister://" || r.Header.Get("X-Access-Token") != "operator-token" {
			t.Errorf("missing CLI auth/CSRF headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/yt_downloader/history/check":
			if r.Header.Get("X-Hister-Media-Confirmation") != "" {
				t.Error("check unexpectedly sent an apply confirmation")
			}
			_, _ = io.WriteString(w, `{"approval_id":"reviewed","confirmation_sha256":"`+strings.Repeat("a", 64)+`","replayed":false}`)
		case "/api/yt_downloader/history/apply":
			if got := r.Header.Get("X-Hister-Media-Confirmation"); got != strings.Repeat("a", 64) {
				t.Errorf("confirmation header = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"approval_id":"reviewed","receipt_id":"reviewed","set_id":"set-1","replayed":false}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cl := client.New(server.URL, client.WithAccessToken("operator-token"))
	approval := []byte(`{"approval":true}`)
	checked, err := cl.CheckMediaHistory(context.Background(), approval)
	if err != nil || checked.ApprovalID != "reviewed" || len(checked.ConfirmationSHA256) != 64 || checked.Replayed {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	applied, err := cl.ApplyMediaHistory(context.Background(), approval, checked.ConfirmationSHA256)
	if err != nil || applied.ReceiptID != "reviewed" || applied.SetID != "set-1" || applied.Replayed {
		t.Fatalf("apply = %#v, %v", applied, err)
	}
}
