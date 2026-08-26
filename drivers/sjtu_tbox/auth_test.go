package sjtu_tbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

func ensureTestConfig() {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
}

func TestLoginWithJAAuthCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jaccount/login":
			cookie, err := r.Cookie("JAAuthCookie")
			if err != nil || cookie == nil || cookie.Value != "test-cookie" {
				var value string
				if cookie != nil {
					value = cookie.Value
				}
				t.Errorf("JAAuthCookie = %q, err=%v", value, err)
				return
			}
			http.Redirect(w, r, "/callback?code=callback-code&state=test", http.StatusFound)
		case "/callback":
			fmt.Fprint(w, "authenticated")
		case "/verify":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if got := r.URL.Query().Get("credential"); got != "callback-code" {
				t.Errorf("credential = %q", got)
			}
			if got := r.URL.Query().Get("type"); got != "sso" {
				t.Errorf("type = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":0,"userId":42,"userToken":"user-token","expiresIn":3600}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldLoginURL, oldVerifyURL, oldCookieURL := SSO_LOGIN_URL, SSO_VERIFY_URL, JACCOUNT_COOKIE_URL
	SSO_LOGIN_URL = server.URL + "/jaccount/login"
	SSO_VERIFY_URL = server.URL + "/verify"
	JACCOUNT_COOKIE_URL = server.URL + "/jaccount/"
	t.Cleanup(func() {
		SSO_LOGIN_URL, SSO_VERIFY_URL, JACCOUNT_COOKIE_URL = oldLoginURL, oldVerifyURL, oldCookieURL
	})

	d := &SJTUTBox{Addition: Addition{JAAuthCookie: "JAAuthCookie=test-cookie\n"}}
	token, userID, expiresAt, err := d.loginWithJAAuthCookie(t.Context())
	if err != nil {
		t.Fatalf("loginWithJAAuthCookie: %v", err)
	}
	if token != "user-token" || userID != 42 {
		t.Fatalf("login result = token %q, userID %d", token, userID)
	}
	if expiresAt.IsZero() {
		t.Fatal("login expiry is empty")
	}
}

