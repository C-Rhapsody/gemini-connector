package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func telegramTransport(t *testing.T, rawProxyURL string) *http.Transport {
	t.Helper()
	client, err := newTelegramHTTPClient(rawProxyURL)
	if err != nil {
		t.Fatalf("newTelegramHTTPClient(%q): %v", rawProxyURL, err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", client.Transport)
	}
	return transport
}

func TestNewTelegramHTTPClient_DirectIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")

	transport := telegramTransport(t, "")
	if transport.Proxy != nil {
		t.Fatal("direct Telegram client must not use environment proxy settings")
	}
}

func TestNewTelegramHTTPClient_HTTPProxy(t *testing.T) {
	transport := telegramTransport(t, "http://user:secret@127.0.0.1:8080")
	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.String() != "http://user:secret@127.0.0.1:8080" {
		t.Fatalf("unexpected proxy URL: %v", proxyURL)
	}
}

func TestNewTelegramHTTPClient_AcceptsSupportedSchemes(t *testing.T) {
	for _, scheme := range []string{"https", "socks5", "socks5h", "SOCKS5H"} {
		t.Run(scheme, func(t *testing.T) {
			transport := telegramTransport(t, scheme+"://user:secret@127.0.0.1:1080")
			if scheme == "https" {
				if transport.Proxy == nil {
					t.Fatal("HTTPS proxy was not installed")
				}
				return
			}
			if transport.Proxy != nil {
				t.Fatal("SOCKS5 transport must not use HTTP proxy mode")
			}
			if transport.DialContext == nil {
				t.Fatal("SOCKS5 dialer was not installed")
			}
		})
	}
}

func TestNewTelegramHTTPClient_RejectsInvalidProxyURLs(t *testing.T) {
	for _, rawProxyURL := range []string{
		"",
		"ftp://127.0.0.1:8080",
		"http://",
		"socks5://",
		"://not-a-url",
	} {
		if rawProxyURL == "" {
			continue
		}
		if _, err := newTelegramHTTPClient(rawProxyURL); err == nil {
			t.Errorf("newTelegramHTTPClient(%q) accepted invalid proxy URL", rawProxyURL)
		}
	}
}

func TestTelegramProxyLogValueRedactsPassword(t *testing.T) {
	u, err := url.Parse("socks5://user:secret@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	redacted := u.Redacted()
	if strings.Contains(redacted, "secret") || !strings.Contains(redacted, "xxxxx") {
		t.Fatalf("proxy password was not redacted: %q", redacted)
	}
	if !strings.Contains(redacted, "user:xxxxx@127.0.0.1:1080") {
		t.Fatalf("proxy endpoint was unexpectedly changed: %q", redacted)
	}
}
