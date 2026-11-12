package team

import (
	"errors"
	"reflect"
	"testing"
)

type freshFingerprintStub struct {
	cwd, head, content string
	paths              []string
	err                error
}

func (*freshFingerprintStub) Fingerprint(string) (string, string, error) {
	panic("严格门禁不得调用缓存指纹")
}

func (f *freshFingerprintStub) FingerprintFresh(cwd string, paths []string) (string, string, error) {
	f.cwd, f.paths = cwd, append([]string(nil), paths...)
	return f.head, f.content, f.err
}

func TestFreshFingerprintRequiresCapability(t *testing.T) {
	for name, service := range map[string]*Service{
		"nil-service": nil,
		"no-adapter":  {},
		"legacy":      {Fingerprints: &fakeFingerprint{head: "head", diff: "cached"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := service.fingerprintFreshForPaths("/workspace", nil); !errors.Is(err, ErrFreshWorkspaceFingerprintUnavailable) {
				t.Fatalf("缺少即时核验能力必须失败，实际 %v", err)
			}
		})
	}
}

func TestFreshFingerprintUsesExactAdapterResult(t *testing.T) {
	stub := &freshFingerprintStub{head: "head", content: "content"}
	service := &Service{Fingerprints: stub}
	paths := []string{"src/a.go", "go.mod"}
	head, content, err := service.fingerprintFreshForPaths("/workspace", paths)
	if err != nil || head != stub.head || content != stub.content || stub.cwd != "/workspace" || !reflect.DeepEqual(stub.paths, paths) {
		t.Fatalf("即时指纹或作用域未传递：%s/%s %v %+v", head, content, err, stub)
	}
	cause := errors.New("仓库读取失败")
	stub.err = cause
	if head, content, err := service.fingerprintFreshForPaths("/workspace", nil); !errors.Is(err, cause) || head != "" || content != "" {
		t.Fatalf("读取失败不能携带可误用身份：%s/%s %v", head, content, err)
	}
	stub.err = nil
	for _, missing := range []string{"head", "content"} {
		stub.head, stub.content = "head", "content"
		if missing == "head" {
			stub.head = ""
		} else {
			stub.content = " "
		}
		if _, _, err := service.fingerprintFreshForPaths("/workspace", nil); err == nil {
			t.Fatalf("缺少 %s 时不能通过核验", missing)
		}
	}
}
