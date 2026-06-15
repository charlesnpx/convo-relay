package inspect

import (
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func BuildShowGraphReport(sessionDir string) (map[string]any, error) {
	st := store.New(sessionDir)
	repairedGraph, events, err := graph.RepairFromEvents(st)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"graph":      repairedGraph,
		"events":     events,
		"validation": StrictValidationResult(st),
	}, nil
}

func FormatGraphSummary(report map[string]any) string {
	graphData, _ := report["graph"].(map[string]any)
	return graph.Summary(graphData)
}
