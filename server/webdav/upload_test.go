package webdav

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/alist-org/alist/v3/internal/errs"
)

func TestPutErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"dial timeout", &url.Error{Op: "Put", URL: "https://storage.example/upload",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}}, http.StatusGatewayTimeout},
		{"wrapped timeout", fmt.Errorf("upload: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{"missing object", fmt.Errorf("upload: %w", errs.ObjectNotFound), http.StatusNotFound},
		{"missing storage", errs.StorageNotFound, http.StatusNotFound},
		{"permission", fmt.Errorf("upload: %w", errs.PermissionDenied), http.StatusForbidden},
		{"filesystem permission", os.ErrPermission, http.StatusForbidden},
		{"unsupported upload", errs.UploadNotSupported, http.StatusMethodNotAllowed},
		{"unsupported operation", errs.NotSupport, http.StatusMethodNotAllowed},
		{"unimplemented", errs.NotImplement, http.StatusNotImplemented},
		{"other upload failure", errors.New("storage connection reset"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := putErrorStatus(tc.err); got != tc.want {
				t.Fatalf("HTTP status=%d want=%d for %v", got, tc.want, tc.err)
			}
		})
	}
}
