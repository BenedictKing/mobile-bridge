// Package service: Bridge's timeline_read core — the annotated read path
// with stub-adapter empty behaviors (subagentRevision injection, goal overlay
// pass-through) and meta enrichment. Parity: conformance/timelineread goldens.
package service

import (
	"sort"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Bridge mirrors the Bridge fields this slice touches. The stub adapter's
// empty behaviors are baked in: no subagents, no goal runtime, home nil.
type Bridge struct {
	Host         string
	Agent        string
	Capabilities []string
	Live         map[string]*LiveSession
}

func NewBridge(host, agent string, capabilities []string) *Bridge {
	return &Bridge{Host: host, Agent: agent, Capabilities: capabilities,
		Live: map[string]*LiveSession{}}
}

func (b *Bridge) Session(threadID string) *LiveSession {
	return b.Live[threadID]
}

// TimelineRead mirrors Bridge.timeline_read for page/changes/detail. The
// saved-history edge expansion needs the store and stays unexercised here
// (scenarios never page past a loaded incomplete edge).
func (b *Bridge) TimelineRead(mode string, session *LiveSession, options ReadOptions) (*jsoncompat.Object, error) {
	// _add_subagents (empty): the revision marker is injected unconditionally
	// and feeds the projection update trigger.
	view := session.View()
	view.Set("subagentRevision", jsoncompat.Num(0))
	// _overlay_native_goal / _annotate_assessments (empty): view unchanged.

	projection := session.Timeline
	stale := projection.State().Sequence == jsoncompat.Num(session.Sequence).String() &&
		valueEqual(projection.Meta().MustGet("goal"), view.MustGet("goal")) &&
		valueEqual(projection.Meta().MustGet("subagentRevision"), view.MustGet("subagentRevision"))
	if !stale {
		projection.Update(view)
	}

	var result *jsoncompat.Object
	var err error
	switch mode {
	case "detail":
		version := any(nil)
		if options.Version != nil {
			version = *options.Version
		}
		result, err = projection.Detail(options.Cursor, options.Offset, version)
		if err != nil {
			return nil, err
		}
	case "changes":
		result = projection.Changes(options.After, options.Epoch, options.Start)
	default:
		result = projection.Page(options.Limit, options.Before)
	}
	if mode != "detail" {
		enriched := jsoncompat.NewObject()
		for _, key := range result.MustGet("meta").(*jsoncompat.Object).Keys() {
			value, _ := result.MustGet("meta").(*jsoncompat.Object).Get(key)
			enriched.Set(key, value)
		}
		enriched.Set("host", b.Host)
		enriched.Set("agent", b.Agent)
		hostLabel := "此电脑"
		if b.Host != "local" {
			hostLabel = b.Host
		}
		enriched.Set("hostLabel", hostLabel)
		capabilities := append([]string{}, b.Capabilities...)
		sort.Strings(capabilities)
		enriched.Set("capabilities", &jsoncompat.Array{Items: stringsToArray(capabilities)})
		enriched.Set("goalRuntimeAvailable", false)
		// _submission_meta (empty ledger): submissions + goalSubmission.
		enriched.Set("submissions", &jsoncompat.Array{})
		enriched.Set("goalSubmission", nil)
		// _fork_origin (empty message actions).
		enriched.Set("forkedFrom", nil)
		result.Set("meta", enriched)
	}
	// artifact_paths skipped: the stub adapter has no home.
	result.Set("files", &jsoncompat.Array{})
	return result, nil
}

func stringsToArray(values []string) []any {
	items := make([]any, len(values))
	for i, value := range values {
		items[i] = value
	}
	return items
}

// ReadOptions carries timeline_read's keyword surface.
type ReadOptions struct {
	Limit   int
	Before  string
	After   string
	Epoch   string
	Start   string
	Cursor  string
	Offset  int
	Version *string
}
