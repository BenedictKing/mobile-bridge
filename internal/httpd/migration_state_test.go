package httpd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestMigrationStateSurvivesReloadAndUncertainDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creations.json")
	ledger := mustLoadCreations(t, path)
	_, _, err := ledger.begin("migration-1", &creationEntry{
		Agent: "codex", ID: "target", Message: "private transcript",
		State: "waiting_for_target", Warning: "desktop unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Creations: mustLoadCreations(t, path)}
	meta := server.migrationMeta("target").(*jsoncompat.Object)
	if meta.MustGet("state") != "waiting_for_target" || meta.MustGet("warning") != "desktop unavailable" {
		t.Fatalf("pending migration lost: %s", jsoncompat.EncodeCompact(meta))
	}
	if strings.Contains(string(jsoncompat.EncodeCompact(meta)), "private transcript") {
		t.Fatal("status metadata exposed the transcript")
	}
	if claimed, err := server.Creations.beginDispatch("migration-1"); err != nil || !claimed {
		t.Fatalf("dispatch claim: claimed=%v err=%v", claimed, err)
	}
	server.Creations = mustLoadCreations(t, path)
	if server.migrationMeta("target").(*jsoncompat.Object).MustGet("state") != "dispatching" {
		t.Fatal("uncertain dispatch must stay visible")
	}
	if _, pending := server.Creations.pendingForTarget("target"); pending != nil {
		t.Fatal("uncertain dispatch must not be retried")
	}
	if err := server.Creations.markSent("migration-1"); err != nil {
		t.Fatal(err)
	}
	if server.migrationMeta("target") != nil || server.migrationMeta("unknown") != nil {
		t.Fatal("completed and absent migrations must clear the notice")
	}
}

func TestSourceMaintenanceSurvivesReloadAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creations.json")
	ledger := mustLoadCreations(t, path)
	_, _, err := ledger.begin("migration-1", &creationEntry{ID: "target", At: 123})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.setSourceNotice("migration-1", "source", "网关压缩已完成：后续请在新会话继续。", "compact"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.markSent("migration-1"); err != nil {
		t.Fatal(err)
	}
	server := &Server{Creations: mustLoadCreations(t, path)}
	view := jsoncompat.NewObject()
	server.annotateSourceMaintenance("source", view)
	server.annotateSourceMaintenance("source", view)
	turns := view.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 || view.MustGet("sourceMaintenance") != nil {
		t.Fatalf("source notice must be one internal turn: %s", jsoncompat.EncodeCompact(view))
	}
	turn := turns.Items[0].(*jsoncompat.Object)
	message := turn.MustGet("messages").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	if message.MustGet("kind") != "gatewayCompaction" || message.MustGet("text") != "网关压缩已完成：后续请在新会话继续。" {
		t.Fatalf("wrong source notice: %s", jsoncompat.EncodeCompact(message))
	}
	other := jsoncompat.NewObject()
	server.annotateSourceMaintenance("other", other)
	if other.MustGet("turns") != nil {
		t.Fatal("source notice leaked to another chat")
	}
}

func TestTimelineIncludesPendingMigrationWithoutLiveConnection(t *testing.T) {
	ledger := mustLoadCreations(t, "")
	_, _, err := ledger.begin("migration-1", &creationEntry{
		ID: "target", Message: "private transcript", State: "waiting_for_target",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(nil, fsys{}, 8790, "instance")
	server.Creations = ledger
	view := jsoncompat.NewObject()
	view.Set("cwd", t.TempDir())
	page := jsoncompat.NewObject()
	page.Set("meta", jsoncompat.NewObject())
	result := server.enrichTimeline("target", view, page)
	meta := result.MustGet("meta").(*jsoncompat.Object)
	if meta.MustGet("migration").(*jsoncompat.Object).MustGet("state") != "waiting_for_target" {
		t.Fatal("saved history must carry the migration status")
	}
}
