package ai

// eco_fold_prior.go — constants bounding the prior roll-up quoted into a fold brief.

// priorRollupTailChars bounds the prior roll-up quoted into the brief. The
// brief is the tail of an already-large prompt, so an unbounded quote of a
// 100 KB chain would eat the room the history needs.
const priorRollupTailChars = 4000
