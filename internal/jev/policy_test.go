package jev

import (
	"fmt"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func testPolicy() []Category {
	return []Category{
		{"danger", "negative", "dangerous work"},
		{"safe", "positive", "ordinary work"},
	}
}

func TestDefaultPolicyValid(t *testing.T) {
	negatives, positives := 0, 0
	names := map[string]bool{}
	for _, category := range defaultPolicy {
		if names[category.Name] {
			t.Fatalf("duplicate category name %q", category.Name)
		}
		names[category.Name] = true
		switch category.Polarity {
		case "negative":
			negatives++
		case "positive":
			positives++
		default:
			t.Fatalf("category %q has polarity %q", category.Name, category.Polarity)
		}
		if category.Description == "" {
			t.Fatalf("category %q has an empty description", category.Name)
		}
	}
	if negatives != 8 || positives != 4 {
		t.Fatalf("default policy = %d negative + %d positive, want 8+4", negatives, positives)
	}
	if err := validatePolicy(defaultPolicy, defaultThreshold); err != nil {
		t.Fatalf("default policy rejected: %v", err)
	}
	value := policyValue(defaultPolicy)
	for _, item := range value.Items {
		entry, ok := item.(*jsoncompat.Object)
		if !ok {
			t.Fatalf("policy entry is %T, want object", item)
		}
		for _, key := range entry.Keys() {
			field, _ := entry.Get(key)
			if _, isString := field.(string); !isString {
				t.Fatalf("policy field %s is %T, want string", key, field)
			}
		}
	}
	if len(jsoncompat.EncodeCompact(value)) == 0 {
		t.Fatal("policy value failed to encode")
	}
}

func TestValidatePolicyRejects(t *testing.T) {
	renamed := func(name string) []Category {
		policy := testPolicy()
		policy[0].Name = name
		return policy
	}
	crowded := []Category{{"safe", "positive", "ordinary work"}}
	for index := 0; index <= maxCategories; index++ {
		crowded = append(crowded, Category{fmt.Sprintf("danger_%02d", index), "negative", "dangerous work"})
	}
	cases := []struct {
		name      string
		policy    []Category
		threshold float64
		want      string
	}{
		{"uppercase name", renamed("Danger"), defaultThreshold, "策略类别 Danger 名称不合法"},
		{"leading digit", renamed("1danger"), defaultThreshold, "策略类别 1danger 名称不合法"},
		{"punctuation in name", renamed("danger!"), defaultThreshold, "策略类别 danger! 名称不合法"},
		{"duplicate name", []Category{{"danger", "negative", "d"}, {"danger", "positive", "p"}}, defaultThreshold, "策略类别名称不能重复"},
		{"missing polarity", []Category{{"danger", "", "d"}, {"safe", "positive", "p"}}, defaultThreshold, "策略类别 danger 缺少 polarity（negative/positive）"},
		{"unknown polarity", []Category{{"danger", "neutral", "d"}, {"safe", "positive", "p"}}, defaultThreshold, "策略类别 danger 缺少 polarity（negative/positive）"},
		{"empty description", []Category{{"danger", "negative", ""}, {"safe", "positive", "p"}}, defaultThreshold, "策略类别 danger 描述不能为空"},
		{"too many categories", crowded, defaultThreshold, "策略类别数量不能超过 24"},
		{"no positive", []Category{{"danger", "negative", "d"}, {"worse", "negative", "d"}}, defaultThreshold, "策略至少需要一个 positive 类别"},
		{"threshold zero", testPolicy(), 0, "阈值必须在 0 和 1 之间"},
		{"threshold one", testPolicy(), 1, "阈值必须在 0 和 1 之间"},
		{"threshold negative", testPolicy(), -0.1, "阈值必须在 0 和 1 之间"},
		{"threshold above one", testPolicy(), 1.5, "阈值必须在 0 和 1 之间"},
	}
	for _, item := range cases {
		err := validatePolicy(item.policy, item.threshold)
		if err == nil || err.Error() != item.want {
			t.Fatalf("%s: err=%v want=%q", item.name, err, item.want)
		}
	}
}

func TestParsePolicyValueToleratesJunk(t *testing.T) {
	encoded := `[{"name":"danger","polarity":"negative","description":"d"},{"name":"safe","polarity":"positive","description":"p"}]`
	value, err := jsoncompat.Unmarshal([]byte(encoded))
	if err != nil {
		t.Fatal(err)
	}
	policy, normalized, ok := parsePolicyValue(value)
	if !ok || len(policy) != 2 || policy[0].Name != "danger" || policy[1].Polarity != "positive" {
		t.Fatalf("valid policy did not parse: %+v ok=%v", policy, ok)
	}
	if normalized == nil || len(normalized.Items) != 2 {
		t.Fatalf("normalized array missing: %v", normalized)
	}
	numbered := jsoncompat.NewObject()
	numbered.Set("name", jsoncompat.Num(7))
	junk := []struct {
		name  string
		value any
	}{
		{"string", "not-an-array"},
		{"object", jsoncompat.NewObject()},
		{"nil", nil},
		{"string element", &jsoncompat.Array{Items: []any{"danger"}}},
		{"nameless element", &jsoncompat.Array{Items: []any{jsoncompat.NewObject()}}},
		{"numeric name", &jsoncompat.Array{Items: []any{numbered}}},
	}
	for _, item := range junk {
		if _, _, ok := parsePolicyValue(item.value); ok {
			t.Fatalf("%s parsed as policy", item.name)
		}
	}
}

func TestNumFloatEncodes(t *testing.T) {
	object := jsoncompat.NewObject()
	object.Set("t", numFloat(0.45))
	if got := string(jsoncompat.EncodeCompact(object)); got != `{"t":0.45}` {
		t.Fatalf("encoded = %s, want {\"t\":0.45}", got)
	}
}
