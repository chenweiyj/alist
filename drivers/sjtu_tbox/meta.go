package sjtu_tbox

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/op"
)

type Addition struct {
	driver.RootPath
	OrderBy      string `json:"order_by" type:"select" required:"true" options:"name,modificationTime,size" default:"name"`
	OrderByType  string `json:"order_by_type" type:"select" required:"true" options:"asc,desc" default:"asc"`
	UserToken    string `json:"user_token" type:"text" help:"登录新云盘 https://pan.sjtu.edu.cn/，按下 F12 打开开发者工具，在“应用程序 -> 存储 -> Cookie”中找到 UserToken。填写 UserToken 后优先使用它进行认证。"`
	JAAuthCookie string `json:"ja_auth_cookie" type:"text" help:"请先登录需要 jAccount 认证的网站（例如 http://my.sjtu.edu.cn/），然后打开 https://jaccount.sjtu.edu.cn/jaccount/，按下 F12 打开开发者工具，在“应用程序 -> 存储 -> Cookie”中找到 JAAuthCookie。"`
}

var config = driver.Config{
	Name:              "SJTUTBox",
	LocalSort:         false,
	OnlyLocal:         false,
	OnlyProxy:         false,
	NoCache:           false,
	NoUpload:          false,
	NeedMs:            false,
	DefaultRoot:       "/",
	CheckStatus:       false,
	Alert:             "",
	NoOverwriteUpload: false,
}

var API_URL = "https://pan.sjtu.edu.cn/api/v1"

// used by refreshToken
var TOKEN_URL = "https://pan.sjtu.edu.cn/user/v1/space/1/personal"

// Used by the JAAuthCookie SSO exchange. Variables (rather than constants)
// also make the protocol testable without contacting the real service.
var SSO_LOGIN_URL = "https://pan.sjtu.edu.cn/user/v1/sign-in/sso-login-redirect/xpw8ou8y?auto_redirect=true&from=web&custom_state=4ycSqbzfqM9mPuzOKmvTUQ%25253D%25253D"
var SSO_VERIFY_URL = "https://pan.sjtu.edu.cn/user/v1/sign-in/verify-account-login/xpw8ou8y"
var JACCOUNT_COOKIE_URL = "https://jaccount.sjtu.edu.cn/jaccount/"

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &SJTUTBox{}
	})
}
