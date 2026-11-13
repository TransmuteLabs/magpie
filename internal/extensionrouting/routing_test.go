package extensionrouting

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/routes"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestRoutingSeatProjectionIsBoundedAndOpaque(test *testing.T) {
	descriptor := sdk.RoutingStrategyDescriptor{ID: "reverse"}
	request, err := routingRequest("routing.fixture", descriptor, "group/test", []routes.Seat{
		{Key: "PRIVATE-API-KEY", Weight: 2, Account: true, Known: true, Used: 12, Soon: []time.Time{time.UnixMilli(1234)},
			Unreached: 1, Lapse: time.UnixMilli(5678), LeftIn: "PRIVATE-ACCOUNT\x00credits", Left: 9, TTFT: 42, TPS: 15},
		{Key: "PRIVATE-EMAIL@example.invalid", Weight: 1},
	})
	if err != nil {
		test.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil || strings.Contains(string(raw), "PRIVATE") || request.Seats[0].Soon[0] != 1234 ||
		request.Seats[0].Lapse != 5678 || request.Seats[0].Weight != 2 || request.Seats[0].TTFT != 42 ||
		len(request.Seats[0].Key) != 64 || len(request.Seats[0].LeftIn) != 64 {
		test.Fatal("routing projection lost metrics or exposed raw route identity")
	}
	again, _ := routingRequest("routing.fixture", descriptor, "group/test", []routes.Seat{{Key: "PRIVATE-API-KEY", Weight: 2}})
	other, _ := routingRequest("other", descriptor, "group/test", []routes.Seat{{Key: "PRIVATE-API-KEY", Weight: 2}})
	if request.Seats[0].Key != again.Seats[0].Key || request.Seats[0].Key == other.Seats[0].Key {
		test.Fatal("routing opaque identities are unstable or shared between modules")
	}
	if _, err := routingRequest("routing.fixture", descriptor, "pool", []routes.Seat{{Weight: 1, Tokens: math.NaN()}}); err == nil {
		test.Fatal("nonfinite routing measurements reached serialization")
	}
}
