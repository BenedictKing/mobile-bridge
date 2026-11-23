package httpd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
)

func uploadGCServer(t *testing.T, root string) *Server {
	t.Helper()
	server := &Server{Uploads: uploads.New(root)}
	ledger, err := LoadSubmissions(filepath.Join(root, "submissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.Queue = ledger
	return server
}

// attach uploads one image and records a submission in the given status that
// references it, mirroring a send that carried the attachment.
func attach(t *testing.T, server *Server, thread, status string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := server.Uploads.Put(thread, id, "photo.png", []byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatal(err)
	}
	entry, _, err := server.Queue.enqueue(thread, uuid.NewString(), "带附件", "queue", "", nil, []string{id}, []string{"photo.png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Queue.setStatus(thread, entry.ID, status); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUploadGCProtectsPendingSubmissionAttachments(t *testing.T) {
	server := uploadGCServer(t, t.TempDir())
	protected := map[string]string{}
	for _, status := range []string{SubQueued, SubUnknown, SubAccepted, SubFailed} {
		protected[attach(t, server, "thread-1", status)] = status
	}
	cancelled := attach(t, server, "thread-1", SubCancelled)
	// Protection is scoped to the thread: the same attachment id may exist on
	// another thread whose own submission still needs it.
	shared := uuid.NewString()
	for _, thread := range []string{"thread-1", "thread-2"} {
		if _, err := server.Uploads.Put(thread, shared, "photo.png", []byte("\x89PNG\r\n\x1a\nimage")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := server.Queue.enqueue("thread-2", uuid.NewString(), "带附件", "queue", "", nil, []string{shared}, []string{"photo.png"}); err != nil {
		t.Fatal(err)
	}

	if removed := server.collectUploadsAt(time.Now().Add(30 * 24 * time.Hour)); removed != 2 {
		t.Fatalf("removed = %d, want 2 (cancelled and other-thread-only attachment)", removed)
	}
	for id, status := range protected {
		if _, err := server.Uploads.Get("thread-1", id); err != nil {
			t.Fatalf("%s submission must keep its attachment: %v", status, err)
		}
	}
	if _, err := server.Uploads.Get("thread-1", cancelled); err == nil {
		t.Fatal("attachment of a cancelled submission past retention should be removed")
	}
	if _, err := server.Uploads.Get("thread-1", shared); err == nil {
		t.Fatal("attachment only referenced by another thread should be removed here")
	}
	if _, err := server.Uploads.Get("thread-2", shared); err != nil {
		t.Fatalf("attachment referenced by its own thread must survive: %v", err)
	}

	attach(t, server, "thread-1", SubCancelled)
	if removed := server.collectUploadsAt(time.Now()); removed != 0 {
		t.Fatalf("uploads inside the retention window must stay, removed = %d", removed)
	}
}

func TestServerUploadGCStopIsSynchronous(t *testing.T) {
	server := uploadGCServer(t, t.TempDir())
	server.StartUploadGC()
	if server.uploadGC == nil {
		t.Fatal("上传清理循环应已启动")
	}
	done := server.uploadGC.done
	server.StopUploadGC()
	select {
	case <-done:
	default:
		t.Fatal("StopUploadGC 返回时循环应已退出")
	}
	server.StopUploadGC()  // 幂等。
	server.StartUploadGC() // 可重启。
	server.StopUploadGC()

	bare := &Server{Uploads: uploads.New(t.TempDir())}
	bare.StartUploadGC()
	if bare.uploadGC != nil {
		t.Fatal("没有提交账本时不应启动清理循环")
	}
	if removed := bare.collectUploadsAt(time.Now()); removed != 0 {
		t.Fatalf("collectUploadsAt without a ledger removed %d", removed)
	}
}
