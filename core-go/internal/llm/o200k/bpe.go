package o200k

import (
	"container/heap"
	"math"
)

const none = math.MaxInt

// pieceTokens is how many tokens byte-pair encoding leaves of one piece: it keeps merging the
// adjacent pair with the lowest rank, leftmost first, until no pair is a token. A heap keeps this
// fast on long pieces, where scanning every pair after each merge would take seconds.
func (e *encoding) pieceTokens(piece string) int {
	if _, ok := e.ranks[piece]; ok {
		return 1
	}
	_, parts := e.merge(piece)
	return parts
}

// merge returns, for each byte that starts a part, where the next part starts, and how many
// parts there are.
func (e *encoding) merge(piece string) (next []int, parts int) {
	n := len(piece)
	next = make([]int, n)
	prev := make([]int, n)
	version := make([]int, n)
	for i := range n {
		next[i], prev[i] = i+1, i-1
	}
	rank := func(i int) int {
		j := next[i]
		if j >= n {
			return none
		}
		if r, ok := e.ranks[piece[i:next[j]]]; ok {
			return r
		}
		return none
	}
	q := make(pairs, 0, n)
	for i := range n - 1 {
		if r := rank(i); r != none {
			q = append(q, pair{rank: r, start: i})
		}
	}
	heap.Init(&q)
	parts = n
	for q.Len() > 0 {
		p := heap.Pop(&q).(pair)
		if version[p.start] < 0 || p.version != version[p.start] {
			continue
		}
		gone := next[p.start]
		next[p.start] = next[gone]
		if next[gone] < n {
			prev[next[gone]] = p.start
		}
		version[gone] = -1
		parts--
		version[p.start]++
		if r := rank(p.start); r != none {
			heap.Push(&q, pair{rank: r, start: p.start, version: version[p.start]})
		}
		if before := prev[p.start]; before >= 0 {
			version[before]++
			if r := rank(before); r != none {
				heap.Push(&q, pair{rank: r, start: before, version: version[before]})
			}
		}
	}
	return next, parts
}

// pair is a possible merge of the part starting at start with the one after it. version tells a
// stale entry from a current one once either part has changed.
type pair struct{ rank, start, version int }

type pairs []pair

func (q pairs) Len() int { return len(q) }
func (q pairs) Less(i, j int) bool {
	if q[i].rank != q[j].rank {
		return q[i].rank < q[j].rank
	}
	return q[i].start < q[j].start
}
func (q pairs) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *pairs) Push(x any)   { *q = append(*q, x.(pair)) }
func (q *pairs) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}
