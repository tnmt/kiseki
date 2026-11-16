package endpoint

import "testing"

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://kiseki.example.com", true},
		{"https://192.0.2.1:8443/", true},
		{"http://127.0.0.1:8095", true},
		{"http://127.1.2.3", true},
		{"http://[::1]:8095", true},
		{"http://localhost:8095", true},
		{"http://LOCALHOST", true},
		{"http://kiseki.example.com", false},
		{"http://192.0.2.1:8095", false},
		{"http://localhost.example.com", false},
		{"http://127.0.0.1.example.com", false},
		{"ftp://127.0.0.1", false},
		{"kiseki.example.com", false},
		{"https://", false},
		{"", false},
	} {
		if err := Check(c.url); (err == nil) != c.ok {
			t.Errorf("Check(%q) = %v, want ok=%v", c.url, err, c.ok)
		}
	}
}
