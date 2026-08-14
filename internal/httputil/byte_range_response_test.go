package httputil

import (
	"net/http"
	"testing"
)

func TestPrepareByteRangeResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		request     http.Header
		wantStatus  int
		wantLength  int64
		wantRange   string
		wantPartial bool
		wantNotMod  bool
		wantErr     bool
	}{
		{name: "complete", request: http.Header{}, wantStatus: http.StatusOK, wantLength: 100},
		{name: "not modified", request: http.Header{"If-None-Match": {`W/"digest"`}}, wantStatus: http.StatusNotModified, wantLength: 100, wantNotMod: true},
		{name: "partial", request: http.Header{"Range": {"bytes=10-19"}}, wantStatus: http.StatusPartialContent, wantLength: 10, wantRange: "bytes 10-19/100", wantPartial: true},
		{name: "if range mismatch", request: http.Header{"Range": {"bytes=10-19"}, "If-Range": {`"other"`}}, wantStatus: http.StatusOK, wantLength: 100},
		{name: "invalid range", request: http.Header{"Range": {"bytes=200-300"}}, wantRange: "bytes */100", wantErr: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			responseHeader := make(http.Header)
			response, err := PrepareByteRangeResponse(responseHeader, testCase.request, 100, `"digest"`, "application/gzip")
			if (err != nil) != testCase.wantErr {
				t.Fatalf("PrepareByteRangeResponse() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if testCase.wantErr {
				if got := responseHeader.Get("Content-Range"); got != testCase.wantRange {
					t.Fatalf("Content-Range = %q, want %q", got, testCase.wantRange)
				}
				return
			}
			if response.Status != testCase.wantStatus || response.ContentLength != testCase.wantLength || response.Partial != testCase.wantPartial || response.NotModified != testCase.wantNotMod {
				t.Fatalf("response = %#v, want status=%d length=%d partial=%v notModified=%v", response, testCase.wantStatus, testCase.wantLength, testCase.wantPartial, testCase.wantNotMod)
			}
			if got := responseHeader.Get("Content-Range"); got != testCase.wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, testCase.wantRange)
			}
		})
	}
}

func TestClearByteRangeResponseHeaders(t *testing.T) {
	t.Parallel()

	header := http.Header{
		"Accept-Ranges":  {"bytes"},
		"Content-Length": {"10"},
		"Content-Range":  {"bytes 0-9/100"},
		"Content-Type":   {"application/gzip"},
		"ETag":           {`"digest"`},
		"X-Request-Id":   {"request-id"},
	}
	ClearByteRangeResponseHeaders(header)
	for _, name := range []string{"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type", "ETag"} {
		if value := header.Get(name); value != "" {
			t.Fatalf("%s = %q, want empty", name, value)
		}
	}
	if value := header.Get("X-Request-Id"); value != "request-id" {
		t.Fatalf("X-Request-Id = %q, want preserved", value)
	}
}
