package jev

import (
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Annotation is the phone-facing record of one Jev verdict on a pending
// card: Assessment feeds the risk badge and RecommendOption the starred
// button. A nil Annotation means Jev never assessed the card (switch off, no
// key, unsupported method, failed call), so the phone keeps its local label.
type Annotation struct {
	Assessment      *jsoncompat.Object
	RecommendOption string
}

// Annotate renders a decision for the pending card. Only verdicts that came
// back from Jev (fresh or cached) produce an annotation.
func Annotate(method string, params *jsoncompat.Object, d Decision) *Annotation {
	if !d.Assessed {
		return nil
	}
	assessment := jsoncompat.NewObject()
	assessment.Set("source", "jev")
	assessment.Set("riskLabel", riskLabel(d.NegativeSum, d.Threshold))
	assessment.Set("negativeSum", numFloat(d.NegativeSum))
	assessment.Set("threshold", numFloat(d.Threshold))
	assessment.Set("reversible", numFloat(d.Reversible))
	assessment.Set("confidence", numFloat(d.Confidence))
	assessment.Set("recommend", d.Recommend)
	assessment.Set("reason", d.Reason)
	assessment.Set("approved", d.Approved)
	return &Annotation{Assessment: assessment, RecommendOption: recommendedOption(method, params, d.Recommend)}
}

// riskLabel buckets the negative-category probability mass against the
// policy threshold into the four labels the phone renders: under a third of
// the threshold is routine, under the threshold low, under twice it elevated.
func riskLabel(negativeSum, threshold float64) string {
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	switch {
	case negativeSum < threshold/3:
		return "routine"
	case negativeSum < threshold:
		return "low"
	case negativeSum < threshold*2:
		return "elevated"
	}
	return "high"
}

// recommendedOption names the card button matching Jev's recommendation: the
// ACP optionId for permission cards, the decision keyword otherwise. "review"
// and recommendations the card cannot express star nothing.
func recommendedOption(method string, params *jsoncompat.Object, recommend string) string {
	if method != "session/request_permission" && method != "session/requestPermission" {
		switch recommend {
		case "accept_once", "accept_session":
			return "accept"
		case "decline":
			return "decline"
		}
		return ""
	}
	for _, value := range arrayValues(params.MustGet("options")) {
		option, _ := value.(*jsoncompat.Object)
		if option == nil {
			continue
		}
		kind := strings.ToLower(jsoncompat.PyStr(option.MustGet("kind")))
		allow := strings.Contains(kind, "allow") || strings.Contains(kind, "accept")
		reject := strings.Contains(kind, "reject") || strings.Contains(kind, "deny") || strings.Contains(kind, "decline")
		always := strings.Contains(kind, "always")
		matched := false
		switch recommend {
		case "accept_once":
			matched = allow && !always
		case "accept_session":
			matched = allow && strings.Contains(kind, "session")
		case "accept_always":
			matched = allow && always
		case "decline":
			matched = reject
		}
		if !matched {
			continue
		}
		optionID := option.MustGet("optionId")
		if optionID == nil {
			optionID = option.MustGet("id")
		}
		return jsoncompat.PyStr(optionID)
	}
	return ""
}
