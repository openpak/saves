package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// PruneOverflow trims saves written before Keep shrank, leaves pinned versions and conflicts
// alone, and never touches the newest Keep.
func TestPruneOverflowKeepsNewestAndSparesPinnedAndConflicts(t *testing.T) {
	url := os.Getenv("SAVES_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SAVES_TEST_DATABASE_URL not set")
	}
	st, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	account := fmt.Sprintf("prune-%d", time.Now().UnixNano())
	var saveID int64
	if err := st.db.QueryRowContext(ctx, `INSERT INTO saves (account_id, platform, title_id) VALUES ($1,'switch','0100000000000000') RETURNING id`, account).Scan(&saveID); err != nil {
		t.Fatal(err)
	}
	defer st.db.ExecContext(ctx, `DELETE FROM saves WHERE id=$1`, saveID)
	// versions 1..6 live; 2 is pinned, 3 is a conflict
	for n := 1; n <= 6; n++ {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO versions (save_id, number, status, conflict, pinned, backend) VALUES ($1,$2,'live',$3,$4,'local')`,
			saveID, n, n == 3, n == 2); err != nil {
			t.Fatal(err)
		}
	}
	// three island-image versions (another kind of backup in the same save row): only the oldest goes
	for n := 7; n <= 9; n++ {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO versions (save_id, number, status, backend, meta) VALUES ($1,$2,'live','local',
			'{"datatype":3,"origin":"console","encoded_digest":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}')`, saveID, n); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := st.PruneOverflow(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var goneNumbers []int
	for _, v := range gone {
		if v.SaveID == saveID {
			goneNumbers = append(goneNumbers, v.Number)
		}
	}
	// unpinned, non-conflict live versions are 1,4,5,6; the newest Keep (5,6) stay, 1 and 4 go.
	// kind "" (no meta): live unpinned non-conflict are 1,4,5,6 -> 1 and 4 go; kind datatype 3: 7,8,9 -> 7 goes.
	if len(goneNumbers) != 3 || goneNumbers[0]+goneNumbers[1]+goneNumbers[2] != 1+4+7 {
		t.Fatalf("pruned %v, want versions 1, 4 and 7", goneNumbers)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT number FROM versions WHERE save_id=$1 AND status='live' ORDER BY number`, saveID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var live []int
	for rows.Next() {
		var n int
		rows.Scan(&n)
		live = append(live, n)
	}
	want := []int{2, 3, 5, 6, 8, 9}
	if fmt.Sprint(live) != fmt.Sprint(want) {
		t.Fatalf("live = %v, want %v (pinned 2 and conflict 3 kept, newest two of each kind kept)", live, want)
	}
	// a second pass finds nothing to do
	again, err := st.PruneOverflow(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range again {
		if v.SaveID == saveID {
			t.Fatalf("second pass pruned version %d again", v.Number)
		}
	}
}
