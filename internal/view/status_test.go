package view

import (
	"strings"
	"testing"

	"ivank.me/zfstui/internal/derive"
	"ivank.me/zfstui/internal/model"
)

func TestStatusTable(t *testing.T) {
	states := []model.HostState{
		{
			Host: "dev1",
			Datasets: []model.Dataset{
				{Host: "dev1", Name: "ssd/srv/git", Type: "filesystem", Written: 4096},
			},
			Snapshots: []model.Snapshot{
				{Host: "dev1", Dataset: "ssd/srv/git", Name: "20260601", GUID: "g1", Creation: 100},
			},
		},
		{
			Host: "dev2",
			Datasets: []model.Dataset{
				{Host: "dev2", Name: "tank/bak/git", Type: "filesystem"},
			},
			Snapshots: []model.Snapshot{
				{Host: "dev2", Dataset: "tank/bak/git", Name: "20260601", GUID: "g1", Creation: 100},
			},
		},
	}
	out := Status(derive.Groups(states))
	t.Log("\n" + out)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header+1 row, got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "dev1:ssd") || !strings.Contains(lines[0], "dev2:tank") {
		t.Errorf("header: %q", lines[0])
	}
	// dev1 has unsnapshotted writes => +; dev2 is behind, no marker.
	if !strings.HasPrefix(lines[1], "git") ||
		!strings.Contains(lines[1], "srv/@20260601+") ||
		!strings.Contains(lines[1], "bak/@20260601") {
		t.Errorf("row: %q", lines[1])
	}
}
