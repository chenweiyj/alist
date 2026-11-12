package sjtu_tbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
)

type SJTUTBox struct {
	model.Storage
	Addition
	ref *SJTUTBox

	accessToken         string
	libraryId           string
	spaceId             string
	userToken           string
	userID              string
	configuredUserToken string
	tokenExpires        time.Time
	userTokenExpires    time.Time
	authenticatedCookie string
	tokenMutex          sync.Mutex
}

func (d *SJTUTBox) Config() driver.Config {
	return config
}

func (d *SJTUTBox) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *SJTUTBox) Init(ctx context.Context) error {
	return d.refreshToken(ctx)
}

func (d *SJTUTBox) InitReference(storage driver.Driver) error {
	refStorage, ok := storage.(*SJTUTBox)
	if ok {
		d.ref = refStorage
		return nil
	}
	return errs.NotSupport
}

func (d *SJTUTBox) Drop(ctx context.Context) error {
	d.ref = nil
	return nil
}

func (d *SJTUTBox) GetRoot(ctx context.Context) (model.Obj, error) {
	return &model.Object{
		ID:       "root",
		Path:     d.rootPath(),
		Name:     "root",
		Size:     0,
		IsFolder: true,
	}, nil
}

func (d *SJTUTBox) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	listURL := fmt.Sprintf("%s/directory/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(dir.GetPath()))

	var allObjs []model.Obj
	page := 1

	for {
		var listResp FolderListResp
		httpResp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParams(map[string]string{
				"access_token":  d.accessToken,
				"page":          strconv.Itoa(page),
				"page_size":     "200",
				"order_by":      d.OrderBy,
				"order_by_type": d.OrderByType,
				"space_org_id":  "1",
			}).
			SetResult(&listResp).
			Execute(http.MethodGet, listURL)

		if err != nil {
			return nil, err
		}
		if httpResp.IsError() {
			return nil, fmt.Errorf("list failed: status=%d body=%s", httpResp.StatusCode(), responseBody(httpResp))
		}

		objs, err := utils.SliceConvert(listResp.Contents, func(src NetdiskObj) (model.Obj, error) {
			fileSize, _ := strconv.ParseInt(src.Size, 10, 64)

			var objPath string
			dirPath := dir.GetPath()
			if dirPath == "" || dirPath == "/" {
				objPath = "/" + src.Name
			} else {
				objPath = path.Join(dirPath, src.Name)
			}

			return &model.Object{
				Name:     src.Name,
				Size:     fileSize,
				IsFolder: src.Type != "file",
				Modified: src.ModificationTime,
				Ctime:    src.CreationTime,
				Path:     objPath,
			}, nil
		})

		if err != nil {
			return nil, err
		}

		allObjs = append(allObjs, objs...)

		// stop if we have fetched all items
		if len(allObjs) >= listResp.TotalNum || len(listResp.Contents) < 200 {
			break
		}
		page++
	}

	return allObjs, nil
}

func (d *SJTUTBox) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	linkURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(file.GetPath()))

	client := d.newClient()
	client.SetRedirectPolicy(resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}))

	resp, err := client.R().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"access_token":        d.accessToken,
			"content_disposition": "attachment",
			"purpose":             "download",
			"space_org_id":        "1",
		}).
		Execute(http.MethodGet, linkURL)

	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, fmt.Errorf("link failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}

	// Keep the presigned URL redirect un-followed. Different SMH deployments
	// use 301/302 or the equivalent temporary redirect codes.
	switch resp.StatusCode() {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode())
	}

	s3URL := resp.Header().Get("Location")
	if s3URL == "" {
		return nil, fmt.Errorf("no Location header in redirect response")
	}

	// parse TTL from S3 presigned URL's X-Amz-Expires parameter
	ttl := 2 * time.Hour
	if u, parseErr := url.Parse(s3URL); parseErr == nil {
		if expiresSec, convErr := strconv.Atoi(u.Query().Get("X-Amz-Expires")); convErr == nil && expiresSec > 0 {
			ttl = time.Duration(expiresSec) * time.Second
		}
	}

	return &model.Link{
		URL:        s3URL,
		Expiration: &ttl,
	}, nil
}

