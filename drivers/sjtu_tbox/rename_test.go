package sjtu_tbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
)

func cachedTestDriver() *SJTUTBox {
	token := strings.Repeat("a", 128)
	return &SJTUTBox{
		Addition:            Addition{UserToken: token},
		userToken:           token,
		configuredUserToken: token,
		accessToken:         "access",
		libraryId:           "library",
		spaceId:             "space",
		tokenExpires:        time.Now().Add(time.Hour),
	}
}

func TestRenameCloudAPI(t *testing.T) {
	ensureTestConfig()
	for _, tc := range []struct {
		name, source, target, endpoint string
		directory                      bool
		status                         int
	}{
		{"root directory", "/untitled folder", "Applications", "/directory/library/space/Applications", true, http.StatusNoContent},
		{"mounted directory", "/data/untitled folder", "Applications", "/directory/library/space/data/Applications", true, http.StatusNoContent},
		{"directory JSON response", "/data/untitled folder", "Applications", "/directory/library/space/data/Applications", true, http.StatusOK},
		{"escaped directory", "/data/untitled folder", "资料 +#%", "/directory/library/space/data/%E8%B5%84%E6%96%99%20%2B%23%25", true, http.StatusNoContent},
		{"file", "/data/old file.txt", "new file.txt", "/file/library/space/data/new%20file.txt", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPut || r.URL.EscapedPath() != tc.endpoint {
					t.Errorf("request = %s %s, want PUT %s", r.Method, r.URL.EscapedPath(), tc.endpoint)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var body struct {
					From string `json:"from"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if want := strings.TrimPrefix(tc.source, "/"); body.From != want {
					t.Errorf("from = %q, want %q", body.From, want)
				}
				if got := r.URL.Query().Get("access_token"); got != "access" {
					t.Errorf("access token = %q", got)
				}
				strategy := "overwrite"
				if tc.directory {
					strategy = "ask"
					if r.URL.Query().Get("move_authority") != "true" {
						t.Error("directory rename must preserve permissions")
					}
				}
				if r.URL.Query().Get("conflict_resolution_strategy") != strategy {
					t.Errorf("unexpected conflict strategy")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					fmt.Fprint(w, `{}`)
				}
			}))
			defer server.Close()
			previous := API_URL
			API_URL = server.URL
			t.Cleanup(func() { API_URL = previous })
			source := &model.Object{Name: path.Base(tc.source), Path: tc.source, IsFolder: tc.directory}
			obj, err := cachedTestDriver().Rename(t.Context(), source, tc.target)
			if err != nil {
				t.Fatalf("Rename: %v", err)
			}
			if obj.GetName() != tc.target || obj.IsDir() != tc.directory ||
				obj.GetPath() != path.Join(path.Dir(tc.source), tc.target) {
				t.Errorf("unexpected renamed object: %+v", obj)
			}
			if requests != 1 {
				t.Errorf("request count = %d, want 1", requests)
			}
		})
	}
}

func TestMoveItemDirectoryConflict(t *testing.T) {
	ensureTestConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/directory/library/space/data/target/child" {
			t.Errorf("unexpected directory move: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			From string `json:"from"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.From != "data/source/child" {
			t.Errorf("from = %q", body.From)
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	previous := API_URL
	API_URL = server.URL
	t.Cleanup(func() { API_URL = previous })
	err := cachedTestDriver().moveItem(t.Context(), "/data/source/child", "/data/target", "child", true, "ask")
	if !errors.Is(err, errs.ObjectNotFound) {
		t.Fatalf("directory conflict = %v; want merge signal", err)
	}
}
