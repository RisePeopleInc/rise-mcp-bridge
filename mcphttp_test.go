package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// runPump feeds lines to a Pump aimed at srv and returns the messages it wrote.
func runPump(t *testing.T, srv *httptest.Server, ts oauth2.TokenSource, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	p := NewPump(srv.Client(), srv.URL, ts, &out)
	if err := p.Run(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad output line %q: %v", l, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func errorText(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

type failingSource struct{}

func (failingSource) Token() (*oauth2.Token, error) { return nil, errors.New("refresh refused") }

const call = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`

func TestFailedRequestGetsErrorReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should reach the server without a token")
	}))
	defer srv.Close()
	msgs := runPump(t, srv, failingSource{}, call, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(msgs) != 1 || msgs[0]["id"] != float64(7) || !strings.Contains(errorText(msgs[0]), "refresh refused") {
		t.Fatalf("want one error reply for id 7 (none for the notification), got %v", msgs)
	}
}

func TestUnansweredRequestGetsErrorReply(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"html 200": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>proxy page</html>")
		},
		"stream with only a notification": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		},
		"202": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			msgs := runPump(t, srv, nil, call)
			last := msgs[len(msgs)-1]
			if last["id"] != float64(7) || !strings.Contains(errorText(last), "no response") {
				t.Fatalf("want a no-response error for id 7 last, got %v", msgs)
			}
		})
	}
}

func TestAnsweredRequestGetsNoExtraReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n")
	}))
	defer srv.Close()
	msgs := runPump(t, srv, nil, call)
	if len(msgs) != 2 || msgs[1]["result"] == nil {
		t.Fatalf("want the notification and the result only, got %v", msgs)
	}
}

func TestBatchRequestsEachGetAnErrorReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	msgs := runPump(t, srv, failingSource{},
		`[{"jsonrpc":"2.0","id":1,"method":"a"},{"jsonrpc":"2.0","method":"n"},{"jsonrpc":"2.0","id":"two","method":"b"}]`)
	if len(msgs) != 2 || msgs[0]["id"] != float64(1) || msgs[1]["id"] != "two" {
		t.Fatalf("want error replies for ids 1 and \"two\", got %v", msgs)
	}
}

// rotatingSource hands out "old" until told the server rejected it.
type rotatingSource struct{ invalidated *oauth2.Token }

func (s *rotatingSource) Token() (*oauth2.Token, error) {
	if s.invalidated != nil {
		return &oauth2.Token{AccessToken: "new"}, nil
	}
	return &oauth2.Token{AccessToken: "old"}, nil
}
func (s *rotatingSource) invalidate(tok *oauth2.Token) { s.invalidated = tok }

func TestUnauthorizedRetriesOnceWithFreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	}))
	defer srv.Close()
	src := &rotatingSource{}
	msgs := runPump(t, srv, src, call)
	if len(msgs) != 1 || msgs[0]["result"] == nil {
		t.Fatalf("want the result after one retry, got %v", msgs)
	}
	if src.invalidated == nil || src.invalidated.AccessToken != "old" {
		t.Fatalf("want the token that was sent (old) invalidated, got %v", src.invalidated)
	}
}

func TestUnauthorizedWithStaticTokenKeepsServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token expired at noon", http.StatusUnauthorized)
	}))
	defer srv.Close()
	msgs := runPump(t, srv, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), call)
	if len(msgs) != 1 || !strings.Contains(errorText(msgs[0]), "token expired at noon") {
		t.Fatalf("want the server's 401 message in the reply, got %v", msgs)
	}
}

// sessionServer acts like Metabase: initialize opens a session, and other
// messages need a live one (404 "Invalid or expired session" otherwise).
type sessionServer struct {
	live  string
	opens int
}

func (s *sessionServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var head rpcHead
	_ = json.NewDecoder(r.Body).Decode(&head)
	switch {
	case head.Method == "initialize":
		s.opens++
		s.live = fmt.Sprintf("s%d", s.opens)
		w.Header().Set("Mcp-Session-Id", s.live)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, head.ID)
	case r.Header.Get("Mcp-Session-Id") == "":
		http.Error(w, `{"error":"Missing Mcp-Session-Id header"}`, http.StatusBadRequest)
	case r.Header.Get("Mcp-Session-Id") != s.live:
		http.Error(w, `{"error":"Invalid or expired session"}`, http.StatusNotFound)
	case head.Method == "expire":
		s.live = "gone"
		w.WriteHeader(http.StatusAccepted)
	case head.ID != nil:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"session":%q}}`, head.ID, s.live)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func TestExpiredSessionIsReopened(t *testing.T) {
	ss := &sessionServer{}
	srv := httptest.NewServer(ss)
	defer srv.Close()
	msgs := runPump(t, srv, nil, initReq, `{"jsonrpc":"2.0","method":"expire"}`, call)
	if len(msgs) != 2 || msgs[1]["result"] == nil || ss.opens != 2 {
		t.Fatalf("want the init reply then the call's result on a new session, got %v (opens %d)", msgs, ss.opens)
	}
}

func TestFailedInitializeIsRetriedBeforeNextCall(t *testing.T) {
	ss := &sessionServer{}
	srv := httptest.NewServer(ss)
	defer srv.Close()
	src := &flakySource{failFirst: true}
	msgs := runPump(t, srv, src, initReq, call)
	if len(msgs) != 2 || errorText(msgs[0]) == "" || msgs[1]["result"] == nil {
		t.Fatalf("want an init error then the call's result, got %v", msgs)
	}
}

// flakySource fails its first Token call, then works.
type flakySource struct{ failFirst bool }

func (s *flakySource) Token() (*oauth2.Token, error) {
	if s.failFirst {
		s.failFirst = false
		return nil, errors.New("token not ready")
	}
	return &oauth2.Token{AccessToken: "ok"}, nil
}
