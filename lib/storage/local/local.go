package local

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/verbeux-ai/whatsmiau/interfaces"
)

var _ interfaces.Storage = (*Local)(nil)

// Local saves files to a directory and returns URLs using a configurable base URL (e.g. http://localhost:8080).
type Local struct {
	dir     string
	baseURL string
}

// New creates a local storage that saves to dir and returns URLs as baseURL/media/fileName.
// baseURL should not end with slash (e.g. http://localhost:8080).
func New(dir, baseURL string) (*Local, error) {
	dir = strings.TrimSuffix(dir, string(os.PathSeparator))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Local{dir: dir, baseURL: baseURL}, nil
}

func (s *Local) Upload(ctx context.Context, fileName, mimetype string, file io.Reader) (string, string, error) {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return "", "", err
	}
	path := filepath.Join(s.dir, fileName)
	out, err := os.Create(path)
	if err != nil {
		return "", "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	url := s.baseURL + "/media/" + fileName
	return url, fileName, nil
}

func (s *Local) UploadBase64IfDontExists(ctx context.Context, fileName, mimetype, b64 string) (string, error) {
	path := filepath.Join(s.dir, fileName)
	if _, err := os.Stat(path); err == nil {
		return s.baseURL + "/media/" + filepath.Base(fileName), nil
	}
	return s.UploadBase64(ctx, fileName, mimetype, b64)
}

func (s *Local) UploadBase64(ctx context.Context, fileName, mimetype, b64 string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(fileName)
	if ext == "" && mimetype != "" {
		if exts, _ := mime.ExtensionsByType(mimetype); len(exts) > 0 {
			ext = exts[0]
		}
	}
	if ext == "" {
		ext = ".bin"
	}
	// keep fileName as-is if it already has extension, else use uuid + ext
	if filepath.Ext(fileName) == "" {
		fileName = fileName + ext
	}
	url, _, err := s.Upload(ctx, fileName, mimetype, bytes.NewReader(decoded))
	if err != nil {
		return "", err
	}
	return url, nil
}
