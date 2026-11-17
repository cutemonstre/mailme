// mailme emails the owner (and only the owner) a plain-text note through
// Cloudflare Email Sending's REST API.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

type config struct {
	AccountID, Token, To, From, FromName, APIBase string
}

type address struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

type payload struct {
	To      []string `json:"to"`
	From    address  `json:"from"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result struct {
		Delivered           []string `json:"delivered"`
		Queued              []string `json:"queued"`
		PermanentBounces    []string `json:"permanent_bounces"`
		SuppressedRecipient []string `json:"suppressed_recipients"`
	} `json:"result"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mailme", "env")
}

// parseEnv parses KEY=VALUE lines; '#' comments and blank lines are ignored,
// and one pair of surrounding quotes is stripped from values.
func parseEnv(r io.Reader) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[k] = v
	}
	return m
}

// loadConfig merges file values with real environment (environment wins).
func loadConfig(path string, getenv func(string) string, warn io.Writer, needToken bool) (config, error) {
	vals := map[string]string{}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(warn, "mailme: warning: %s has permissions %04o; run chmod 600 on it\n", path, st.Mode().Perm())
		}
		vals = parseEnv(f)
	}
	get := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		if v := vals[k]; v != "" {
			return v
		}
		return def
	}
	c := config{
		AccountID: get("CF_ACCOUNT_ID", ""),
		Token:     get("CF_EMAIL_TOKEN", ""),
		To:        get("MAILME_TO", ""),
		From:      get("MAILME_FROM", ""),
		FromName:  get("MAILME_FROM_NAME", "Note to self"),
		APIBase:   strings.TrimRight(get("MAILME_API_BASE", "https://api.cloudflare.com/client/v4"), "/"),
	}
	req := []struct{ k, v string }{{"CF_ACCOUNT_ID", c.AccountID}, {"MAILME_TO", c.To}, {"MAILME_FROM", c.From}}
	if needToken {
		req = append(req, struct{ k, v string }{"CF_EMAIL_TOKEN", c.Token})
	}
	for _, r := range req {
		if r.v == "" {
			return c, fmt.Errorf("%s is missing; set it in %s or the environment", r.k, path)
		}
	}
	return c, nil
}

// cleanSubject strips CR/LF and collapses whitespace.
func cleanSubject(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// deriveSubject uses the first non-empty line of body, capped at 80 runes.
func deriveSubject(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r", "\n"), "\n") {
		s := cleanSubject(line)
		if s == "" {
			continue
		}
		if r := []rune(s); len(r) > 80 {
			s = strings.TrimSpace(string(r[:80])) + "…"
		}
		return s
	}
	return ""
}

func buildPayload(c config, subject, body string) payload {
	return payload{
		To:      []string{c.To},
		From:    address{Address: c.From, Name: c.FromName},
		Subject: cleanSubject(subject),
		Text:    body,
	}
}

func contains(list []string, a string) bool {
	for _, x := range list {
		if strings.EqualFold(x, a) {
			return true
		}
	}
	return false
}

// send posts the payload. It returns queued=true when the recipient was only
// queued. Errors never contain the token.
func send(c config, p payload) (queued bool, err error) {
	redact := func(s string) string {
		if c.Token != "" {
			s = strings.ReplaceAll(s, c.Token, "[redacted]")
		}
		return s
	}
	buf, _ := json.Marshal(p)
	url := c.APIBase + "/accounts/" + c.AccountID + "/email/sending/send"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return false, errors.New(redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return false, fmt.Errorf("request failed: %s", redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	fail := func(detail string) error {
		return fmt.Errorf("send failed (HTTP %d): %s", resp.StatusCode, redact(detail))
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		if len(raw) > 300 {
			raw = raw[:300]
		}
		return false, fail(strings.TrimSpace(string(raw)))
	}
	var parts []string
	for _, e := range env.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	if contains(env.Result.PermanentBounces, c.To) {
		parts = append(parts, "recipient permanently bounced")
	}
	if contains(env.Result.SuppressedRecipient, c.To) {
		parts = append(parts, "recipient suppressed")
	}
	if resp.StatusCode/100 != 2 || !env.Success ||
		contains(env.Result.PermanentBounces, c.To) || contains(env.Result.SuppressedRecipient, c.To) {
		if len(parts) == 0 {
			parts = append(parts, "no error detail returned")
		}
		return false, fail(strings.Join(parts, "; "))
	}
	switch {
	case contains(env.Result.Delivered, c.To):
		return false, nil
	case contains(env.Result.Queued, c.To):
		return true, nil
	}
	return false, fail("recipient not in delivered or queued")
}

func stdinIsPipe() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

func run(args []string, stdin io.Reader, hasStdin bool, stdout, stderr io.Writer, getenv func(string) string, cfgPath string) int {
	fs := flag.NewFlagSet("mailme", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var subject string
	var dry, showVer bool
	fs.StringVar(&subject, "s", "", "subject")
	fs.StringVar(&subject, "subject", "", "subject")
	fs.BoolVar(&dry, "dry-run", false, "print the JSON payload instead of sending")
	fs.BoolVar(&showVer, "version", false, "print version")
	usage := func() {
		fmt.Fprint(stderr, `usage: mailme [-s subject] [--dry-run] [message...]
       echo "body" | mailme -s "subject"
       mailme --version

Emails the owner (MAILME_TO) a plain-text note. Config: `+cfgPath+`
`)
		fs.PrintDefaults()
	}
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVer {
		fmt.Fprintln(stdout, "mailme", version)
		return 0
	}
	body := strings.Join(fs.Args(), " ")
	if body == "" && hasStdin {
		b, _ := io.ReadAll(stdin)
		body = strings.TrimRight(string(b), "\r\n")
	}
	if strings.TrimSpace(body) == "" {
		body = ""
	}
	subject = cleanSubject(subject)
	if subject == "" && body == "" {
		usage()
		return 2
	}
	if subject == "" {
		subject = deriveSubject(body)
	}
	if body == "" {
		body = subject
	}

	c, err := loadConfig(cfgPath, getenv, stderr, !dry)
	if err != nil {
		fmt.Fprintln(stderr, "mailme:", err)
		return 1
	}
	p := buildPayload(c, subject, body)
	if dry {
		out, _ := json.MarshalIndent(p, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	queued, err := send(c, p)
	if err != nil {
		fmt.Fprintln(stderr, "mailme:", err)
		return 1
	}
	if queued {
		fmt.Fprintln(stdout, "Queued for inbox:", p.Subject)
	} else {
		fmt.Fprintln(stdout, "Sent to inbox:", p.Subject)
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, stdinIsPipe(), os.Stdout, os.Stderr, os.Getenv, configPath()))
}
