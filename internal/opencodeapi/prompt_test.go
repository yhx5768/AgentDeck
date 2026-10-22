package opencodeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The reconcile poller must use the flat v1 listing routes. The v2
// /api/question/request route resolves the directory against the serve
// process's own cwd, so a session in another project reads an empty list and a
// missed question would never be recovered.
func TestListPendingQuestionsUsesFlatRoute(t *testing.T) {
	var gotPath, gotDir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDir = r.URL.Query().Get("directory")
		_ = json.NewEncoder(w).Encode([]QuestionRequest{{
			ID:        "que_1",
			SessionID: "ses_1",
			Questions: []QuestionPrompt{{Question: "Which one?", Header: "Pick"}},
		}})
	}))
	defer srv.Close()

	s := &Server{baseURL: srv.URL}
	out, err := s.ListPendingQuestions(context.Background(), "/work/proj")
	if err != nil {
		t.Fatalf("ListPendingQuestions: %v", err)
	}
	if gotPath != "/question" {
		t.Errorf("path = %q, want /question (flat v1 route)", gotPath)
	}
	if gotDir != "/work/proj" {
		t.Errorf("directory = %q, want /work/proj", gotDir)
	}
	if len(out) != 1 || out[0].ID != "que_1" || len(out[0].Questions) != 1 {
		t.Fatalf("decoded = %+v", out)
	}
}

func TestListPendingPermissionsUsesFlatRoute(t *testing.T) {
	var gotPath, gotDir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDir = r.URL.Query().Get("directory")
		_ = json.NewEncoder(w).Encode([]PermissionRequest{{
			ID: "per_1", SessionID: "ses_1", Permission: "bash",
		}})
	}))
	defer srv.Close()

	s := &Server{baseURL: srv.URL}
	out, err := s.ListPendingPermissions(context.Background(), "/work/proj")
	if err != nil {
		t.Fatalf("ListPendingPermissions: %v", err)
	}
	if gotPath != "/permission" {
		t.Errorf("path = %q, want /permission (flat v1 route)", gotPath)
	}
	if gotDir != "/work/proj" {
		t.Errorf("directory = %q, want /work/proj", gotDir)
	}
	if len(out) != 1 || out[0].ID != "per_1" {
		t.Fatalf("decoded = %+v", out)
	}
}
