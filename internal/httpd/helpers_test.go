package httpd

import (
	"io/fs"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type fsys struct{}

func (fsys) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func jsonObject(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}
