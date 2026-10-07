package ask

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The fetch_url tool: the ask model may read external links (documents
// cite URLs, users paste them). Plain HTTP GET, HTML reduced to text,
// size-capped — a small, local WebFetch.

const (
	fetchTimeout  = 30 * time.Second
	fetchMaxBytes = 500_000
	fetchMaxChars = 8000
)

var (
	fetchScript = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	fetchStyle  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	fetchBreak  = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/section|/article|/pre)\b[^>]*>`)
	fetchTags   = regexp.MustCompile(`(?s)<[^>]+>`)
	fetchSpaces = regexp.MustCompile(`[ \t\r\f\v]+`)
	fetchBlank  = regexp.MustCompile(`\n{3,}`)
	fetchLine   = regexp.MustCompile(` *\n *`)
)

// webFetch downloads an http(s) URL and returns readable text. Link-local
// metadata addresses and non-http(s) schemes are refused.
func webFetch(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http/https URLs may be fetched")
	}
	if u.Hostname() == "169.254.169.254" || u.Hostname() == "metadata.google.internal" {
		return "", fmt.Errorf("blocked host")
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "vellum-ask/1 (+local)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	// ProxyFromEnvironment honors HTTPS_PROXY/HTTP_PROXY (this machine
	// needs the local proxy for egress).
	client := &http.Client{
		Timeout:   fetchTimeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	text := string(body)
	if isHTML(resp.Header.Get("Content-Type"), text) {
		text = htmlToText(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("no readable text at %s", u.String())
	}
	if len(text) > fetchMaxChars {
		text = text[:fetchMaxChars] + "\n…[truncated]"
	}
	return text, nil
}

func isHTML(ctype, body string) bool {
	if strings.Contains(strings.ToLower(ctype), "html") {
		return true
	}
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(strings.ToLower(head), "<html") ||
		strings.Contains(strings.ToLower(head), "<!doctype html")
}

// htmlToText is a deliberately crude HTML→text reduction (no DOM): drop
// script/style, turn block tags into newlines, strip the rest, unescape
// entities, and collapse whitespace.
func htmlToText(h string) string {
	h = fetchScript.ReplaceAllString(h, " ")
	h = fetchStyle.ReplaceAllString(h, " ")
	h = fetchBreak.ReplaceAllString(h, "\n")
	h = fetchTags.ReplaceAllString(h, " ")
	h = html.UnescapeString(h)
	h = fetchSpaces.ReplaceAllString(h, " ")
	h = fetchLine.ReplaceAllString(h, "\n")
	h = fetchBlank.ReplaceAllString(h, "\n\n")
	return strings.TrimSpace(h)
}
