// Package overland reads location history from overland-server's read API.
// overland-server owns the data; kiseki fetches a day when a page needs it and
// stores nothing.
package overland

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

type Client struct {
	// BaseURL is where overland-server is reached, such as http://127.0.0.1:8095.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Day mirrors overland-server's GET /api/days/{date} response.
type Day struct {
	Date     string `json:"date"`
	Timezone string `json:"timezone"`
	Stays    []Stay `json:"stays"`
	Moves    []Move `json:"moves"`
}

type Stay struct {
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Latitude      float64   `json:"latitude"`
	Longitude     float64   `json:"longitude"`
	Source        string    `json:"source"`
	GooglePlaceID string    `json:"google_place_id"`
	SemanticType  string    `json:"semantic_type"`
	Place         *Place    `json:"place"`
}

type Place struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Move struct {
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	Mode           string    `json:"mode"`
	DistanceMeters float64   `json:"distance_meters"`
	Source         string    `json:"source"`
}

// Day fetches the stays and moves of date (YYYY-MM-DD in overland-server's
// time zone).
func (c *Client) Day(ctx context.Context, date string) (Day, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/api/days/" + url.PathEscape(date)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Day{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Day{}, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(body).Decode(&e)
		return Day{}, fmt.Errorf("overland-server: %s %s", resp.Status, e.Error)
	}
	var day Day
	if err := json.NewDecoder(body).Decode(&day); err != nil {
		return Day{}, fmt.Errorf("overland-server: decode day: %w", err)
	}
	return day, nil
}
