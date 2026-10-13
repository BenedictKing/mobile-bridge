package uploads

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestUploadPutThumbAndPreview(t *testing.T) {
	manager := New(t.TempDir())
	id := uuid.NewString()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 32)...)
	row, err := manager.Put("thread-1", id, "photo.png", png)
	if err != nil || row.MustGet("image") != "image/png" {
		t.Fatalf("put: %v %v", row, err)
	}
	thumb := append([]byte("RIFF1234WEBP"), bytes.Repeat([]byte("t"), 16)...)
	if _, err = manager.SetThumb("thread-1", id, thumb, 6, 4); err != nil {
		t.Fatal(err)
	}
	preview, variant, err := manager.Preview("thread-1", id, "thumb")
	if err != nil || variant != "thumb" || preview.MustGet("previewMime") != "image/webp" {
		t.Fatalf("preview: %v %s %v", preview, variant, err)
	}
}
