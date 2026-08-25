package inspect

import (
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/relayv2"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func BuildShowGraphReport(sessionDir string) (map[string]any, error) {
	if sess, found, err := relayv2.Open(sessionDir); err != nil {
		return nil, err
	} else if found {
		return relayv2.BuildGraphReport(sess)
	}
	st := store.New(sessionDir)
	repairedGraph, events, err := graph.RepairFromEvents(st)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"graph":      repairedGraph,
		"events":     events,
		"validation": StrictValidationResult(st),
	}
	if meta, loadErr := LoadMeta(sessionDir); loadErr == nil {
		if root := BuildRootInspectionReport(sessionDir, meta, false); root != nil {
			report["root"] = root
		}
	}
	return report, nil
}

func FormatGraphSummary(report map[string]any) string {
	graphData, _ := report["graph"].(map[string]any)
	summary := graph.Summary(graphData)
	if root, ok := report["root"].(map[string]any); ok {
		summary += "\n" + FormatRootSummary(root)
	}
	return summary
}
