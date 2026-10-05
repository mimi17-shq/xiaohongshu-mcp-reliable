// Modified from upstream: covers the fork's resilient publishing behavior.
package xiaohongshu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptsImage(t *testing.T) {
	cases := []struct {
		name   string
		accept string
		want   bool
	}{
		{"图片扩展名", ".png,.webp", true},
		{"大写扩展名", ".JPG", true},
		{"MIME 通配", "image/*", true},
		{"非图片扩展名", ".xyz,.abc", false},
		{"非图片 MIME", "video/mp4", false},
		{"空值", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, acceptsImage(c.accept))
		})
	}
}

func TestValidateUploadImagePathsRejectsAnyMissingImage(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.png")
	if err := os.WriteFile(existing, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := validateUploadImagePaths([]string{existing, filepath.Join(dir, "missing.png")})
	if err == nil || !strings.Contains(err.Error(), "第2张图片不存在或不可访问") {
		t.Fatalf("error=%v, want missing second image error", err)
	}
}

func TestValidateUploadImagePathsAcceptsReadableFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateUploadImagePaths([]string{path}); err != nil {
		t.Fatal(err)
	}
}
