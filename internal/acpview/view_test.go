package acpview

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func cardParams(title string, rawInput *jsoncompat.Object) *jsoncompat.Object {
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", title)
	toolCall.Set("rawInput", rawInput)
	params := jsoncompat.NewObject()
	params.Set("toolCall", toolCall)
	return params
}

func TestPermissionCardExtractsSubject(t *testing.T) {
	rawInput := jsoncompat.NewObject()
	rawInput.Set("command", "cargo test")
	title, command, question := PermissionCard(cardParams("Bash", rawInput))
	if title != "Bash" || command != "cargo test" || question {
		t.Fatalf("subject = %q %q %v", title, command, question)
	}

	// kimi bridges AskUserQuestion onto request_permission: a questions array
	// in rawInput flags the card even under a plain tool title.
	choice := jsoncompat.NewObject()
	choice.Set("question", "选 A 还是 B？")
	choice.Set("options", &jsoncompat.Array{Items: []any{optionLabel("A"), optionLabel("B")}})
	questionInput := jsoncompat.NewObject()
	questionInput.Set("questions", &jsoncompat.Array{Items: []any{choice}})
	_, _, question = PermissionCard(cardParams("Bash", questionInput))
	if !question {
		t.Fatal("questions rawInput should flag the card as a question")
	}

	if title, command, question = PermissionCard(jsoncompat.NewObject()); title != "权限请求" || command != "" || question {
		t.Fatalf("empty params subject = %q %q %v", title, command, question)
	}
	if title, command, question = PermissionCard(nil); title != "权限请求" || command != "" || question {
		t.Fatalf("nil params subject = %q %q %v", title, command, question)
	}
}
