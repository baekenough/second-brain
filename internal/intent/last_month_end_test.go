package intent_test

import (
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/timeutil"
)

func TestLastMonthAtMonthEnd(t *testing.T) {
	for _, date := range []string{"2026-03-31", "2024-03-31", "2026-05-31", "2026-07-31", "2026-01-31", "2026-03-30", "2026-03-29"} {
		t.Run(date, func(t *testing.T) {
			now, err := time.ParseInLocation("2006-01-02", date, timeutil.KST())
			if err != nil {
				t.Fatal(err)
			}
			wantTo := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.KST())
			wantFrom := wantTo.AddDate(0, -1, 0)
			from, to := classifyWindow(t, now.UTC(), "지난달")
			if !from.Equal(wantFrom) || !to.Equal(wantTo) {
				t.Fatalf("classifier = [%v,%v), want [%v,%v)", from, to, wantFrom, wantTo)
			}
			from, to, _, ok := intent.DeterministicWindow("지난달", now)
			if !ok || !from.Equal(wantFrom) || !to.Equal(wantTo) {
				t.Fatalf("planner = [%v,%v), want [%v,%v)", from, to, wantFrom, wantTo)
			}
		})
	}
}
