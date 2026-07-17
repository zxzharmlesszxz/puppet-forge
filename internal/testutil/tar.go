package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
)

func BuildTarGz(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)

	for name, body := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
		if _, err := io.WriteString(tarWriter, body); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