func (d *SJTUTBox) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	infoURL := fmt.Sprintf("%s/directory/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(buildPath(parentDir.GetPath(), dirName)))

	resp, err := d.newClient().R().
		SetContext(ctx).
		SetQueryParam("access_token", d.accessToken).
		SetQueryParam("conflict_resolution_strategy", "ask").
		Execute(http.MethodPut, infoURL)

	if err != nil {
		return nil, err
	}
	if resp.IsError() && resp.StatusCode() != http.StatusConflict {
		return nil, fmt.Errorf("create directory failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}

	// A duplicate directory (409) is treated as success for idempotency. The
	// API has returned both 200 and 201 for a newly created directory.
	if resp.StatusCode() != http.StatusConflict && (resp.StatusCode() < 200 || resp.StatusCode() >= 300) {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode())
	}

	return &model.Object{
		Name:     dirName,
		IsFolder: true,
		Modified: time.Now(),
		Ctime:    time.Now(),
		Path:     buildPath(parentDir.GetPath(), dirName),
	}, nil
}

func (d *SJTUTBox) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	srcPath := srcObj.GetPath()
	targetPath := path.Join(dstDir.GetPath(), srcObj.GetName())

	// file move: use overwrite strategy
	if !srcObj.IsDir() {
		apiURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(targetPath))

		var moveResp MoveCopyResp
		resp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParam("access_token", d.accessToken).
			SetQueryParam("conflict_resolution_strategy", "overwrite").
			SetBody(map[string]interface{}{"from": srcPath}).
			SetResult(&moveResp).
			Execute(http.MethodPut, apiURL)

		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			return nil, fmt.Errorf("move failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
		}

		actualName := srcObj.GetName()
		if len(moveResp.Path) > 0 {
			actualName = moveResp.Path[len(moveResp.Path)-1]
		}
		return &model.Object{
			Name:     actualName,
			Size:     srcObj.GetSize(),
			IsFolder: false,
			Modified: srcObj.ModTime(),
			Ctime:    srcObj.CreateTime(),
			Path:     buildPath(dstDir.GetPath(), actualName),
		}, nil
		// folder move
	} else {
		// use ask strategy to detect conflict
		err := d.moveItem(ctx, srcPath, dstDir.GetPath(), srcObj.GetName(), true, "ask")
		if err == nil {
			return &model.Object{
				Name:     srcObj.GetName(),
				Size:     srcObj.GetSize(),
				IsFolder: true,
				Modified: srcObj.ModTime(),
				Ctime:    srcObj.CreateTime(),
				Path:     targetPath,
			}, nil
		}

		// 409 means conflict name, needs merge
		if !errors.Is(err, errs.ObjectNotFound) {
			return nil, err
		}

		// list all child item of srcObj
		children, listErr := d.List(ctx, srcObj, model.ListArgs{})
		if listErr != nil {
			return nil, fmt.Errorf("merge: list source folder failed: %w", listErr)
		}

		// move every child item to dstDir recursively
		for _, child := range children {
			childSrcPath := path.Join(srcPath, child.GetName())
			targetDstPath := targetPath

			// subfolder: recursive Move
			if child.IsDir() {
				childErr := d.moveItem(ctx, childSrcPath, targetDstPath, child.GetName(), true, "ask")
				if childErr != nil && errors.Is(childErr, errs.ObjectNotFound) {
					childObj := &model.Object{
						Name:     child.GetName(),
						Path:     childSrcPath,
						Size:     child.GetSize(),
						IsFolder: true,
					}
					_, mergeErr := d.Move(ctx, childObj, &model.Object{
						Path:     targetPath,
						Name:     srcObj.GetName(),
						IsFolder: true,
					})
					if mergeErr != nil {
						return nil, fmt.Errorf("merge subfolder %s: %w", child.GetName(), mergeErr)
					}
				} else if childErr != nil {
					return nil, fmt.Errorf("merge subfolder %s: %w", child.GetName(), childErr)
				}
			} else {
				// child item is file
				fileErr := d.moveItem(ctx, childSrcPath, targetDstPath, child.GetName(), false, "overwrite")
				if fileErr != nil {
					return nil, fmt.Errorf("merge file %s: %w", child.GetName(), fileErr)
				}
			}
		}

		// delete empty srcObj folder
		_ = d.Remove(ctx, srcObj)

		return &model.Object{
			Name:     srcObj.GetName(),
			Size:     srcObj.GetSize(),
			IsFolder: true,
			Modified: srcObj.ModTime(),
			Ctime:    srcObj.CreateTime(),
			Path:     targetPath,
		}, nil
	}
}

