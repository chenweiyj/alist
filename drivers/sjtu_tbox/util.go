package sjtu_tbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/go-resty/resty/v2"
)

// refresh and cache access_token, libraryId, spaceId
func (d *SJTUTBox) refreshToken(ctx context.Context) error {
	d.tokenMutex.Lock()
	defer d.tokenMutex.Unlock()
	jaCookie := d.normalizedJAAuthCookie()
	configuredUserToken := d.normalizedUserToken()

	// UserToken has the same priority as TboxWebdav: when configured, it is
	// used directly and JAAuthCookie is ignored.
	if configuredUserToken != "" {
		if len(configuredUserToken) != 128 {
			return fmt.Errorf("UserToken must be 128 characters")
		}
		if d.configuredUserToken != configuredUserToken || d.authenticatedCookie != "" {
			d.userToken = configuredUserToken
			d.userID = ""
			d.configuredUserToken = configuredUserToken
			d.authenticatedCookie = ""
			d.userTokenExpires = time.Time{}
			d.accessToken = ""
			d.tokenExpires = time.Time{}
		}
	} else {
		if jaCookie == "" {
			d.clearAuthentication()
			return fmt.Errorf("UserToken or JAAuthCookie is required")
		}
		if d.configuredUserToken != "" {
			d.clearAuthentication()
		}
	}

	// If the cached access token is more than two minutes from expiration, it
	// can be reused as long as the configured authentication source is unchanged.
	if d.accessToken != "" && time.Now().Add(2*time.Minute).Before(d.tokenExpires) &&
		((configuredUserToken != "" && d.configuredUserToken == configuredUserToken) ||
			(configuredUserToken == "" && jaCookie != "" && d.authenticatedCookie == jaCookie)) {
		return nil
	}

	if configuredUserToken == "" && jaCookie != "" && (d.authenticatedCookie != jaCookie ||
		(!d.userTokenExpires.IsZero() && time.Now().Add(2*time.Minute).After(d.userTokenExpires))) {
		token, userID, expiresIn, err := d.loginWithJAAuthCookie(ctx)
		if err != nil {
			return err
		}
		d.userToken = token
		d.userID = ""
		if userID != 0 {
			d.userID = fmt.Sprint(userID)
		}
		d.authenticatedCookie = jaCookie
		d.configuredUserToken = ""
		d.userTokenExpires = expiresIn
		d.accessToken = ""
		d.tokenExpires = time.Time{}
	}
	if d.userToken == "" {
		token, userID, expiresIn, err := d.loginWithJAAuthCookie(ctx)
		if err != nil {
			return err
		}
		d.userToken = token
		if d.userID == "" && userID != 0 {
			d.userID = fmt.Sprint(userID)
		}
		d.authenticatedCookie = jaCookie
		d.userTokenExpires = expiresIn
	}

	client := d.newClient()
	var resp TokenResp

	r, err := client.R().
		SetContext(ctx).
		SetQueryParam("user_token", d.userToken).
		SetBody("{}").
		SetResult(&resp).
		Execute(http.MethodPost, TOKEN_URL)

	if err != nil {
		return err
	}

	if r.IsError() {
		return fmt.Errorf("get space credentials failed: status=%d body=%s", r.StatusCode(), responseBody(r))
	}
	if resp.Status != 0 {
		return fmt.Errorf("get space credentials failed: %s", resp.Message)
	}
	if resp.AccessToken == "" || resp.LibraryId == "" || resp.SpaceId == "" {
		return fmt.Errorf("get space credentials failed: incomplete response")
	}

	d.accessToken = resp.AccessToken
	d.libraryId = resp.LibraryId
	d.spaceId = resp.SpaceId

	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 1800
	}
	d.tokenExpires = time.Now().Add(time.Duration(expiresIn) * time.Second)

	return nil
}

func (d *SJTUTBox) clearAuthentication() {
	d.userToken = ""
	d.userID = ""
	d.configuredUserToken = ""
	d.authenticatedCookie = ""
	d.userTokenExpires = time.Time{}
	d.accessToken = ""
	d.tokenExpires = time.Time{}
}

