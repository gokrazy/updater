package updater_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gokrazy/updater"
)

// trackingBody counts how many times Close is called on a response body.
type trackingBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (tb *trackingBody) Close() error {
	tb.closed.Add(1)
	return tb.ReadCloser.Close()
}

// trackingTransport wraps each response body so the test can compare the
// number of requests issued against the number of bodies closed.
type trackingTransport struct {
	base     http.RoundTripper
	closures *atomic.Int32
	requests *atomic.Int32
}

func (tt *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tt.requests.Add(1)
	resp, err := tt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &trackingBody{
		ReadCloser: resp.Body,
		closed:     tt.closures,
	}
	return resp, nil
}

// fakeGokrazy serves the minimal set of update endpoints exercised by the
// leak test.
func fakeGokrazy(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/update/features", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("partuuid,updatehash"))
	})

	mux.HandleFunc("/update/root", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.Header.Get("X-Gokrazy-Update-Hash") == "crc32" {
			h := crc32.NewIEEE()
			h.Write(body)
			w.Write([]byte(hex.EncodeToString(h.Sum(nil))))
		} else {
			h := sha256.Sum256(body)
			w.Write([]byte(hex.EncodeToString(h[:])))
		}
	})

	mux.HandleFunc("/update/switch", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/update/testboot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/reboot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResponseBodyLeak verifies that every response body opened while talking
// to a target is closed again. It fails before the body-closing fix (every
// body leaks) and passes afterwards.
func TestResponseBodyLeak(t *testing.T) {
	srv := fakeGokrazy(t)

	closures := &atomic.Int32{}
	requests := &atomic.Int32{}
	client := &http.Client{
		Transport: &trackingTransport{
			base:     srv.Client().Transport,
			closures: closures,
			requests: requests,
		},
	}

	ctx := context.Background()
	target, err := updater.NewTarget(ctx, srv.URL+"/", client)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}

	if err := target.StreamTo(ctx, "root", strings.NewReader("hello")); err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if err := target.Switch(ctx); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if err := target.Testboot(ctx); err != nil {
		t.Fatalf("Testboot: %v", err)
	}
	if err := target.Reboot(ctx); err != nil {
		t.Fatalf("Reboot: %v", err)
	}

	req := requests.Load()
	cls := closures.Load()
	if leaked := req - cls; leaked > 0 {
		t.Errorf("%d requests, %d bodies closed, %d bodies leaked", req, cls, leaked)
	}
}
