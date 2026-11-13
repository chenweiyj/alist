package webdav

import (
	"context"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/server/common"
)

const (
	trashDeleteHeader = "X-AList-Trash"
	trashReceiptTTL   = time.Minute
	trashReceiptLimit = 2048
)

type trashPathKind uint8

const (
	trashOutside trashPathKind = iota
	trashDirectory
	trashItem
)

func trashDeleteEnabled(r *http.Request) bool {
	values := r.Header.Values(trashDeleteHeader)
	return len(values) == 1 && strings.TrimSpace(values[0]) == "1"
}

func classifyTrashPath(raw string) trashPathKind {
	if !strings.HasPrefix(raw, "/") || strings.ContainsAny(raw, "\\\x00") {
		return trashOutside
	}
	clean := strings.TrimSuffix(raw, "/")
	if clean != path.Clean(clean) {
		return trashOutside
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if parts[0] != ".Trashes" {
		return trashOutside
	}
	if len(parts) == 1 {
		return trashDirectory
	}
	if parts[1] == "" {
		return trashOutside
	}
	for _, digit := range parts[1] {
		if digit < '0' || digit > '9' {
			return trashOutside
		}
	}
	if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
		return trashOutside
	}
	if len(parts) == 2 {
		return trashDirectory
	}
	return trashItem
}

func classifyUserTrashPath(user *model.User, raw string) trashPathKind {
	clean := strings.TrimSuffix(raw, "/")
	if clean != path.Clean(clean) {
		return trashOutside
	}
	base := path.Clean("/" + user.BasePath)
	if base != "/" && strings.HasPrefix(raw, base+"/") {
		raw = strings.TrimPrefix(raw, base)
	}
	return classifyTrashPath(raw)
}

type trashReceiptKey struct {
	userID      uint
	destination string
}

type trashDeleteReceipt struct {
	source  string
	object  model.Object
	expires time.Time
}

// Receipts only acknowledge rclone's post-MOVE metadata probe. They never hold
// file contents or appear in directory listings and are scoped to the DAV user.
type trashDeleteReceipts struct {
	mu          sync.Mutex
	entries     map[trashReceiptKey]trashDeleteReceipt
	directories map[trashReceiptKey]struct{}
}

func (c *trashDeleteReceipts) rememberDirectory(key trashReceiptKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.directories == nil {
		c.directories = make(map[trashReceiptKey]struct{})
	}
	if len(c.directories) >= trashReceiptLimit {
		for old := range c.directories {
			delete(c.directories, old)
			break
		}
	}
	c.directories[key] = struct{}{}
}

func (c *trashDeleteReceipts) directoryNames(userID uint, root string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for key := range c.directories {
		if key.userID == userID && path.Dir(key.destination) == root {
			names = append(names, path.Base(key.destination))
		}
	}
	return names
}

func (c *trashDeleteReceipts) get(key trashReceiptKey) (trashDeleteReceipt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	receipt, ok := c.entries[key]
	if ok && !time.Now().Before(receipt.expires) {
		delete(c.entries, key)
		return trashDeleteReceipt{}, false
	}
	return receipt, ok
}

