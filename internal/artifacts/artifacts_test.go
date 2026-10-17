package artifacts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func parse(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		t.Fatal("not an object")
	}
	return object
}

func TestReferencePath(t *testing.T) {
	cases := []struct {
		value, cwd, want string
	}{
		{"/tmp/a b.png", "", "/tmp/a b.png"},
		{"file:///tmp/shot.png", "", "/tmp/shot.png"},
		{"file://localhost/tmp/x.png", "", "/tmp/x.png"},
		{"file://evil/tmp/x.png", "", ""},
		{"https://example.com/a.png", "", ""},
		{"/tmp/shot.png:12", "", "/tmp/shot.png"},
		{"poster.png", "/work", "/work/poster.png"},
		{"../out.png", "/work/sub", "/work/out.png"},
		{"", "/work", ""},
	}
	for _, item := range cases {
		if got := ReferencePath(item.value, item.cwd); got != item.want {
			t.Errorf("ReferencePath(%q, %q)=%q want %q", item.value, item.cwd, got, item.want)
		}
	}
}

func viewWithAgentText(t *testing.T, text string, attachments ...*jsoncompat.Object) *jsoncompat.Object {
	t.Helper()
	view := jsoncompat.NewObject()
	view.Set("cwd", "")
	turn := jsoncompat.NewObject()
	turn.Set("id", "t1")
	message := jsoncompat.NewObject()
	message.Set("id", "m1")
	message.Set("kind", "agentMessage")
	message.Set("role", "assistant")
	message.Set("text", text)
	if len(attachments) > 0 {
		list := &jsoncompat.Array{}
		for _, attachment := range attachments {
			list.Items = append(list.Items, attachment)
		}
		message.Set("attachments", list)
	}
	messages := &jsoncompat.Array{Items: []any{message}}
	turn.Set("messages", messages)
	turns := &jsoncompat.Array{Items: []any{turn}}
	view.Set("turns", turns)
	return view
}

func imageAttachment(t *testing.T, path string) *jsoncompat.Object {
	t.Helper()
	attachment := jsoncompat.NewObject()
	attachment.Set("type", "localImage")
	attachment.Set("path", path)
	return attachment
}

func TestCollectScopesAndTrust(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "shot.png")
	html := filepath.Join(root, "viz.html")
	escaped := filepath.Join(outside, "escape.png")
	route := filepath.Join(outside, "route.png")
	writeFile(t, inside, []byte("\x89PNG\r\n\x1a\npayload"))
	writeFile(t, html, []byte("<html>ok</html>"))
	writeFile(t, escaped, []byte("\x89PNG\r\n\x1a\noutside"))
	writeFile(t, route, []byte("\x89PNG\r\n\x1a\nroute"))

	// Markdown references are workspace-bound; the attachment is runtime
	// evidence and may live outside the roots.
	view := viewWithAgentText(t,
		"看图 ![图](shot.png) [预览](viz.html) ![逃逸](../"+filepath.Base(outside)+"/escape.png)",
		imageAttachment(t, route))
	view.Set("cwd", root)

	files := Collect(view, root)
	if len(files) != 3 {
		t.Fatalf("collected %d files, want 3: %+v", len(files), files)
	}
	htmlEntry, ok := files[ID(html)]
	if !ok || htmlEntry.Reference != "viz.html" || htmlEntry.Image {
		t.Fatalf("html entry wrong: %+v", htmlEntry)
	}
	if _, ok := files[ID(escaped)]; ok {
		t.Fatal("markdown reference escaped the workspace roots")
	}
	shot, ok := files[ID(inside)]
	if !ok || !shot.Image || shot.Name != "shot.png" {
		t.Fatalf("shot entry wrong: %+v", shot)
	}
	if _, ok := files[ID(route)]; !ok {
		t.Fatal("trusted attachment outside roots dropped")
	}
}

func TestCollectRelativeAndFileURLReferences(t *testing.T) {
	root := t.TempDir()
	poster := filepath.Join(root, "sub", "海报.png")
	writeFile(t, poster, []byte("\x89PNG\r\n\x1a\nx"))
	view := viewWithAgentText(t, "相对 ![海报](sub/海报.png) 与 file:// ![直连](file://"+filepath.ToSlash(poster)+")")
	files := Collect(view, root)
	if len(files) != 1 {
		t.Fatalf("files=%d want 1: %+v", len(files), files)
	}
	entry, _ := files[ID(poster)]
	if entry.Reference != "sub/海报.png" || !entry.Image {
		t.Fatalf("entry kept first reference: %+v", entry)
	}
}

func TestCollectSizeAndRegularFileGuards(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.png")
	writeFile(t, big, make([]byte, MaxFile+1))
	dir := filepath.Join(root, "dir.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	view := viewWithAgentText(t, "![大](big.png) ![目录](dir.png)")
	if files := Collect(view, root); len(files) != 0 {
		t.Fatalf("guards failed: %+v", files)
	}
}

func TestFilesPayloadAndAttachmentPaths(t *testing.T) {
	root := t.TempDir()
	shot := filepath.Join(root, "shot.png")
	writeFile(t, shot, []byte("\x89PNG\r\n\x1a\nx"))
	view := viewWithAgentText(t, "![图](shot.png)", imageAttachment(t, shot))
	view.Set("cwd", root)
	files := Collect(view, root)
	payload := FilesPayload(files)
	if len(payload.Items) != 1 {
		t.Fatalf("payload len=%d", len(payload.Items))
	}
	first, _ := payload.Items[0].(*jsoncompat.Object)
	if first.MustGet("id") != ID(shot) || first.MustGet("image") != true {
		t.Fatalf("payload entry=%v", first)
	}
	paths := AttachmentPaths(view)
	if len(paths) != 1 || Identity(paths[0]) != Identity(shot) {
		t.Fatalf("attachment paths=%v", paths)
	}
}

func TestServeType(t *testing.T) {
	if serve := ServeType("a.png", []byte("\x89PNG\r\n\x1a\nx")); serve.Mime != "image/png" || serve.Sandbox != "" {
		t.Fatalf("png sniff: %+v", serve)
	}
	if serve := ServeType("viz.html", []byte("<html>")); serve.Mime != "text/html; charset=utf-8" ||
		serve.Sandbox != "sandbox allow-scripts; frame-ancestors 'self'" {
		t.Fatalf("html serve: %+v", serve)
	}
	if serve := ServeType("pic.svg", []byte("<svg/>")); serve.Sandbox != "sandbox; frame-ancestors 'self'" {
		t.Fatalf("svg sandbox: %+v", serve)
	}
	if serve := ServeType("blob", []byte("junk")); serve.Mime != "application/octet-stream" {
		t.Fatalf("fallback: %+v", serve)
	}
}
