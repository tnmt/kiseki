package overland

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if r.URL.Path != "/api/days/2026-09-30" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"date":"2026-09-30","timezone":"Asia/Tokyo",
			"stays":[{"start":"2026-09-29T22:00:00+09:00","end":"2026-09-30T08:30:00+09:00","latitude":35.0,"longitude":139.0,
				"source":"google-timeline","google_place_id":"ChIJ","semantic_type":"HOME","place":{"id":1,"name":"Home"}}],
			"moves":[{"start":"2026-09-30T08:30:00+09:00","end":"2026-09-30T09:10:00+09:00","mode":"IN_TRAIN","distance_meters":7000,"source":"google-timeline"}]}`))
	}))
	defer srv.Close()

	day, err := (&Client{BaseURL: srv.URL + "/", Token: "secret"}).Day(context.Background(), "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	if len(day.Stays) != 1 || day.Stays[0].Place == nil || day.Stays[0].Place.Name != "Home" || day.Stays[0].Start.Hour() != 22 {
		t.Errorf("stays = %+v", day.Stays)
	}
	if len(day.Moves) != 1 || day.Moves[0].Mode != "IN_TRAIN" || day.Moves[0].DistanceMeters != 7000 {
		t.Errorf("moves = %+v", day.Moves)
	}

	_, err = (&Client{BaseURL: srv.URL, Token: "wrong"}).Day(context.Background(), "2026-09-30")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("wrong token: %v", err)
	}
}