func (d *SJTUTBox) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	parentPath := path.Dir(strings.ReplaceAll(srcObj.GetPath(), "\\", "/"))
	dstPath := buildPath(parentPath, newName)
	// The cloud API's JSON source path is relative to the space root.
	srcPath := strings.TrimPrefix(strings.ReplaceAll(srcObj.GetPath(), "\\", "/"), "/")

	// file rename
	if !srcObj.IsDir() {
		renameURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(dstPath))

		var moveResp MoveCopyResp
		resp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParam("access_token", d.accessToken).
			SetQueryParam("conflict_resolution_strategy", "overwrite").
			SetBody(map[string]interface{}{"from": srcPath}).
			SetResult(&moveResp).
			Execute(http.MethodPut, renameURL)

		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			return nil, fmt.Errorf("rename failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
		}

		actualName := newName
		if len(moveResp.Path) > 0 {
			actualName = moveResp.Path[len(moveResp.Path)-1]
		}
		return &model.Object{
			Name:     actualName,
			Size:     srcObj.GetSize(),
			IsFolder: false,
			Modified: srcObj.ModTime(),
			Ctime:    srcObj.CreateTime(),
			Path:     buildPath(parentPath, actualName),
		}, nil
	} else {
		// folder rename
		renameURL := fmt.Sprintf("%s/directory/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(dstPath))

		resp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParam("access_token", d.accessToken).
			SetQueryParam("conflict_resolution_strategy", "ask").
			SetQueryParam("move_authority", "true").
			SetBody(map[string]interface{}{"from": srcPath}).
			Execute(http.MethodPut, renameURL)

		if err != nil {
			return nil, err
		}
		if resp.IsError() && resp.StatusCode() != http.StatusConflict {
			return nil, fmt.Errorf("rename failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
		}

		// Any successful response means the move completed. The API normally
		// returns 204, but some deployments return a JSON response with 200.
		if resp.StatusCode() >= 200 && resp.StatusCode() < 300 {
			return &model.Object{
				Name:     newName,
				Size:     srcObj.GetSize(),
				IsFolder: true,
				Modified: srcObj.ModTime(),
				Ctime:    srcObj.CreateTime(),
				Path:     dstPath,
			}, nil
		}

		// 409 means conflict name
		if resp.StatusCode() == http.StatusConflict {
			children, listErr := d.List(ctx, srcObj, model.ListArgs{})
			if listErr != nil {
				return nil, fmt.Errorf("rename merge: list source folder failed: %w", listErr)
			}

			targetDirObj := &model.Object{Path: dstPath, Name: newName, IsFolder: true}

			for _, child := range children {
				childSrcPath := path.Join(srcObj.GetPath(), child.GetName())

				if child.IsDir() {
					childObj := &model.Object{
						Name: child.GetName(), Path: childSrcPath,
						Size: child.GetSize(), IsFolder: true,
					}
					if _, mergeErr := d.Move(ctx, childObj, targetDirObj); mergeErr != nil {
						return nil, fmt.Errorf("rename merge subfolder %s: %w", child.GetName(), mergeErr)
					}
				} else {
					fileErr := d.moveItem(ctx, childSrcPath, dstPath, child.GetName(), false, "overwrite")
					if fileErr != nil {
						return nil, fmt.Errorf("rename merge file %s: %w", child.GetName(), fileErr)
					}
				}
			}

			_ = d.Remove(ctx, srcObj)

			return &model.Object{
				Name:     newName,
				Size:     srcObj.GetSize(),
				IsFolder: true,
				Modified: srcObj.ModTime(),
				Ctime:    srcObj.CreateTime(),
				Path:     dstPath,
			}, nil
		}

		return nil, fmt.Errorf("unexpected rename folder response: status=%d", resp.StatusCode())
	}
}

func (d *SJTUTBox) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	srcPath := srcObj.GetPath()
	targetPath := path.Join(dstDir.GetPath(), srcObj.GetName())

	// file copy
	if !srcObj.IsDir() {
		copyURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(targetPath))

		var copyResp MoveCopyResp
		resp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParam("access_token", d.accessToken).
			SetQueryParam("conflict_resolution_strategy", "overwrite").
			SetBody(map[string]interface{}{"copyFrom": srcPath}).
			SetResult(&copyResp).
			Execute(http.MethodPut, copyURL)

		if err != nil {
			return nil, err
		}
		if resp.IsError() {
			return nil, fmt.Errorf("copy failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
		}

		actualName := srcObj.GetName()
		if len(copyResp.Path) > 0 {
			actualName = copyResp.Path[len(copyResp.Path)-1]
		}
		return &model.Object{
			Name:     actualName,
			Size:     srcObj.GetSize(),
			IsFolder: false,
			Modified: srcObj.ModTime(),
			Ctime:    time.Now(),
			Path:     buildPath(dstDir.GetPath(), actualName),
		}, nil
	} else {
		// folder copy
		infoURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(targetPath))

		resp, err := d.newClient().R().
			SetContext(ctx).
			SetQueryParam("access_token", d.accessToken).
			SetQueryParam("conflict_resolution_strategy", "ask").
			SetBody(map[string]interface{}{"copyFrom": srcPath}).
			Execute(http.MethodPut, infoURL)

		if err != nil {
			return nil, err
		}
		if resp.IsError() && resp.StatusCode() != http.StatusConflict {
			return nil, fmt.Errorf("copy failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
		}

		if resp.StatusCode() == http.StatusConflict {
			children, listErr := d.List(ctx, srcObj, model.ListArgs{})
			if listErr != nil {
				return nil, fmt.Errorf("copy merge: list source folder failed: %w", listErr)
			}

			targetDirObj := &model.Object{
				Path:     targetPath,
				Name:     srcObj.GetName(),
				IsFolder: true,
			}

			for _, child := range children {
				childSrcPath := path.Join(srcPath, child.GetName())

				if child.IsDir() {
					childObj := &model.Object{
						Name:     child.GetName(),
						Path:     childSrcPath,
						Size:     child.GetSize(),
						IsFolder: true,
					}
					if _, mergeErr := d.Copy(ctx, childObj, targetDirObj); mergeErr != nil {
						return nil, fmt.Errorf("copy merge subfolder %s: %w", child.GetName(), mergeErr)
					}
				} else {
					childEncoded := d.encodePath(path.Join(targetPath, child.GetName()))
					childURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, childEncoded)
					if _, fileErr := d.newClient().R().
						SetContext(ctx).
						SetQueryParam("access_token", d.accessToken).
						SetQueryParam("conflict_resolution_strategy", "overwrite").
						SetBody(map[string]interface{}{"copyFrom": childSrcPath}).
						Execute(http.MethodPut, childURL); fileErr != nil {
						return nil, fmt.Errorf("copy merge file %s: %w", child.GetName(), fileErr)
					}
				}
			}

			return &model.Object{
				Name:     srcObj.GetName(),
				Size:     srcObj.GetSize(),
				IsFolder: true,
				Modified: srcObj.ModTime(),
				Ctime:    time.Now(),
				Path:     targetPath,
			}, nil
		}

		// 200/201 means the operation completed synchronously; 202 requires
		// polling the task endpoint.
		if resp.StatusCode() >= 200 && resp.StatusCode() < 300 && resp.StatusCode() != http.StatusAccepted {
			return &model.Object{
				Name:     srcObj.GetName(),
				Size:     srcObj.GetSize(),
				IsFolder: true,
				Modified: srcObj.ModTime(),
				Ctime:    time.Now(),
				Path:     targetPath,
			}, nil
		}

		if resp.StatusCode() != http.StatusAccepted {
			return nil, fmt.Errorf("unexpected copy folder response: status=%d", resp.StatusCode())
		}

		var taskResp TaskInitResp
		if err := json.Unmarshal(resp.Body(), &taskResp); err != nil {
			return nil, fmt.Errorf("failed to parse task id: %w", err)
		}
		if taskResp.TaskId == 0 {
			return nil, fmt.Errorf("failed to parse task id: response did not contain a task id")
		}

		taskURL := fmt.Sprintf("%s/task/%s/%s/%d", API_URL, d.libraryId, d.spaceId, taskResp.TaskId)
		actualName := srcObj.GetName()
		taskDone := false

		for i := 0; i < 30 && !taskDone; i++ {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1 * time.Second):
			}

			var taskStatus []TaskStatusItem
			pollResp, pollErr := d.newClient().R().
				SetContext(ctx).
				SetQueryParam("access_token", d.accessToken).
				SetResult(&taskStatus).
				Execute(http.MethodGet, taskURL)

			if pollErr != nil {
				return nil, pollErr
			}
			if pollResp.IsError() {
				return nil, fmt.Errorf("copy task polling failed: status=%d body=%s", pollResp.StatusCode(), responseBody(pollResp))
			}
			if len(taskStatus) == 0 {
				continue
			}

			switch taskStatus[0].Status {
			case 200:
				if taskStatus[0].Result != nil && len(taskStatus[0].Result.Path) > 0 {
					actualName = taskStatus[0].Result.Path[len(taskStatus[0].Result.Path)-1]
				}
				taskDone = true
			case 204:
				taskDone = true
			}
		}

		return &model.Object{
			Name:     actualName,
			Size:     srcObj.GetSize(),
			IsFolder: true,
			Modified: srcObj.ModTime(),
			Ctime:    time.Now(),
			Path:     buildPath(dstDir.GetPath(), actualName),
		}, nil
	}
}

