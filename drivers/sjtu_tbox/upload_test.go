package sjtu_tbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/stream"
)

type uploadRoundTripper func(*http.Request) (*http.Response, error)

func (f uploadRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func dialTimeout() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
}

func TestStorageUploadReusesConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || (string(body) != "payload" && len(body) != 0) || r.ContentLength != int64(len(body)) {
			t.Errorf("upload body=%q length=%d err=%v", body, r.ContentLength, err)
		}
		if len(r.TransferEncoding) != 0 {
			t.Errorf("known-size upload must not be chunked: %v", r.TransferEncoding)
		}
		if r.Header.Get("X-Upload-Token") != "signature" {
			t.Error("upload headers missing")
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "uploaded")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	client := newStorageUploadClient()
	defer client.CloseIdleConnections()
	for i := 0; i < 3; i++ {
		payload := "payload"
		if i == 1 {
			payload = ""
		}
		if err := uploadToStorage(t.Context(), client, server.URL,
			map[string]string{"X-Upload-Token": "signature"}, strings.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("opened %d TCP connections for 3 uploads, want 1", got)
	}
}

func TestPutRetriesDialFailureWithoutConsumingStream(t *testing.T) {
	ensureTestConfig()
	const payload = "complete single-pass upload"
	var received atomic.Value
	received.Store("")
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		received.Store(string(body))
		if r.Header.Get("X-Upload-Token") != "signature" {
			t.Error("upload signature header missing")
		}
		if cookie := r.Header.Get("Cookie"); cookie != "" {
			t.Errorf("API cookies leaked to storage: %q", cookie)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	var initialized, confirmed atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPut:
			initialized.Add(1)
			json.NewEncoder(w).Encode(UploadInitResp{
				Domain: strings.TrimPrefix(storage.URL, "https://"), Path: "/upload",
				Headers: map[string]string{"X-Upload-Token": "signature"}, ConfirmKey: "confirm-key",
			})
		case http.MethodPost:
			confirmed.Add(1)
			if got := received.Load().(string); got != payload {
				t.Errorf("confirmed incomplete upload: %q", got)
			}
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected API method: %s", r.Method)
		}
	}))
	defer api.Close()
	previousURL, previousClient := API_URL, storageUploadClient
	API_URL = api.URL
	client := storage.Client()
	transport := client.Transport
	var attempts int
	client.Transport = uploadRoundTripper(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			r.Body.Close()
			return nil, dialTimeout()
		}
		return transport.RoundTrip(r)
	})
	storageUploadClient = client
	t.Cleanup(func() { API_URL, storageUploadClient = previousURL, previousClient })
	file := &stream.FileStream{
		Obj: &model.Object{Name: "upload.txt", Size: int64(len(payload))}, Reader: strings.NewReader(payload),
	}
	defer file.Close()
	obj, err := cachedTestDriver().Put(t.Context(), &model.Object{Path: "/data"}, file, func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || initialized.Load() != 1 || confirmed.Load() != 1 || received.Load().(string) != payload || obj.GetSize() != int64(len(payload)) {
		t.Fatalf("attempts=%d init=%d confirm=%d received=%q size=%d", attempts, initialized.Load(), confirmed.Load(), received.Load(), obj.GetSize())
	}
}

func TestStorageUploadRetrySafety(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consume  bool
		cancel   bool
		attempts int
	}{
		{name: "bounded dial retries", attempts: 3},
		{name: "consumed stream is not replayed", consume: true, attempts: 1},
		{name: "cancel stops retries", cancel: true, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attempts := 0
			client := &http.Client{Transport: uploadRoundTripper(func(r *http.Request) (*http.Response, error) {
				attempts++
				if r.GetBody != nil {
					t.Error("single-pass body must not enable HTTP transport replays")
				}
				if tc.consume {
					buffer := make([]byte, 1)
					if _, err := r.Body.Read(buffer); err != nil {
						t.Fatal(err)
					}
				}
				r.Body.Close()
				if tc.cancel {
					cancel()
				}
				return nil, dialTimeout()
			})}
			err := uploadToStorage(ctx, client, "https://storage.example/upload", nil, strings.NewReader("payload"), 7)
			if err == nil || attempts != tc.attempts {
				t.Fatalf("attempts=%d want=%d err=%v", attempts, tc.attempts, err)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled upload returned %v", err)
			}
		})
	}
}
