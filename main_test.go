package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveSubject(t *testing.T) {
	long := strings.Repeat("é", 100)
	cases := map[string]string{
		"pay bills":                   "pay bills",
		"\n\n  first   line \nsecond": "first line",
		"crlf one\r\ntwo":             "crlf one",
		"\r\n\r\n  after blanks\r\nx": "after blanks",
		long:                          strings.Repeat("é", 80) + "…",
		"":                            "",
	}
	for in, want := range cases {
		if got := deriveSubject(in); got != want {
			t.Errorf("deriveSubject(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanSubject("a\r\nb\n c"); got != "a b c" {
		t.Errorf("cleanSubject = %q", got)
	}
}

func TestEnvParsingAndOverride(t *testing.T) {
	m := parseEnv(strings.NewReader("# c\n\nA=1\nB=\"two words\"\nC='x'\nexport D = 4\nbad\n"))
	if m["A"] != "1" || m["B"] != "two words" || m["C"] != "x" || m["D"] != "4" || len(m) != 4 {
		t.Fatalf("parsed %v", m)
	}
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("CF_ACCOUNT_ID=file\nMAILME_TO=a@b.c\nMAILME_FROM=me@example.com\nCF_EMAIL_TOKEN=tok\n"), 0o644)
	var warn strings.Builder
	c, err := loadConfig(p, func(k string) string {
		if k == "CF_ACCOUNT_ID" {
			return "env"
		}
		return ""
	}, &warn, true)
	if err != nil || c.AccountID != "env" || c.To != "a@b.c" || c.From != "me@example.com" || c.FromName != "Note to self" {
		t.Fatalf("cfg %+v err %v", c, err)
	}
	if !strings.Contains(warn.String(), "permissions") {
		t.Errorf("expected permissions warning")
	}
	_, err = loadConfig(p, func(string) string { return "" }, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("CF_ACCOUNT_ID=x\nMAILME_TO=a@b.c\nMAILME_FROM=me@example.com\n"), 0o600)
	if _, err = loadConfig(p, func(string) string { return "" }, io.Discard, true); err == nil || !strings.Contains(err.Error(), "CF_EMAIL_TOKEN") {
		t.Errorf("want missing token error, got %v", err)
	}
	if _, err = loadConfig(p, func(string) string { return "" }, io.Discard, false); err != nil {
		t.Errorf("dry-run should not need token: %v", err)
	}
}

func TestPayloadShape(t *testing.T) {
	c := config{To: "me@x.com", From: "me@example.com", FromName: "Note to self"}
	b, _ := json.Marshal(buildPayload(c, "hi\r\nthere", "body"))
	want := `{"to":["me@x.com"],"from":{"address":"me@example.com","name":"Note to self"},"subject":"hi there","text":"body"}`
	if string(b) != want {
		t.Errorf("got %s", b)
	}
}

func TestSend(t *testing.T) {
	const token = "SECRET-TOKEN"
	cases := []struct {
		name, body string
		status     int
		wantErr    bool
		queued     bool
		errHas     string
	}{
		{"queued", `{"success":true,"errors":[],"result":{"queued":["me@x.com"]}}`, 200, false, true, ""},
		{"delivered", `{"success":true,"result":{"delivered":["ME@x.com"]}}`, 200, false, false, ""},
		{"failure", `{"success":false,"errors":[{"code":1001,"message":"bad thing"}],"result":null}`, 400, true, false, "1001 bad thing"},
		{"bounce", `{"success":true,"result":{"permanent_bounces":["me@x.com"]}}`, 200, true, false, "bounced"},
		{"suppressed", `{"success":true,"result":{"suppressed_recipients":["me@x.com"]}}`, 200, true, false, "suppressed"},
		{"not listed", `{"success":true,"result":{}}`, 200, true, false, "not in delivered"},
		{"500 html", `<html>oops ` + token + `</html>`, 500, true, false, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := config{AccountID: "acct", Token: token, To: "me@x.com", From: "f@x.com", FromName: "n", APIBase: srv.URL}
			queued, err := send(c, buildPayload(c, "s", "b"))
			if gotAuth != "Bearer "+token || gotPath != "/accounts/acct/email/sending/send" {
				t.Errorf("auth %q path %q", gotAuth, gotPath)
			}
			if (err != nil) != tc.wantErr || queued != tc.queued {
				t.Fatalf("queued=%v err=%v", queued, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), token) {
					t.Errorf("token leaked: %v", err)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Errorf("error %q missing %q", err, tc.errHas)
				}
			}
		})
	}
}

func TestMissingFrom(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("CF_ACCOUNT_ID=x\nMAILME_TO=a@b.c\nCF_EMAIL_TOKEN=tok\n"), 0o600)
	if _, err := loadConfig(p, func(string) string { return "" }, io.Discard, true); err == nil || !strings.Contains(err.Error(), "MAILME_FROM") {
		t.Errorf("want missing MAILME_FROM error, got %v", err)
	}
}

func TestRunDryAndUsage(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"MAILME_TO": "me@x.com", "MAILME_FROM": "me@example.com", "CF_ACCOUNT_ID": "a", "CF_EMAIL_TOKEN": "tok"}[k]
	}
	var out, errb strings.Builder
	if code := run([]string{"--dry-run", "hello", "world"}, nil, false, &out, &errb, env, "/nonexistent"); code != 0 ||
		!strings.Contains(out.String(), `"subject": "hello world"`) || strings.Contains(out.String(), "tok") {
		t.Errorf("code %d out %s err %s", code, out.String(), errb.String())
	}
	out.Reset()
	run([]string{"--dry-run", "-s", "S"}, strings.NewReader("piped\nbody\n"), true, &out, &errb, env, "/nonexistent")
	if !strings.Contains(out.String(), `"text": "piped\nbody"`) {
		t.Errorf("stdin body: %s", out.String())
	}
	if code := run(nil, nil, false, &out, &errb, env, "/nonexistent"); code != 2 {
		t.Errorf("no args code %d", code)
	}
}
