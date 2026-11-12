package sjtu_tbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

var storageUploadClient = newStorageUploadClient()

func newStorageUploadClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 16
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport}
}

type uploadReader struct {
	io.Reader
	started atomic.Bool
}

func (r *uploadReader) Read(p []byte) (int, error) {
	r.started.Store(true)
	return r.Reader.Read(p)
}

func uploadToStorage(ctx context.Context, client *http.Client, uploadURL string,
	headers map[string]string, body io.Reader, size int64) error {
	// Keep stream ownership with the caller: HTTP closes bodies even on dial failures.
	reader := &uploadReader{Reader: body}
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
		if err != nil {
			return err
		}
		request.ContentLength = size
		if size == 0 {
			request.Body = http.NoBody
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := client.Do(request)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
				return fmt.Errorf("upload failed: status=%d body=%s", response.StatusCode, body)
			}
			_, err = io.Copy(io.Discard, response.Body)
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The upload stream cannot be replayed once any read has started.
		var networkError *net.OpError
		if attempt >= 2 || reader.started.Load() || !errors.As(err, &networkError) || networkError.Op != "dial" {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
