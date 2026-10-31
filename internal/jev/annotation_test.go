package jev

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func acpOptions(kinds ...string) *jsoncompat.Object {
	params := jsoncompat.NewObject()
	options := &jsoncompat.Array{}
	for _, kind := range kinds {
		option := jsoncompat.NewObject()
		option.Set("optionId", "opt-"+kind)
		option.Set("kind", kind)
		options.Items = append(options.Items, option)
	}
	params.Set("options", options)
	return params
}

func TestAnnotateSkipsUnassessedDecisions(t *testing.T) {
	for _, reason := range []string{"auto_approve_off", "no_api_key", "unsupported_method", "assess_failed"} {
		if got := Annotate("session/request_permission", acpOptions("allow_once"), Decision{Reason: reason}); got != nil {
			t.Fatalf("%s: unassessed decision must not annotate, got %+v", reason, got)
		}
	}
}

func TestAnnotateCarriesVerdictAndRecommendedOption(t *testing.T) {
	params := acpOptions("allow_always", "allow_once", "reject_once")
	got := Annotate("session/request_permission", params, Decision{
		Assessed: true, Reason: "low_confidence", NegativeSum: 0.02, Threshold: 0.3,
		Reversible: 0.96, Confidence: 0.59, Recommend: "accept_once",
	})
	if got == nil {
		t.Fatal("assessed decision should annotate")
	}
	if got.RecommendOption != "opt-allow_once" {
		t.Fatalf("accept_once should star allow_once, got %q", got.RecommendOption)
	}
	a := got.Assessment
	if a.MustGet("source") != "jev" || a.MustGet("riskLabel") != "routine" || a.MustGet("reason") != "low_confidence" {
		t.Fatalf("assessment fields wrong: %s", jsoncompat.EncodeCompact(a))
	}
	if number(a.MustGet("confidence"), -1) != 0.59 || number(a.MustGet("reversible"), -1) != 0.96 {
		t.Fatalf("numbers wrong: %s", jsoncompat.EncodeCompact(a))
	}
}

func TestRecommendedOptionMapping(t *testing.T) {
	params := acpOptions("allow_always", "allow_once", "reject_once")
	cases := map[string]string{"accept_once": "opt-allow_once", "accept_always": "opt-allow_always", "decline": "opt-reject_once", "review": "", "accept_session": ""}
	for recommend, want := range cases {
		if got := recommendedOption("session/request_permission", params, recommend); got != want {
			t.Fatalf("%s: want %q, got %q", recommend, want, got)
		}
	}
	if got := recommendedOption("item/commandExecution/requestApproval", nil, "accept_once"); got != "accept" {
		t.Fatalf("codex accept_once should star accept, got %q", got)
	}
}

func TestRiskLabelBuckets(t *testing.T) {
	cases := []struct {
		sum  float64
		want string
	}{{0, "routine"}, {0.09, "routine"}, {0.1, "low"}, {0.29, "low"}, {0.3, "elevated"}, {0.41, "elevated"}, {0.6, "high"}, {0.95, "high"}}
	for _, c := range cases {
		if got := riskLabel(c.sum, 0.3); got != c.want {
			t.Fatalf("sum %.2f: want %s, got %s", c.sum, c.want, got)
		}
	}
}
