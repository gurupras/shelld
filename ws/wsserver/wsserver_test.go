package wsserver

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gurupras/shelld/ws"
)

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckUpgrade(r); err != nil {
			http.Error(w, err.Error(), err.(*UpgradeError).Status)
			return
		}
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			op, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(op, msg); err != nil {
				return
			}
		}
	}))
}

func TestRejectsPlainRequest(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// ws.Client against the real Upgrade: the handshake both halves agree on,
// and a message each way through it.
func TestClientRoundTripsThroughUpgrade(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	nc, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ws.Client(nc, "shelld", "/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, m := range []string{"hello", strings.Repeat("x", 70000)} {
		if err := c.WriteMessage(ws.OpBinary, []byte(m)); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != ws.OpBinary || string(got) != m {
			t.Fatalf("echo of %d bytes: op %v, %d bytes, err %v", len(m), op, len(got), err)
		}
	}
}
