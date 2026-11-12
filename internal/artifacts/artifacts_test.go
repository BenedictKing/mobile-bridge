package artifacts

import (
	"os"
	"path/filepath"
	"strconv"
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

func viewWithUserAndAgentText(t *testing.T, userText, agentText string) *jsoncompat.Object {
	t.Helper()
	view := viewWithAgentText(t, agentText)
	message := jsoncompat.NewObject()
	message.Set("role", "user")
	message.Set("text", userText)
	turn := jsoncompat.NewObject()
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	turns := view.MustGet("turns").(*jsoncompat.Array)
	turns.Items = append([]any{turn}, turns.Items...)
	return view
}

func TestCollectUserReferencedProjects(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	paths := []string{
		filepath.Join(cwd, "dist", "mobile-bridge.apk"),
		filepath.Join(outside, "fridge-master", "android", "app-debug.apk"),
		filepath.Join(outside, "best-workout", "android", "app-debug.apk"),
		filepath.Join(outside, "fridge-master-private", "app-debug.apk"),
	}
	agentText := ""
	for _, path := range paths {
		writeFile(t, path, []byte("apk"))
		agentText += "[下载](" + path + ")\n"
	}
	if err := os.Truncate(paths[1], 80*1024*1024); err != nil {
		t.Fatal(err)
	}
	view := viewWithUserAndAgentText(t,
		filepath.Join(outside, "fridge-master")+" "+filepath.Join(outside, "best-workout")+" 类似这两个项目也修复这个问题",
		agentText)
	files := Collect(view, cwd)
	if len(files) != 3 {
		t.Fatalf("files=%+v, want three APKs", files)
	}
	for _, path := range paths[:3] {
		if entry, ok := files[ID(path)]; !ok || entry.Reference != path {
			t.Errorf("missing APK %s: %+v", path, entry)
		}
	}
	if _, ok := files[ID(paths[3])]; ok {
		t.Fatal("user directory reference granted a sibling with the same prefix")
	}
}

func TestCollectUserPathScopes(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	project := filepath.Join(outside, "other project")
	apk, sibling := filepath.Join(project, "app.apk"), filepath.Join(project, "notes.txt")
	partial := filepath.Join(outside, "other", "private.apk")
	writeFile(t, apk, []byte("apk"))
	writeFile(t, sibling, []byte("notes"))
	writeFile(t, partial, []byte("private"))
	agentText := "[下载](<" + apk + ">) [文档](<" + sibling + ">) [未授权](" + partial + ")"
	for _, test := range []struct {
		name, userText string
		want           int
	}{
		{"backticks", "处理 `" + project + "`", 2},
		{"double quotes", "处理 \"" + project + "\"", 2},
		{"single quotes", "处理 '" + project + "'", 2},
		{"markdown directory", "处理 [项目](<" + project + ">)", 2},
		{"file URL", "处理 `file://" + filepath.ToSlash(project) + "`", 2},
		{"exact file", "只处理 [APK](<" + apk + ">)", 1},
		{"relative path", "../" + filepath.Base(outside), 0},
		{"remote URL", "https://example.com" + filepath.ToSlash(apk), 0},
		{"assistant only", "生成下载链接", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := viewWithUserAndAgentText(t, test.userText, agentText)
			files := Collect(view, cwd)
			if len(files) != test.want {
				t.Fatalf("files=%+v, want %d", files, test.want)
			}
			if test.want == 1 {
				if _, ok := files[ID(apk)]; !ok {
					t.Fatal("explicit file reference did not grant the APK")
				}
			}
		})
	}
	view := viewWithUserAndAgentText(t, "`"+project+"`", agentText)
	turn := view.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	message := turn.MustGet("messages").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	message.Set("role", "tool")
	if files := Collect(view, cwd); len(files) != 0 {
		t.Fatalf("tool output granted artifact access: %+v", files)
	}
}

func TestCollectUserDirectorySymlinkEscape(t *testing.T) {
	cwd, project, outside := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "private.apk")
	writeFile(t, secret, []byte("private"))
	link := filepath.Join(project, "escape.apk")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	view := viewWithUserAndAgentText(t, project, "[下载]("+link+")")
	if files := Collect(view, cwd); len(files) != 0 {
		t.Fatalf("symlink escaped the user-referenced directory: %+v", files)
	}
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

func TestCollectCodexVisualizationDirective(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "chart.html")
	writeFile(t, path, []byte("<html>chart</html>"))
	directive := "\uE200visualize\uE202{\"path\":" + strconv.Quote(path) + ",\"mode\":\"wide\"}\uE201"
	view := viewWithAgentText(t, "前文\n"+directive+"\n后文")
	files := Collect(view, root)
	entry, ok := files[ID(path)]
	if !ok || entry.Reference != path || entry.Name != "chart.html" || entry.Image {
		t.Fatalf("visualization entry=%+v, files=%+v", entry, files)
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

func TestCollectAPKMetadataAndLimit(t *testing.T) {
	root := t.TempDir()
	apk := filepath.Join(root, "app-release.apk")
	writeFile(t, apk, nil)
	if err := os.Truncate(apk, 80*1024*1024); err != nil {
		t.Fatal(err)
	}
	view := viewWithAgentText(t, "[下载](app-release.apk)")
	files := Collect(view, root)
	entry, ok := files[ID(apk)]
	if !ok || entry.Size != 80*1024*1024 || entry.ModTime <= 0 {
		t.Fatalf("apk entry=%+v", entry)
	}
	payload := FilesPayload(files)
	first := payload.Items[0].(*jsoncompat.Object)
	if first.MustGet("size") != entry.Size || first.MustGet("modifiedAt") != entry.ModTime {
		t.Fatalf("apk payload=%v", first)
	}
	if FileLimit(apk) != MaxAPKFile || FileLimit(filepath.Join(root, "big.zip")) != MaxFile {
		t.Fatal("artifact limits are not extension-specific")
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
	if serve := ServeType("app.apk", nil); serve.Mime != "application/vnd.android.package-archive" {
		t.Fatalf("apk serve: %+v", serve)
	}
}