func (c *trashDeleteReceipts) put(key trashReceiptKey, source string, object model.Obj) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[trashReceiptKey]trashDeleteReceipt)
	}
	now := time.Now()
	for key, receipt := range c.entries {
		if !now.Before(receipt.expires) {
			delete(c.entries, key)
		}
	}
	if len(c.entries) >= trashReceiptLimit {
		var oldestKey trashReceiptKey
		var oldest time.Time
		for key, receipt := range c.entries {
			if oldest.IsZero() || receipt.expires.Before(oldest) {
				oldestKey, oldest = key, receipt.expires
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = trashDeleteReceipt{
		source: source,
		object: model.Object{Name: path.Base(key.destination), Size: object.GetSize(),
			IsFolder: object.IsDir(), Modified: object.ModTime(), Ctime: object.CreateTime()},
		expires: now.Add(trashReceiptTTL),
	}
}

func (c *trashDeleteReceipts) remove(userID uint, requestPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.userID == userID && (key.destination == requestPath ||
			strings.HasPrefix(key.destination, strings.TrimSuffix(requestPath, "/")+"/")) {
			delete(c.entries, key)
		}
	}
}

func canTrashDelete(permission int32) bool {
	return common.HasPermission(permission, common.PermRemove) &&
		common.HasPermission(permission, common.PermWebdavManage)
}

func (h *Handler) deleteForTrash(r *http.Request, source, destination string) (int, error) {
	ctx := r.Context()
	user := ctx.Value("user").(*model.User)
	if !canTrashDelete(common.MergeRolePermissions(user, source)) {
		return http.StatusForbidden, errs.PermissionDenied
	}
	key := trashReceiptKey{user.ID, destination}
	// A retry must not delete a new object recreated at the same source path.
	if receipt, ok := h.trashDeletes.get(key); ok && receipt.source == source {
		return http.StatusCreated, nil
	}
	object, err := fs.Get(ctx, source, &fs.GetArgs{})
	if err != nil {
		return putErrorStatus(err), err
	}
	if err := fs.Remove(ctx, source); err != nil {
		return putErrorStatus(err), err
	}
	h.trashDeletes.put(key, source, object)
	return http.StatusCreated, nil
}

// Opted-in clients see an empty Trash namespace without creating remote folders.
// Pending VFS uploads into that namespace are discarded rather than resurrected.
func (h *Handler) handleTrashNamespace(w http.ResponseWriter, r *http.Request) (bool, int, error) {
	if !trashDeleteEnabled(r) {
		return false, 0, nil
	}
	raw, _, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return false, 0, nil
	}
	ctx := context.WithValue(r.Context(), "userAgent", r.UserAgent())
	user := ctx.Value("user").(*model.User)
	kind := classifyUserTrashPath(user, raw)
	if kind == trashOutside {
		return false, 0, nil
	}
	requestPath, err := ResolvePath(user, raw)
	if err != nil {
		return true, http.StatusForbidden, err
	}
	key := trashReceiptKey{user.ID, requestPath}
	root := path.Join("/", user.BasePath, ".Trashes")
	if requestPath != root {
		relative := strings.TrimPrefix(requestPath, root+"/")
		uid := strings.SplitN(relative, "/", 2)[0]
		h.trashDeletes.rememberDirectory(trashReceiptKey{user.ID, path.Join(root, uid)})
	}
	object := model.Object{Name: path.Base(requestPath), IsFolder: kind == trashDirectory,
		Modified: time.Now(), Ctime: time.Now()}
	switch r.Method {
	case "MKCOL":
		if kind == trashDirectory {
			return true, http.StatusCreated, nil
		}
		return true, http.StatusMethodNotAllowed, errs.NotSupport
	case "DELETE", "PUT":
		if !canTrashDelete(common.MergeRolePermissions(user, requestPath)) {
			return true, http.StatusForbidden, errs.PermissionDenied
		}
		release, status, err := h.confirmLocks(r, requestPath, "")
		if err != nil {
			return true, status, err
		}
		defer release()
		if r.Method == "DELETE" {
			h.trashDeletes.remove(user.ID, requestPath)
			return true, http.StatusNoContent, nil
		}
		if kind != trashItem {
			return true, http.StatusMethodNotAllowed, errs.NotSupport
		}
		size, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return true, http.StatusBadRequest, err
		}
		object.Size = size
		h.trashDeletes.put(key, "", &object)
		return true, http.StatusCreated, nil
	case "PROPFIND":
		depth := infiniteDepth
		if value := r.Header.Get("Depth"); value != "" {
			depth = parseDepth(value)
			if depth == invalidDepth {
				return true, http.StatusBadRequest, errInvalidDepth
			}
		}
		if kind == trashItem {
			receipt, ok := h.trashDeletes.get(key)
			if !ok || depth != 0 {
				return true, http.StatusNotFound, errs.ObjectNotFound
			}
			object = receipt.object
		}
		pf, status, err := readPropfind(r.Body)
		if err != nil {
			return true, status, err
		}
		mw := multistatusWriter{w: w}
		write := func(info model.Obj, href string) error {
			var pstats []Propstat
			var err error
			if pf.Propname != nil {
				names, err := propnames(ctx, h.LockSystem, info)
				if err != nil {
					return err
				}
				stat := Propstat{Status: http.StatusOK}
				for _, name := range names {
					stat.Props = append(stat.Props, Property{XMLName: name})
				}
				pstats = []Propstat{stat}
			} else if pf.Allprop != nil {
				pstats, err = allprop(ctx, h.LockSystem, info, pf.Prop)
			} else {
				pstats, err = props(ctx, h.LockSystem, info, pf.Prop)
			}
			if err != nil {
				return err
			}
			return mw.write(makePropstatResponse(href, pstats))
		}
		href := r.URL.EscapedPath()
		if object.IsDir() && !strings.HasSuffix(href, "/") {
			href += "/"
		}
		if err := write(&object, href); err != nil {
			return true, http.StatusInternalServerError, err
		}
		if requestPath == root && depth != 0 {
			for _, name := range h.trashDeletes.directoryNames(user.ID, root) {
				info := model.Object{Name: name, IsFolder: true, Modified: object.Modified, Ctime: object.Ctime}
				if err := write(&info, strings.TrimSuffix(href, "/")+"/"+name+"/"); err != nil {
					return true, http.StatusInternalServerError, err
				}
			}
		}
		return true, 0, mw.close()
	case "OPTIONS":
		return false, 0, nil
	default:
		return true, http.StatusNotFound, errs.ObjectNotFound
	}
}
