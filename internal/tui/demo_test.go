package tui

import (
	"time"

	"github.com/valbaudo/dawn/internal/store"
)

// Demo data for the renderers. A sweep 1h47m into a CyberGym Level 1 run with
// 20 workers: mostly healthy, one worker past its pool cap, one wedged, one
// whose container went away. These are the states the layout has to be legible
// in, so they are the states it gets rendered in.

var demoNow = time.Date(2026, 9, 2, 14, 31, 8, 0, time.UTC)

type demoSpec struct {
	task             string
	elapsed          time.Duration
	in, out          int64
	spend, cap       store.USD
	state            LiveState
	quiet            time.Duration
	note             string
	child            string
	childElapsed     time.Duration
	childIn, cOut    int64
	childSpend, cap2 store.USD
	childState       LiveState
	childQuiet       time.Duration
	childNote        string
	childModel       string
}

var demoSpecs = []demoSpec{
	{"w00 arvo:10400", 12*time.Minute + 41*time.Second, 1_940_000, 28_400, 4.11, 6, Live, 0, "",
		"salvage", 52 * time.Second, 181_000, 3_100, 0.42, 1.50, Live, 0, "", "claude-opus-5"},
	{"w01 arvo:3938", 31*time.Minute + 8*time.Second, 4_210_000, 61_000, 9.88, 6, Busy, 4 * time.Second, "over cap",
		"", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w02 oss-fuzz:42535201", 7*time.Minute + 55*time.Second, 1_100_000, 17_200, 2.34, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w03 arvo:11238", 15*time.Minute + 3*time.Second, 2_600_000, 35_900, 5.72, 6, Live, 0, "",
		"verify-poc", 2*time.Minute + 11*time.Second, 402_000, 6_400, 0.91, 1.50, Busy, 9 * time.Second, "", "claude-sonnet-5"},
	{"w04 arvo:7712", 44*time.Minute + 2*time.Second, 5_880_000, 88_100, 6.00, 6, Idle, 31 * time.Second, "budget",
		"", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w05 arvo:2201", 3*time.Minute + 18*time.Second, 388_000, 5_900, 0.83, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w06 oss-fuzz:39114820", 22*time.Minute + 40*time.Second, 3_050_000, 44_600, 6.51, 6, Busy, 12 * time.Second, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w07 arvo:15003", 51*time.Minute + 26*time.Second, 2_970_000, 41_200, 6.19, 6, Stale, 4*time.Minute + 12*time.Second, "",
		"minimize", 6*time.Minute + 5*time.Second, 210_000, 2_800, 0.44, 1.50, Stale, 4*time.Minute + 12*time.Second, "", "claude-opus-5"},
	{"w08 arvo:9081", 9*time.Minute + 12*time.Second, 1_320_000, 19_800, 2.81, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w09 arvo:4417", 18*time.Minute + 47*time.Second, 2_410_000, 33_100, 5.13, 6, Busy, 2 * time.Second, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w10 oss-fuzz:41002773", 5*time.Minute + 31*time.Second, 702_000, 10_400, 1.49, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w11 arvo:6650", 27*time.Minute + 9*time.Second, 3_610_000, 52_000, 7.68, 6, Live, 0, "",
		"triage", 41 * time.Second, 96_000, 1_400, 0.21, 1.50, Live, 0, "", "claude-haiku-4-5"},
	{"w12 arvo:13390", 1*time.Minute + 4*time.Second, 118_000, 1_700, 0.25, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w13 arvo:8802", 38*time.Minute + 55*time.Second, 4_020_000, 57_300, 8.55, 6, Gone, 1*time.Minute + 3*time.Second, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w14 arvo:5129", 13*time.Minute + 22*time.Second, 1_780_000, 25_600, 3.79, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w15 oss-fuzz:38870145", 20*time.Minute + 16*time.Second, 2_840_000, 39_700, 6.03, 6, Busy, 7 * time.Second, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w16 arvo:10977", 2*time.Minute + 49*time.Second, 301_000, 4_500, 0.64, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w17 arvo:3304", 33*time.Minute + 41*time.Second, 4_450_000, 63_900, 9.47, 6, Idle, 22 * time.Second, "overloaded_error x3", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w18 arvo:12016", 11*time.Minute + 38*time.Second, 1_610_000, 23_100, 3.42, 6, Live, 0, "", "", 0, 0, 0, 0, 0, 0, 0, "", ""},
	{"w19 arvo:2965", 25*time.Minute + 55*time.Second, 3_330_000, 47_800, 7.09, 6, Live, 0, "",
		"salvage", 1*time.Minute + 47*time.Second, 288_000, 4_200, 0.63, 1.50, Live, 0, "", "claude-opus-5"},
}

func demoFrame() Frame {
	f := Frame{
		Sweep:      "sweep 7f3a91c2",
		Name:       "cybergym-l1-nsfocus",
		Started:    demoNow.Add(-1*time.Hour - 47*time.Minute - 12*time.Second),
		Now:        demoNow,
		Query:      11 * time.Millisecond,
		Workers:    20,
		Tasks:      142,
		TasksTotal: 1507,
		OK:         121, Rejected: 9, Failed: 8, Cancelled: 4,
		Calls: 4318, Attempts: 4502,
		In: 214_900_000, Out: 3_710_000, CacheR: 198_200_000,
		Spend: 412.66, Cap: 900,
	}
	for _, d := range demoSpecs {
		n := &Node{
			Name: d.task, Opened: demoNow.Add(-d.elapsed),
			In: d.in, Out: d.out, Spend: d.spend, Cap: d.cap,
			State: d.state, Quiet: d.quiet, Outcome: d.note,
			Model: "claude-opus-5", PriceKnown: true,
		}
		if d.child != "" {
			n.kids = append(n.kids, &Node{
				Name: d.child, Opened: demoNow.Add(-d.childElapsed),
				In: d.childIn, Out: d.cOut, Spend: d.childSpend, Cap: d.cap2,
				State: d.childState, Quiet: d.childQuiet, Outcome: d.childNote,
				Model: d.childModel, PriceKnown: true,
			})
		}
		f.Roots = append(f.Roots, n)
	}
	return f
}
