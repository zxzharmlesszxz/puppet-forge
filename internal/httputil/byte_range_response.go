package httputil

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type ByteRangeResponse struct {
	Range         ByteRange
	Partial       bool
	NotModified   bool
	Status        int
	ContentLength int64
}

func PrepareByteRangeResponse(response, request http.Header, size int64, etag, contentType string) (ByteRangeResponse, error) {
	result := ByteRangeResponse{Status: http.StatusOK, ContentLength: size}
	if etag != "" {
		response.Set("ETag", etag)
		if ETagMatches(request.Get("If-None-Match"), etag) {
			result.NotModified = true
			result.Status = http.StatusNotModified
			return result, nil
		}
	}

	response.Set("Accept-Ranges", "bytes")
	rangeHeader := request.Get("Range")
	if !IfRangeMatches(request.Get("If-Range"), etag) {
		rangeHeader = ""
	}
	byteRange, partial, err := ParseSingleByteRange(rangeHeader, size)
	if err != nil {
		response.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return ByteRangeResponse{}, err
	}

	result.Range = byteRange
	result.Partial = partial
	if partial {
		result.Status = http.StatusPartialContent
		result.ContentLength = byteRange.Length
		response.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.Start, byteRange.Start+byteRange.Length-1, size))
	}
	if contentType != "" {
		response.Set("Content-Type", contentType)
	}
	response.Set("Content-Length", strconv.FormatInt(result.ContentLength, 10))
	return result, nil
}

func ClearByteRangeResponseHeaders(header http.Header) {
	for _, name := range []string{"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type", "ETag"} {
		header.Del(name)
	}
}

func ETagMatches(raw, etag string) bool {
	if raw == "" || etag == "" {
		return false
	}
	for candidate := range strings.SplitSeq(raw, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func IfRangeMatches(raw, etag string) bool {
	raw = strings.TrimSpace(raw)
	return raw == "" || etag != "" && raw == etag
}
