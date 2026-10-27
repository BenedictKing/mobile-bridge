package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Category is one class of the approval policy: Jev sorts each permission
// request into these categories and reports a probability per class. Negative
// categories lean toward blocking, positive ones toward allowing; only the
// negative probabilities enter the blocking sum.
type Category struct {
	Name        string
	Polarity    string
	Description string
}

const (
	defaultThreshold = 0.3
	maxCategories    = 24
)

// categoryNamePattern keeps names valid as wire keys (the choice criteria and
// the probabilities map the remote model answers with).
var categoryNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// defaultPolicy is the built-in 8-negative/4-positive policy. Descriptions
// are tuned for the ACP permission domain — bash command lines, file writes,
// configuration changes and external services — not just shell work.
var defaultPolicy = []Category{
	{"irreversible_destruction", "negative", "Destroys data or state that cannot be restored afterwards: rm -rf outside scratch directories, dropping or truncating databases, force-pushing or rewriting shared git history, deleting backups or logs."},
	{"secret_exposure", "negative", "Reveals secrets to the user, a log, or an outside service: printing or sending credentials, API keys, tokens, private keys, .env contents, or password-manager entries."},
	{"security_weakened", "negative", "Weakens the machine's or repository's security posture: disabling protections, adding trust or SSH keys, opening ports, chmod 777, editing sudoers, firewall or authentication rules."},
	{"code_from_elsewhere", "negative", "Downloads and runs code or content from outside the project: curl|sh style pipes, installs from unvetted sources, executing fetched scripts, or evaluating remote data as code."},
	{"privilege_escalation", "negative", "Gains or uses privileges beyond what the task needs: sudo, su, administrator shells, container escapes, changing file ownership or capabilities."},
	{"service_disruption", "negative", "Stops, restarts or reconfigures running services or shared infrastructure: killing system processes, systemctl restart, redeploying, or mutating a database other services depend on."},
	{"write_outside_allowed", "negative", "Writes outside the workspace or requested scope: user dotfiles, system configuration, /etc or /usr, other projects, or paths the user never mentioned."},
	{"scratch_misplaced", "negative", "Leaves temporary or scratch artifacts in the wrong place: dumps in the repository root, temp files in shared directories, or backups that clutter and get committed accidentally."},
	{"routine_work", "positive", "Ordinary development work: reading, listing and searching files, running tests and builds, editing project files, git status or diff, installing project-local dependencies."},
	{"permission_tightened", "positive", "The operation reduces risk: revoking access, removing stale keys or credentials, tightening permissions, firewall rules, or cleaning up dangerous state."},
	{"credential_handled_safely", "positive", "Touches secrets only to store or rotate them safely where the user asked: writing a provided key into its proper config, without displaying it or sending it anywhere external."},
	{"scoped_operation", "positive", "A narrowly scoped action with bounded impact: creating a scratch directory, touching a single named file, previewing a diff, or anything limited to explicitly requested paths."},
}

// numFloat is the only way float64 values enter jsoncompat trees:
// writeValue has no float64 branch and panics on one, so every derived
// numeric value is rendered to its shortest exact lexeme first.
func numFloat(v float64) json.Number {
	return json.Number(strconv.FormatFloat(v, 'f', -1, 64))
}

// policyValue renders categories as the JSON shape stored in jev.json,
// served through Public, and hashed into fingerprints: an array of objects
// whose fields are all strings.
func policyValue(policy []Category) *jsoncompat.Array {
	value := &jsoncompat.Array{}
	for _, category := range policy {
		entry := jsoncompat.NewObject()
		entry.Set("name", category.Name)
		entry.Set("polarity", category.Polarity)
		entry.Set("description", category.Description)
		value.Items = append(value.Items, entry)
	}
	return value
}

// parsePolicyValue decodes a stored or submitted policy value back into
// categories and also returns the normalized array form. ok is false when
// the value is not an array of objects whose name is a non-empty string;
// deeper rule violations are left to validatePolicy.
func parsePolicyValue(value any) ([]Category, *jsoncompat.Array, bool) {
	list, _ := value.(*jsoncompat.Array)
	if list == nil {
		return nil, nil, false
	}
	policy := []Category{}
	for _, item := range list.Items {
		entry, _ := item.(*jsoncompat.Object)
		if entry == nil {
			return nil, nil, false
		}
		name, _ := entry.MustGet("name").(string)
		if name == "" {
			return nil, nil, false
		}
		polarity, _ := entry.MustGet("polarity").(string)
		description, _ := entry.MustGet("description").(string)
		policy = append(policy, Category{Name: name, Polarity: polarity, Description: description})
	}
	return policy, policyValue(policy), true
}

// validatePolicy enforces the persisted policy contract: snake_case unique
// names, an explicit polarity, non-empty descriptions, at most 24 categories,
// at least one positive category, and a threshold strictly between 0 and 1.
func validatePolicy(policy []Category, threshold float64) error {
	seen := map[string]bool{}
	positive := 0
	for _, category := range policy {
		if !categoryNamePattern.MatchString(category.Name) {
			return fmt.Errorf("策略类别 %s 名称不合法", category.Name)
		}
		if seen[category.Name] {
			return errors.New("策略类别名称不能重复")
		}
		seen[category.Name] = true
		if category.Polarity != "negative" && category.Polarity != "positive" {
			return fmt.Errorf("策略类别 %s 缺少 polarity（negative/positive）", category.Name)
		}
		if category.Description == "" {
			return fmt.Errorf("策略类别 %s 描述不能为空", category.Name)
		}
		if category.Polarity == "positive" {
			positive++
		}
	}
	if len(policy) > maxCategories {
		return fmt.Errorf("策略类别数量不能超过 %d", maxCategories)
	}
	if positive == 0 {
		return errors.New("策略至少需要一个 positive 类别")
	}
	if !(threshold > 0 && threshold < 1) {
		return errors.New("阈值必须在 0 和 1 之间")
	}
	return nil
}

// negativeNames indexes which categories count toward the blocking sum.
func negativeNames(policy []Category) map[string]bool {
	names := map[string]bool{}
	for _, category := range policy {
		if category.Polarity == "negative" {
			names[category.Name] = true
		}
	}
	return names
}

// validThreshold reports whether a threshold is usable (open interval).
func validThreshold(threshold float64) bool {
	return !math.IsNaN(threshold) && threshold > 0 && threshold < 1
}