func TestRefreshTokenPrefersConfiguredUserToken(t *testing.T) {
	ensureTestConfig()
	userToken := strings.Repeat("a", 128)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("user_token"); got != userToken {
			t.Errorf("user_token length = %d, want %d", len(got), len(userToken))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"libraryId":"library","spaceId":"space","accessToken":"access","expiresIn":1800}`)
	}))
	defer server.Close()

	oldTokenURL, oldLoginURL := TOKEN_URL, SSO_LOGIN_URL
	TOKEN_URL = server.URL
	SSO_LOGIN_URL = server.URL + "/cookie-login-must-not-run"
	t.Cleanup(func() {
		TOKEN_URL, SSO_LOGIN_URL = oldTokenURL, oldLoginURL
	})

	d := &SJTUTBox{Addition: Addition{
		UserToken:    "USER_TOKEN=" + userToken + "\n",
		JAAuthCookie: "invalid-cookie",
	}}
	if err := d.refreshToken(t.Context()); err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if d.userToken != userToken || d.accessToken != "access" {
		t.Fatalf("unexpected token state: userToken length=%d accessTokenSet=%v", len(d.userToken), d.accessToken != "")
	}
}

func TestRefreshTokenRequiresOneCredential(t *testing.T) {
	d := &SJTUTBox{}
	if err := d.refreshToken(t.Context()); err == nil || !strings.Contains(err.Error(), "UserToken or JAAuthCookie") {
		t.Fatalf("error = %v, want missing credential error", err)
	}
}

func TestDriverRegistrationAndCredentialFields(t *testing.T) {
	constructor, err := op.GetDriver("SJTUTBox")
	if err != nil {
		t.Fatalf("SJTUTBox is not registered: %v", err)
	}
	instance := constructor()
	if _, ok := instance.(*SJTUTBox); !ok {
		t.Fatalf("SJTUTBox constructor returned %T", instance)
	}

	info := op.GetDriverInfoMap()["SJTUTBox"]
	fields := make(map[string]bool, len(info.Additional))
	items := make(map[string]driver.Item, len(info.Additional))
	for _, item := range info.Additional {
		fields[item.Name] = true
		items[item.Name] = item
	}
	for _, name := range []string{"root_folder_path", "user_token", "ja_auth_cookie"} {
		if !fields[name] {
			t.Errorf("storage configuration is missing %q", name)
		}
	}
	if root := items["root_folder_path"]; root.Type != "string" || root.Default != "/" {
		t.Errorf("root_folder_path metadata = type %q, default %q; want string and /", root.Type, root.Default)
	}
	if !strings.Contains(items["user_token"].Help, "https://pan.sjtu.edu.cn/") || !strings.Contains(items["user_token"].Help, "UserToken") {
		t.Errorf("user_token help is missing login instructions: %q", items["user_token"].Help)
	}
	if !strings.Contains(items["ja_auth_cookie"].Help, "https://jaccount.sjtu.edu.cn/jaccount/") || !strings.Contains(items["ja_auth_cookie"].Help, "JAAuthCookie") {
		t.Errorf("ja_auth_cookie help is missing login instructions: %q", items["ja_auth_cookie"].Help)
	}
	if _, err := op.GetDriver("Jbox"); err == nil {
		t.Error("legacy Jbox driver name is still registered")
	}
}

func TestGetRootUsesConfiguredRootPath(t *testing.T) {
	d := &SJTUTBox{Addition: Addition{RootPath: driver.RootPath{RootFolderPath: "共享资料\\项目/"}}}
	root, err := d.GetRoot(t.Context())
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	if got, want := root.GetPath(), "/共享资料/项目"; got != want {
		t.Fatalf("root path = %q, want %q", got, want)
	}

	d = &SJTUTBox{}
	root, err = d.GetRoot(t.Context())
	if err != nil {
		t.Fatalf("GetRoot default: %v", err)
	}
	if got := root.GetPath(); got != "/" {
		t.Fatalf("default root path = %q, want %q", got, "/")
	}
}

func TestEncodePathEscapesEachPathPart(t *testing.T) {
	d := &SJTUTBox{}
	if got, want := d.encodePath(`/folder/a b+#/报告.txt`), "folder/a%20b%2B%23/%E6%8A%A5%E5%91%8A.txt"; got != want {
		t.Fatalf("encodePath = %q, want %q", got, want)
	}
	if got := d.encodePath("/"); got != "" {
		t.Fatalf("encodePath(root) = %q, want empty path", got)
	}
	if _, err := url.Parse("https://pan.sjtu.edu.cn/" + d.encodePath("/a b")); err != nil {
		t.Fatalf("encoded path is not a valid URL path: %v", err)
	}
}

func TestIntegrationCredentials(t *testing.T) {
	ensureTestConfig()
	userToken := os.Getenv("SJTU_TBOX_USER_TOKEN")
	jaAuthCookie := os.Getenv("SJTU_TBOX_JA_AUTH_COOKIE")
	if cookieFile := os.Getenv("SJTU_TBOX_JA_AUTH_COOKIE_FILE"); jaAuthCookie == "" && cookieFile != "" {
		contents, err := os.ReadFile(cookieFile)
		if err != nil {
			t.Fatalf("read JAAuthCookie file: %v", err)
		}
		jaAuthCookie = string(contents)
	}
	if userToken == "" && jaAuthCookie == "" {
		t.Skip("set SJTU_TBOX_USER_TOKEN or SJTU_TBOX_JA_AUTH_COOKIE[_FILE] to run")
	}

	d := &SJTUTBox{Addition: Addition{
		RootPath:     driver.RootPath{RootFolderPath: os.Getenv("SJTU_TBOX_ROOT_PATH")},
		OrderBy:      "name",
		OrderByType:  "asc",
		UserToken:    userToken,
		JAAuthCookie: jaAuthCookie,
	}}
	if err := d.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	root, err := d.GetRoot(t.Context())
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	objs, err := d.List(t.Context(), root, model.ListArgs{})
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	t.Logf("authenticated and listed root %q: %d items", root.GetPath(), len(objs))
}