func (d *SJTUTBox) Remove(ctx context.Context, obj model.Obj) error {
	if err := d.refreshToken(ctx); err != nil {
		return err
	}

	removeURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(obj.GetPath()))
	resp, err := d.newClient().R().
		SetContext(ctx).
		SetQueryParam("access_token", d.accessToken).
		SetQueryParam("permanent", "0").
		SetQueryParam("space_org_id", "1").
		Execute(http.MethodDelete, removeURL)

	if err != nil {
		return err
	}
	if resp.IsError() {
		return fmt.Errorf("remove failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}

	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return fmt.Errorf("remove failed: status=%d", resp.StatusCode())
	}

	return nil
}

func (d *SJTUTBox) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if err := d.refreshToken(ctx); err != nil {
		return nil, err
	}

	parentPath := strings.TrimPrefix(strings.ReplaceAll(dstDir.GetPath(), "\\", "/"), "/")

	// step 1: apply for confirm key
	initURL := fmt.Sprintf("%s/file/%s/%s/%s", API_URL, d.libraryId, d.spaceId, d.encodePath(buildPath(parentPath, stream.GetName())))

	mainClient := d.newClient()

	var initResp UploadInitResp
	resp, err := mainClient.R().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"access_token":                 d.accessToken,
			"conflict_resolution_strategy": "overwrite",
			"filesize":                     strconv.FormatInt(stream.GetSize(), 10),
		}).
		SetBody("{}").
		SetResult(&initResp).
		Execute(http.MethodPut, initURL)

	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, fmt.Errorf("initialize upload failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}

	if initResp.Domain == "" || initResp.Path == "" || initResp.ConfirmKey == "" {
		return nil, fmt.Errorf("initialize upload failed: response did not contain upload URL or confirm key")
	}

	// step 2: upload file
	s3URL := fmt.Sprintf("https://%s%s", initResp.Domain, initResp.Path)

	if err := uploadToStorage(ctx, storageUploadClient, s3URL, initResp.Headers,
		driver.NewLimitedUploadStream(ctx, stream), stream.GetSize()); err != nil {
		return nil, err
	}

	// step 3: confirm the upload
	confirmURL := fmt.Sprintf("%s/file/%s/%s/%s?confirm", API_URL, d.libraryId, d.spaceId, initResp.ConfirmKey)

	var confirmResp ConfirmResp
	resp, err = mainClient.R().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"access_token":                 d.accessToken,
			"conflict_resolution_strategy": "overwrite",
		}).
		SetBody("{}").
		SetResult(&confirmResp).
		Execute(http.MethodPost, confirmURL)

	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, fmt.Errorf("confirm upload failed: status=%d body=%s", resp.StatusCode(), responseBody(resp))
	}

	return &model.Object{
		Name:     stream.GetName(),
		Size:     stream.GetSize(),
		IsFolder: false,
		Modified: time.Now(),
		Ctime:    time.Now(),
		Path:     buildPath(dstDir.GetPath(), stream.GetName()),
	}, nil
}

func (d *SJTUTBox) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	return nil, errs.NotSupport
}

var _ driver.Driver = (*SJTUTBox)(nil)
var _ driver.Other = (*SJTUTBox)(nil)
var _ driver.MkdirResult = (*SJTUTBox)(nil)
var _ driver.MoveResult = (*SJTUTBox)(nil)
var _ driver.RenameResult = (*SJTUTBox)(nil)
var _ driver.PutResult = (*SJTUTBox)(nil)
var _ driver.GetRooter = (*SJTUTBox)(nil)
