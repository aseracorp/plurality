package search

import "sort"

const rrfK = 60

// RRFMerge combines multiple ranked result lists using Reciprocal Rank Fusion.
// Results are grouped by ConversationID, taking the best-scoring entry per conversation.
// Returns conversations sorted by descending RRF score.
func RRFMerge(lists ...[]ScoredResult) []ScoredResult {
	scores := make(map[string]float64)    // conversation_id -> total RRF score
	bestSource := make(map[string]string) // conversation_id -> best source_id

	for _, list := range lists {
		for rank, r := range list {
			cid := r.ConversationID
			rrfScore := 1.0 / float64(rrfK+rank+1)
			scores[cid] += rrfScore

			if _, exists := bestSource[cid]; !exists {
				bestSource[cid] = r.SourceID
			}
		}
	}

	results := make([]ScoredResult, 0, len(scores))
	for cid, score := range scores {
		results = append(results, ScoredResult{
			ConversationID: cid,
			SourceID:       bestSource[cid],
			Score:          score,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
