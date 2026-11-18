package extension

import (
	"encoding/json"
	"os"
	"testing"
)

func TestReadyURLPolicy(t *testing.T) {
	body, err := os.ReadFile("testdata/ready-urls.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		URL   string
		Valid bool
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			if got := ValidReadyURL(test.URL); got != test.Valid {
				t.Fatalf("ValidReadyURL=%t, want %t", got, test.Valid)
			}
		})
	}
}
