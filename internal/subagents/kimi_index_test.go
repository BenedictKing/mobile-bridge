package subagents

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestKimiIndexCacheInvalidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session_index.jsonl")
	store := NewKimiStore(root)
	if got := store.sessionDir("one"); got != "" {
		t.Fatal(got)
	}
	write(t, path, "{\"sessionId\":\"one\",\"sessionDir\":\"/a\"}\n")
	if got := store.sessionDir("one"); got != "/a" {
		t.Fatal(got)
	}
	cachedInfo := store.indexInfo
	var readers sync.WaitGroup
	for range 20 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if store.sessionDir("one") != "/a" || store.sessionDir("missing") != "" {
				t.Error("concurrent lookup returned wrong directory")
			}
		}()
	}
	readers.Wait()
	if store.indexInfo != cachedInfo {
		t.Fatal("unchanged index was parsed again")
	}
	// Atomic replacement must invalidate even with identical size and mtime.
	replacement := filepath.Join(root, "replacement.jsonl")
	write(t, replacement, "{\"sessionId\":\"one\",\"sessionDir\":\"/b\"}\n")
	if err := os.Chtimes(replacement, cachedInfo.ModTime(), cachedInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := store.sessionDir("one"); got != "/b" {
		t.Fatalf("replacement kept stale cache: %s", got)
	}
	// An append must also invalidate a cached negative lookup. Duplicates keep
	// the first valid directory, matching the previous per-request scanner.
	write(t, path, "{\"sessionId\":123,\"sessionDir\":\"/invalid\"}\n"+
		"{\"sessionId\":\"one\",\"sessionDir\":\"/b\"}\n"+
		"{\"sessionId\":\"missing\",\"sessionDir\":\"/c\"}\n"+
		"{\"sessionId\":\"one\",\"sessionDir\":\"/d\"}\n")
	if store.sessionDir("missing") != "/c" || store.sessionDir("one") != "/b" {
		t.Fatal("append or first-match semantics broken")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if store.sessionDir("one") != "" || store.indexDirs != nil {
		t.Fatal("deleted index retained cached entries")
	}
}

func TestKimiIndexDoesNotCachePartialAppend(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session_index.jsonl")
	store := NewKimiStore(root)
	write(t, path, "{\"sessionId\":\"one\",\"sessionDir\":\"/a\"}\n{\"sessionId\":")
	if store.sessionDir("one") != "/a" || store.indexInfo != nil {
		t.Fatal("partial append should return complete entries without caching")
	}
	write(t, path, "{\"sessionId\":\"one\",\"sessionDir\":\"/a\"}\n{\"sessionId\":\"two\",\"sessionDir\":\"/b\"}\n")
	if store.sessionDir("two") != "/b" || store.indexInfo == nil {
		t.Fatal("completed append was not discovered")
	}
}
