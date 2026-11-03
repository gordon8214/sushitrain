// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"testing"
	"time"
)

func TestPruning(t *testing.T) {
	g := writableGateway(t, map[string]string{"a.txt": "a", "dir/b.txt": "b", "doc.txt": "v1", "other.txt": "o1"})
	ts := newTestService(t, g, 0)
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := ts.store.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Removed on the gateway: their rows stay as tombstones
	g.Engine.Remove("a.txt")
	g.Engine.Remove("dir")
	ts.refresh()
	if n := count(`SELECT COUNT(*) FROM items WHERE deleted = 1`); n != 3 {
		t.Fatalf("%d tombstones, want 3", n)
	}
	// Two saves, both committed; and a save of another file whose next edit can't be sent, which builds on it
	for _, contents := range []string{"o2", "o3"} {
		ts.write(ts.mustLookup("docs/other.txt"), contents, true)
		time.Sleep(5 * testSettleDelay)
		ts.settled()
	}
	doc := ts.mustLookup("docs/doc.txt")
	ts.write(doc, "v2", true)
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	if _, err := ts.store.db.Exec(`INSERT INTO outbox (op_id, folder, item, kind, state, created_at, updated_at)
		VALUES ('stuck', ?, ?, 'replace', 'failed', 0, 0)`, ts.folder.ID, doc.Item); err != nil {
		t.Fatal(err)
	}

	// Too recent
	if err := ts.prune(time.Now().Add(-pruneAfter)); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM items WHERE deleted = 1`); n != 3 {
		t.Fatalf("%d tombstones left, want 3", n)
	}
	if n := count(`SELECT COUNT(*) FROM outbox`); n != 4 {
		t.Fatalf("%d operations left, want 4", n)
	}

	if err := ts.prune(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM items WHERE deleted = 1`); n != 0 {
		t.Fatalf("%d tombstones left", n)
	}
	if n := count(`SELECT COUNT(*) FROM remote_items WHERE deleted = 1`); n != 0 {
		t.Fatalf("%d of the gateway's removed items left", n)
	}
	if n := count(`SELECT COUNT(*) FROM outbox WHERE item = ?`, ts.mustLookup("docs/other.txt").Item); n != 0 {
		t.Fatalf("%d finished operations left", n)
	}
	if n := count(`SELECT COUNT(*) FROM outbox WHERE item = ?`, doc.Item); n != 2 {
		t.Fatalf("%d operations of the edit that can't be sent and what it builds on, want 2", n)
	}

	// The rest goes on as before
	if _, err := ts.store.db.Exec(`DELETE FROM outbox WHERE op_id = 'stuck'`); err != nil {
		t.Fatal(err)
	}
	ts.create("docs", "new.txt", "new")
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayHas("new.txt", "new")
	if got := ts.readPath("docs/other.txt"); got != "o3" {
		t.Fatalf("other.txt reads %q", got)
	}
	// A pruned file made again on the gateway shows again
	g.Engine.Write("a.txt", "again")
	ts.refresh()
	if got := ts.readPath("docs/a.txt"); got != "again" {
		t.Fatalf("a.txt reads %q", got)
	}
}
