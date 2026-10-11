package web

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/tnmt/kiseki/internal/store"
)

// One weigh-in arrives as several measure groups (weight and body
// composition, heart rate, PWV and vascular age); groups closer together than
// this belong to the same weigh-in.
const weighInGap = 30 * 60 * 1000

type metricFormat struct {
	typ    int
	label  string
	unit   string
	digits int
}

// Display order. Other measure types (fat mass, hydration, ...) are not shown.
var weighInMetrics = []metricFormat{
	{1, "Weight", "kg", 1},
	{6, "Fat", "%", 1},
	{76, "Muscle", "kg", 1},
	{91, "PWV", "m/s", 1},
	{11, "HR", "bpm", 0},
	{155, "Vascular age", "", 0},
}

type metric struct {
	Label, Value string
}

type weighIn struct {
	Time    string
	Metrics []metric
}

type measureGroup struct {
	Measures []struct {
		Value int64 `json:"value"`
		Type  int   `json:"type"`
		Unit  int   `json:"unit"`
	} `json:"measures"`
}

// weighIns groups the day's Withings rows, which arrive in time order, into
// weigh-ins. A later value of the same type within a weigh-in wins.
func weighIns(rows []store.Row) []weighIn {
	var out []weighIn
	var latest map[int]float64
	var start, last int64
	var tz string
	flush := func() {
		if latest == nil {
			return
		}
		var ms []metric
		for _, f := range weighInMetrics {
			if v, ok := latest[f.typ]; ok {
				ms = append(ms, metric{f.label, strconv.FormatFloat(v, 'f', f.digits, 64) + f.unit})
			}
		}
		if ms != nil {
			out = append(out, weighIn{Time: clock(start, tz, "15:04"), Metrics: ms})
		}
		latest = nil
	}
	for _, row := range rows {
		var g measureGroup
		if json.Unmarshal([]byte(row.Payload), &g) != nil {
			continue
		}
		if latest != nil && row.OccurredAt-last > weighInGap {
			flush()
		}
		if latest == nil {
			latest = map[int]float64{}
			start, tz = row.OccurredAt, row.TZ
		}
		last = row.OccurredAt
		for _, m := range g.Measures {
			latest[m.Type] = float64(m.Value) * math.Pow10(m.Unit)
		}
	}
	flush()
	return out
}
