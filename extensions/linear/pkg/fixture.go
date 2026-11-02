package linearext

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/extensions/linear/lineartest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	trackerext "github.com/open-conduit/open-conduit/extensions/tracker/pkg"
)

// fixture is Linear's certification case: a fake Linear with one open
// issue, and the signed webhook delivery of its creation.
func fixture() (trackerext.Fixture, error) {
	const token = "lin-certify"
	f := lineartest.New(token, "ENG")
	f.AddIssue("ENG", 1, "The export is broken", "Todo")
	body := []byte(`{"type":"Issue","action":"create","webhookTimestamp":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `,"data":{"identifier":"ENG-1"}}`)
	h := http.Header{}
	h.Set("Linear-Signature", tracker.SignHMAC(trackerext.FixtureHookValue, body))
	h.Set("Linear-Delivery", "certify-1")
	conn, _ := json.Marshal(map[string]any{"api_url": f.URL()})
	return trackerext.Fixture{Conn: conn, Token: token, Header: h, Body: body, Close: f.Close,
		Config: json.RawMessage(`{"namespace":"linear/eng","scope":"` + trackerext.FixtureScope + `","teams":["ENG"],"transitions":{"claimed":"In Progress"},"webhook_secret":"hook"}`)}, nil
}
