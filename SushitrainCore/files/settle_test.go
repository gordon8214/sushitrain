// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// orderOpsByScanning is the ordering orderOps replaced, which it must match: each round, the first ready operation by
// category, depth and listing; with none ready, the first of all that are left
func orderOpsByScanning(ops []*plannedOp) []*plannedOp {
	less := func(a, b *plannedOp) bool {
		if a.order != b.order {
			return a.order < b.order
		}
		if a.order == orderDirDelete || a.order == orderFileDelete {
			return a.c.depth > b.c.depth
		}
		return a.c.depth < b.c.depth
	}
	emitted := map[*plannedOp]bool{}
	var result []*plannedOp
	for len(result) < len(ops) {
		var next *plannedOp
		for _, op := range ops {
			if emitted[op] {
				continue
			}
			ready := !slices.ContainsFunc(op.after, func(a *plannedOp) bool { return !emitted[a] })
			if ready && (next == nil || less(op, next)) {
				next = op
			}
		}
		if next == nil {
			for _, op := range ops {
				if !emitted[op] && (next == nil || less(op, next)) {
					next = op
				}
			}
		}
		emitted[next] = true
		result = append(result, next)
	}
	return result
}

func TestOrderOpsMatchesScanning(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for round := range 500 {
		n := 1 + r.IntN(40)
		ops := make([]*plannedOp, n)
		for i := range ops {
			ops[i] = &plannedOp{c: &candidate{depth: r.IntN(4)}, order: r.IntN(orderDirDelete + 1)}
		}
		for _, op := range ops {
			// Mostly what came before, as the planner's needs are; now and then a cycle, a repeat or itself
			for range r.IntN(3) {
				op.after = append(op.after, ops[r.IntN(n)])
			}
		}
		want, got := orderOpsByScanning(ops), orderOps(ops)
		if !slices.Equal(want, got) {
			t.Fatalf("round %d: the order differs", round)
		}
	}
}

// A tar x of a large tree: tens of thousands of operations, a directory's files after it
func TestOrderOpsScales(t *testing.T) {
	var ops, dirs []*plannedOp
	for i := range 100_000 {
		if i%50 == 0 {
			d := &plannedOp{c: &candidate{depth: len(dirs) % 8}, order: orderDirCreate}
			if len(dirs) > 0 {
				d.after = []*plannedOp{dirs[len(dirs)-1]}
			}
			dirs = append(dirs, d)
			ops = append(ops, d)
			continue
		}
		ops = append(ops, &plannedOp{c: &candidate{depth: 9}, order: orderFileCreate,
			after: []*plannedOp{dirs[len(dirs)-1]}})
	}
	start := time.Now()
	ordered := orderOps(ops)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("ordering %d operations took %v", len(ops), took)
	}
	if len(ordered) != len(ops) || ordered[0] != dirs[0] {
		t.Fatal("not every operation was ordered, or the first directory isn't first")
	}
}
