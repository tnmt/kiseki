package web

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tnmt/kiseki/internal/overland"
)

// LocationSource provides one day's stays and moves. It is read at request
// time and never stored, because overland-server owns location data.
type LocationSource interface {
	Day(ctx context.Context, date string) (overland.Day, error)
}

const locationTimeout = 5 * time.Second

type locationEntry struct {
	Kind       string // stay or move
	Start, End string
	Name       string
	Detail     string
	MapURL     string
}

// locationEntries merges stays and moves into one list in time order. A time
// outside date carries its month and day, since stays overlapping the date are
// returned with their full range.
func locationEntries(day overland.Day, date string) []locationEntry {
	type timed struct {
		at time.Time
		e  locationEntry
	}
	at := func(t time.Time) string {
		if t.Format(time.DateOnly) != date {
			return t.Format("01/02 15:04")
		}
		return t.Format("15:04")
	}
	var all []timed
	for _, s := range day.Stays {
		e := locationEntry{Kind: "stay", Start: at(s.Start), End: at(s.End), MapURL: mapURL(s)}
		switch {
		case s.Place != nil && s.Place.Name != "":
			e.Name = s.Place.Name
		case semanticLabel(s.SemanticType) != "":
			e.Name = semanticLabel(s.SemanticType)
		default:
			e.Name = fmt.Sprintf("%.4f, %.4f", s.Latitude, s.Longitude)
		}
		all = append(all, timed{s.Start, e})
	}
	for _, m := range day.Moves {
		e := locationEntry{Kind: "move", Start: at(m.Start), End: at(m.End), Name: moveLabel(m.Mode)}
		if m.DistanceMeters > 0 {
			e.Detail = distance(m.DistanceMeters)
		}
		all = append(all, timed{m.Start, e})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	out := make([]locationEntry, len(all))
	for i, t := range all {
		out[i] = t.e
	}
	return out
}

// semanticLabel names the Google Timeline semantic types that identify a
// place on their own. UNKNOWN and the like say nothing about the place.
func semanticLabel(t string) string {
	switch t {
	case "HOME", "INFERRED_HOME":
		return "Home"
	case "WORK", "INFERRED_WORK":
		return "Work"
	}
	return ""
}

func moveLabel(mode string) string {
	switch mode {
	case "", "UNKNOWN_ACTIVITY_TYPE":
		return "Move"
	case "IN_PASSENGER_VEHICLE", "IN_VEHICLE":
		return "Car"
	case "FLYING":
		return "Flight"
	}
	s := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(mode, "IN_"), "_", " "))
	return strings.ToUpper(s[:1]) + s[1:]
}

func distance(m float64) string {
	if m < 1000 {
		return strconv.Itoa(int(m)) + " m"
	}
	return strconv.FormatFloat(m/1000, 'f', 1, 64) + " km"
}

func mapURL(s overland.Stay) string {
	q := url.Values{"api": {"1"}, "query": {fmt.Sprintf("%f,%f", s.Latitude, s.Longitude)}}
	if s.GooglePlaceID != "" {
		q.Set("query_place_id", s.GooglePlaceID)
	}
	return "https://www.google.com/maps/search/?" + q.Encode()
}