// loginWithJAAuthCookie follows the same jAccount SSO flow as TboxWebdav.
// The JAAuthCookie is scoped to jaccount.sjtu.edu.cn and is never sent to the
// cloud-drive API host.
func (d *SJTUTBox) loginWithJAAuthCookie(ctx context.Context) (string, int64, time.Time, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("create cookie jar: %w", err)
	}
	jaccountURL, err := url.Parse(JACCOUNT_COOKIE_URL)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("parse jAccount cookie URL: %w", err)
	}
	cookieValue := d.normalizedJAAuthCookie()
	if cookieValue == "" {
		return "", 0, time.Time{}, fmt.Errorf("JAAuthCookie is empty")
	}
	jar.SetCookies(jaccountURL, []*http.Cookie{{Name: "JAAuthCookie", Value: cookieValue, Path: "/jaccount"}})

	var finalURL *url.URL
	client := &http.Client{
		Jar:     jar,
		Timeout: base.DefaultTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			finalURL = req.URL
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SSO_LOGIN_URL, nil)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("create jAccount login request: %w", err)
	}
	req.Header.Set("User-Agent", base.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("jAccount login request: %w", err)
	}
	defer resp.Body.Close()
	if finalURL == nil {
		finalURL = resp.Request.URL
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", 0, time.Time{}, fmt.Errorf("jAccount login failed: status=%d", resp.StatusCode)
	}
	code := finalURL.Query().Get("code")
	if code == "" {
		return "", 0, time.Time{}, fmt.Errorf("jAccount login failed: SSO callback code not found")
	}

	verifyQuery := url.Values{
		"device_id":  {"Chrome 116.0.0.0"},
		"type":       {"sso"},
		"credential": {code},
	}
	verifyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, SSO_VERIFY_URL+"?"+verifyQuery.Encode(), nil)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("create jAccount verification request: %w", err)
	}
	verifyReq.Header.Set("User-Agent", base.UserAgent)
	verifyResp, err := client.Do(verifyReq)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("jAccount verification request: %w", err)
	}
	defer verifyResp.Body.Close()
	body, err := io.ReadAll(verifyResp.Body)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("read jAccount verification response: %w", err)
	}
	if verifyResp.StatusCode < http.StatusOK || verifyResp.StatusCode >= http.StatusMultipleChoices {
		return "", 0, time.Time{}, fmt.Errorf("jAccount verification failed: status=%d body=%s", verifyResp.StatusCode, string(body))
	}
	var loginResp jAccountLoginResp
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("decode jAccount verification response: %w", err)
	}
	if loginResp.Status != 0 {
		return "", 0, time.Time{}, fmt.Errorf("jAccount verification failed: %s", loginResp.Message)
	}
	if loginResp.UserToken == "" {
		return "", 0, time.Time{}, fmt.Errorf("jAccount verification failed: empty userToken")
	}
	expiresAt := time.Time{}
	if loginResp.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(loginResp.ExpiresIn) * time.Second)
	}
	return loginResp.UserToken, loginResp.UserId, expiresAt, nil
}

func (d *SJTUTBox) normalizedJAAuthCookie() string {
	cookieValue := strings.TrimSpace(d.JAAuthCookie)
	if strings.HasPrefix(cookieValue, "JAAuthCookie=") {
		cookieValue = strings.TrimPrefix(cookieValue, "JAAuthCookie=")
	}
	return cookieValue
}

func (d *SJTUTBox) normalizedUserToken() string {
	token := strings.TrimSpace(d.UserToken)
	if strings.HasPrefix(token, "USER_TOKEN=") {
		token = strings.TrimPrefix(token, "USER_TOKEN=")
	}
	return token
}

// rootPath returns the remote path represented by the Alist storage root.
// RootPath is optional for backwards compatibility with configurations created
// before the root-folder field existed, so an empty value means the drive root.
func (d *SJTUTBox) rootPath() string {
	root := strings.TrimSpace(strings.ReplaceAll(d.GetRootPath(), "\\", "/"))
	root = strings.Trim(root, "/")
	if root == "" {
		return "/"
	}
	return path.Clean("/" + root)
}

func responseBody(resp *resty.Response) string {
	body := strings.TrimSpace(resp.String())
	if len(body) > 512 {
		return body[:512]
	}
	return body
}

// create a resty client with preloaded USER_TOKEN and keep_alive
func (d *SJTUTBox) newClient() *resty.Client {
	client := base.NewRestyClient()
	client.SetCookie(&http.Cookie{Name: "USER_TOKEN", Value: d.userToken, Path: "/"})
	return client
}

// standardize and encode the internal path of alist
func (d *SJTUTBox) encodePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "/" || p == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, part := range parts {
		parts[i] = escapePathPart(part)
	}
	return strings.Join(parts, "/")
}

// escapePathPart matches Uri.EscapeDataString used by TboxWebdav: only
// RFC3986 unreserved bytes remain literal, so names containing '+' or '&' do
// not get interpreted as URL syntax by the cloud-drive API.
func escapePathPart(part string) string {
	const hex = "0123456789ABCDEF"
	bytes := []byte(part)
	var b strings.Builder
	b.Grow(len(bytes))
	for _, c := range bytes {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// concatenate basic paths and file names, handling root directory boundary situation
func buildPath(base, name string) string {
	if base == "/" || base == "" {
		return "/" + name
	}
	return path.Join(base, name)
}

// move a single file or folder
func (d *SJTUTBox) moveItem(ctx context.Context, srcPath, dstDirPath, name string, isDir bool, strategy string) error {
	// The Tencent SMH API uses the file endpoint for both file and directory
	// copy/move operations; the item type is inferred from the source path.
	moveItemURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(path.Join(dstDirPath, name)))

	req := d.newClient().R().
		SetContext(ctx).
		SetQueryParam("access_token", d.accessToken).
		SetQueryParam("conflict_resolution_strategy", strategy).
		SetBody(map[string]interface{}{"from": srcPath})

	if isDir {
		req.SetQueryParam("move_authority", "true")
	}

	resp, err := req.Execute(http.MethodPut, moveItemURL)
	if err != nil {
		return err
	}

	// 409 means conflict name
	if resp.StatusCode() == http.StatusConflict {
		return errs.ObjectNotFound
	}
	if resp.IsError() {
		return fmt.Errorf("move failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}
	return nil
}
