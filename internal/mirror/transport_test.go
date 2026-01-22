package mirror

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFallbackRoundTripperRetriesOnReset(t *testing.T) {
	var primaryCalls int
	var fallbackCalls int
	primary := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		primaryCalls++
		return nil, fmt.Errorf("wrap: %w", syscall.ECONNRESET)
	})
	fallback := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		fallbackCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})
	rt := &fallbackRoundTripper{
		primary:   primary,
		fallbacks: []http.RoundTripper{fallback},
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("expected fallback success, got error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("unexpected calls: primary=%d fallback=%d", primaryCalls, fallbackCalls)
	}
}

func TestFallbackRoundTripperSkipsNonRetryable(t *testing.T) {
	var primaryCalls int
	var fallbackCalls int
	primary := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		primaryCalls++
		return nil, fmt.Errorf("wrap: %w", syscall.ECONNRESET)
	})
	fallback := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		fallbackCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})
	rt := &fallbackRoundTripper{
		primary:   primary,
		fallbacks: []http.RoundTripper{fallback},
	}

	body := io.NopCloser(strings.NewReader("data"))
	req, err := http.NewRequest(http.MethodPost, "http://example.com", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.GetBody = nil
	req.ContentLength = 4

	resp, err := rt.RoundTrip(req)
	if err == nil || resp != nil {
		t.Fatalf("expected error without retry")
	}
	if primaryCalls != 1 || fallbackCalls != 0 {
		t.Fatalf("unexpected calls: primary=%d fallback=%d", primaryCalls, fallbackCalls)
	}
}
