package httputil

import (
	"errors"
	"strconv"
	"strings"
)

type ByteRange struct {
	Start  int64
	Length int64
}

func ParseSingleByteRange(raw string, size int64) (ByteRange, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ByteRange{}, false, nil
	}
	if size < 0 || !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") {
		return ByteRange{}, false, errors.New("invalid byte range")
	}
	value := strings.TrimSpace(strings.TrimPrefix(raw, "bytes="))
	startText, endText, ok := strings.Cut(value, "-")
	if !ok || (startText == "" && endText == "") {
		return ByteRange{}, false, errors.New("invalid byte range")
	}
	if startText == "" {
		suffix, err := strconv.ParseInt(endText, 10, 64)
		if err != nil || suffix <= 0 || size == 0 {
			return ByteRange{}, false, errors.New("invalid suffix byte range")
		}
		suffix = min(suffix, size)
		return ByteRange{Start: size - suffix, Length: suffix}, true, nil
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start < 0 || start >= size {
		return ByteRange{}, false, errors.New("byte range starts beyond object")
	}
	end := size - 1
	if endText != "" {
		end, err = strconv.ParseInt(endText, 10, 64)
		if err != nil || end < start {
			return ByteRange{}, false, errors.New("invalid byte range end")
		}
		end = min(end, size-1)
	}
	return ByteRange{Start: start, Length: end - start + 1}, true, nil
}
