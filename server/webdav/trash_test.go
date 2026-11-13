package webdav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
)

func TestClassifyTrashPath(t *testing.T) {
	for _, test := range []struct {
		path string
		want trashPathKind
	}{
		{"/.Trashes", trashDirectory},
		{"/.Trashes/", trashDirectory},
		{"/.Trashes/0", trashDirectory},
		{"/.Trashes/501/", trashDirectory},
		{"/.Trashes/502/file.txt", trashItem},
		{"/.Trashes/1001/folder/child.txt", trashItem},
		{"/.Trashes/4294967295/folder/", trashItem},
		{"/.Trashes/501/name with spaces.txt", trashItem},
		{"/.Trashes/501/link.rclonelink", trashItem},
		{"", trashOutside},
		{".Trashes/501/file", trashOutside},
		{"/.Trash/501/file", trashOutside},
		{"/.Trashes-backup/501/file", trashOutside},
		{"/folder/.Trashes/501/file", trashOutside},
		{"/.Trashes/user/file", trashOutside},
		{"/.Trashes/-1/file", trashOutside},
		{"/.Trashes/+501/file", trashOutside},
		{"/.Trashes/4294967296/file", trashOutside},
		{"/.Trashes//501/file", trashOutside},
		{"/.Trashes/501//file", trashOutside},
		{"/.Trashes/501/./file", trashOutside},
		{"/.Trashes/501/../file", trashOutside},
		{"/folder/../.Trashes/501/file", trashOutside},
		{"/.Trashes/501/file\\child", trashOutside},
		{"/.Trashes/501/file\x00", trashOutside},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := classifyTrashPath(test.path); got != test.want {
				t.Fatalf("classifyTrashPath(%q)=%v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestTrashDeleteRequiresExactOptIn(t *testing.T) {
	for _, value := range []string{"", "0", "true", "1,1", "2"} {
		request := httptest.NewRequest("MOVE", "/dav/source", nil)
		request.Header.Set(trashDeleteHeader, value)
		if trashDeleteEnabled(request) {
			t.Fatalf("unexpected opt-in for %q", value)
		}
	}
	request := httptest.NewRequest("MOVE", "/dav/source", nil)
	request.Header.Set(trashDeleteHeader, "1")
	if !trashDeleteEnabled(request) {
		t.Fatal("explicit opt-in not recognized")
	}
	request.Header.Add(trashDeleteHeader, "1")
	if trashDeleteEnabled(request) {
		t.Fatal("ambiguous duplicate header accepted")
	}
}

func TestTrashPathsRespectUserBase(t *testing.T) {
	user := &model.User{BasePath: "/scope"}
	for _, name := range []string{"/.Trashes/502/file", "/scope/.Trashes/502/file"} {
		if classifyUserTrashPath(user, name) != trashItem {
			t.Fatalf("user Trash path not recognized: %s", name)
		}
	}
	for _, name := range []string{"/other/.Trashes/502/file", "/scope/../.Trashes/502/file"} {
		if classifyUserTrashPath(user, name) != trashOutside {
			t.Fatalf("out-of-scope Trash path accepted: %s", name)
		}
	}
}

func TestTrashDeleteRequiresRemoveAndWebDAVManage(t *testing.T) {
	remove := int32(1 << common.PermRemove)
	manage := int32(1 << common.PermWebdavManage)
	move := int32(1 << common.PermMove)
	for _, permission := range []int32{0, remove, manage, move, move | manage} {
		if canTrashDelete(permission) {
			t.Fatalf("delete allowed with insufficient permissions %d", permission)
		}
	}
	if !canTrashDelete(remove | manage) {
		t.Fatal("delete denied with required permissions")
	}
}

func TestTrashReceiptsExpireAndIsolateUsers(t *testing.T) {
	var receipts trashDeleteReceipts
	key := trashReceiptKey{7, "/.Trashes/502/file.txt"}
	info := &model.Object{Name: "source.txt", Size: 42, Modified: time.Now()}
	receipts.put(key, "/cloud/source.txt", info)
	receipt, ok := receipts.get(key)
	if !ok || receipt.object.Name != "file.txt" || receipt.object.Size != 42 || receipt.source != "/cloud/source.txt" {
		t.Fatalf("invalid receipt: %+v", receipt)
	}
	if _, ok := receipts.get(trashReceiptKey{8, key.destination}); ok {
		t.Fatal("receipt leaked to another DAV user")
	}
	receipt.expires = time.Now().Add(-time.Second)
	receipts.entries[key] = receipt
	if _, ok := receipts.get(key); ok {
		t.Fatal("expired receipt returned")
	}
}

func TestTrashReceiptCacheIsBoundedAndRemovalIsScoped(t *testing.T) {
	var receipts trashDeleteReceipts
	info := &model.Object{Name: "file", Modified: time.Now()}
	for index := 0; index < trashReceiptLimit+10; index++ {
		receipts.put(trashReceiptKey{7, "/.Trashes/502/" + time.Unix(int64(index), 0).String()}, "/source", info)
	}
	if len(receipts.entries) != trashReceiptLimit {
		t.Fatalf("receipt cache size=%d", len(receipts.entries))
	}
	other := trashReceiptKey{8, "/.Trashes/502/other"}
	receipts.put(other, "/source", info)
	receipts.remove(7, "/.Trashes/502")
	if _, ok := receipts.get(other); !ok {
		t.Fatal("receipt removal affected another user")
	}
	if len(receipts.entries) != 1 {
		t.Fatalf("remaining receipts=%d, want 1", len(receipts.entries))
	}
}

func trashRequest(method, name string, userID uint) *http.Request {
	request := httptest.NewRequest(method, "/dav"+name, nil)
	request.Header.Set(trashDeleteHeader, "1")
	request.Header.Set("Depth", "0")
	user := &model.User{ID: userID, BasePath: "/"}
	return request.WithContext(context.WithValue(request.Context(), "user", user))
}

func TestTrashNamespaceAcknowledgesMetadataWithoutListingDeletedItems(t *testing.T) {
	handler := &Handler{Prefix: "/dav", LockSystem: NewMemLS()}
	handler.trashDeletes.put(trashReceiptKey{7, "/.Trashes/502/file.txt"}, "/cloud/source.txt",
		&model.Object{Name: "source.txt", Size: 42, Modified: time.Now()})
	for _, test := range []struct {
		path   string
		userID uint
		depth  string
		status int
	}{
		{"/.Trashes/502/file.txt", 7, "0", http.StatusMultiStatus},
		{"/.Trashes/502/file.txt", 8, "0", http.StatusNotFound},
		{"/.Trashes/502/file.txt", 7, "1", http.StatusNotFound},
		{"/.Trashes/502/missing.txt", 7, "0", http.StatusNotFound},
		{"/.Trashes/502/", 7, "1", http.StatusMultiStatus},
		{"/.Trashes/1001/", 7, "1", http.StatusMultiStatus},
	} {
		request := trashRequest("PROPFIND", test.path, test.userID)
		request.Header.Set("Depth", test.depth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s user %d depth %s status=%d, want %d", test.path, test.userID, test.depth, response.Code, test.status)
		}
		if test.path == "/.Trashes/502/" && strings.Contains(response.Body.String(), "file.txt") {
			t.Fatal("deleted item appeared in Trash listing")
		}
	}
}

func TestTrashNamespaceDoesNotExposeFileContents(t *testing.T) {
	handler := &Handler{Prefix: "/dav", LockSystem: NewMemLS()}
	for _, method := range []string{"GET", "HEAD", "COPY", "MOVE"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, trashRequest(method, "/.Trashes/502/file.txt", 7))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", method, response.Code)
		}
	}
}

func TestTrashNamespaceOptInAndDirectoryCreation(t *testing.T) {
	handler := &Handler{Prefix: "/dav", LockSystem: NewMemLS()}
	for _, name := range []string{"/.Trashes/", "/.Trashes/0/", "/.Trashes/502/", "/.Trashes/1001/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, trashRequest("MKCOL", name, 7))
		if response.Code != http.StatusCreated {
			t.Fatalf("MKCOL %s returned %d", name, response.Code)
		}
	}
	response := httptest.NewRecorder()
	request := trashRequest("PROPFIND", "/.Trashes/", 7)
	request.Header.Set("Depth", "1")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), "/.Trashes/502/") ||
		!strings.Contains(response.Body.String(), "/.Trashes/1001/") {
		t.Fatalf("virtual UID directories missing from root: %s", response.Body.String())
	}
	request = trashRequest("PROPFIND", "/.Trashes/502/", 7)
	request.Header.Del(trashDeleteHeader)
	if handled, _, _ := handler.handleTrashNamespace(httptest.NewRecorder(), request); handled {
		t.Fatal("Trash extension enabled without explicit header")
	}
	if handled, _, _ := handler.handleTrashNamespace(httptest.NewRecorder(), trashRequest("PROPFIND", "/normal/path", 7)); handled {
		t.Fatal("normal path intercepted by Trash extension")
	}
}

func TestTrashDeleteWithoutRemovePermissionNeverTouchesStorage(t *testing.T) {
	handler := &Handler{Prefix: "/dav", LockSystem: NewMemLS()}
	request := trashRequest("MOVE", "/cloud/source.txt", 7)
	request.Header.Set("Depth", "infinity")
	request.Header.Set("Destination", "http://example.com/dav/.Trashes/502/source.txt")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("delete without permissions returned %d, want 403", response.Code)
	}
}
