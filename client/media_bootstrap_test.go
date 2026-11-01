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

func TestMediaSourceBootstrapClientSendsExplicitBindingAndConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"document_id":"11:https://www.youtube.com/watch?v=abcdefghijk","owner_id":11,"url":"https://www.youtube.com/watch?v=abcdefghijk"}` {
			t.Errorf("request body = %q, %v", body, err)
		}
		if r.Header.Get("Origin") != "hister://" || r.Header.Get("X-Access-Token") != "operator-token" {
			t.Errorf("missing CLI auth/CSRF headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/yt_downloader/source/check":
			if r.Header.Get("X-Hister-Media-Source-Confirmation") != "" {
				t.Error("check unexpectedly sent apply confirmation")
			}
			_, _ = io.WriteString(w, `{"document_id":"11:https://www.youtube.com/watch?v=abcdefghijk","owner_id":11,"url":"https://www.youtube.com/watch?v=abcdefghijk","confirmation_sha256":"`+strings.Repeat("b", 64)+`","replayed":false}`)
		case "/api/yt_downloader/source/apply":
			if got := r.Header.Get("X-Hister-Media-Source-Confirmation"); got != strings.Repeat("b", 64) {
				t.Errorf("confirmation header = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"document_id":"11:https://www.youtube.com/watch?v=abcdefghijk","owner_id":11,"source_id":"new-source","replayed":false}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cl := client.New(server.URL, client.WithAccessToken("operator-token"))
	request := client.MediaSourceBootstrapRequest{DocumentID: "11:https://www.youtube.com/watch?v=abcdefghijk", OwnerID: 11, URL: "https://www.youtube.com/watch?v=abcdefghijk"}
	checked, err := cl.CheckMediaSourceBootstrap(context.Background(), request)
	if err != nil || len(checked.ConfirmationSHA256) != 64 || checked.Replayed {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	applied, err := cl.ApplyMediaSourceBootstrap(context.Background(), request, checked.ConfirmationSHA256)
	if err != nil || applied.SourceID != "new-source" || applied.Replayed {
		t.Fatalf("apply = %#v, %v", applied, err)
	}
}
