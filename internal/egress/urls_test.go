package egress

import (
	"net/url"
	"strings"
	"testing"
)

func TestProxyFormatsPreserveProviderCredentials(t *testing.T) {
	for _, tc := range []struct{ raw, scheme, host, user, password string }{
		{"socks5h://us.account-A:p%40ss%3Aword@proxy.example:1080", "socks5h", "proxy.example:1080", "us.account-A", "p@ss:word"},
		{"http://custom-group.account-B:p%23ss@proxy.example:8080", "http", "proxy.example:8080", "custom-group.account-B", "p#ss"},
		{"proxy.example:8080:us.account-A:secret", "http", "proxy.example:8080", "us.account-A", "secret"},
		{"us.account-A:secret:proxy.example:1080", "socks5h", "proxy.example:1080", "us.account-A", "secret"},
		{"us.account-A:secret@proxy.example:1080", "socks5h", "proxy.example:1080", "us.account-A", "secret"},
		{"proxy.example:1080@us.account-A:secret", "socks5h", "proxy.example:1080", "us.account-A", "secret"},
		{"socks5h://proxy.example:1080:us.account-A:secret", "http", "proxy.example:1080", "us.account-A", "secret"},
		{"proxy.example|8080|user|p:ass", "http", "proxy.example:8080", "user", "p:ass"},
		{"user,secret,proxy.example,1080", "socks5h", "proxy.example:1080", "user", "secret"},
		{"[2001:db8::1]:1080", "socks5h", "[2001:db8::1]:1080", "", ""},
		{"https://proxy.example", "http", "proxy.example:443", "", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := Normalize(tc.raw, tc.scheme)
			if err != nil {
				t.Fatal(err)
			}
			user, password := "", ""
			if u.User != nil {
				user = u.User.Username()
				password, _ = u.User.Password()
			}
			if u.Host != tc.host || user != tc.user || password != tc.password {
				t.Fatalf("host=%s user=%s password=%q", u.Host, user, password)
			}
			if strings.Contains(tc.raw, "socks5h://") && u.Scheme != "socks5h" {
				t.Fatal("explicit protocol was lost")
			}
		})
	}
}

func TestProxyFormatRejectsInvalidAndAmbiguousValues(t *testing.T) {
	for _, raw := range []string{"ftp://proxy.example:21", "http://proxy.example:0", "http://proxy.example:65536", "http://proxy.example:", "http://proxy.example/path", "http://user:p%0Ass@proxy.example", "one.example:1234:two.example:1234", "not a proxy", "socks5h://", "http://proxy.example?x=y"} {
		if _, err := Normalize(raw, "http"); err == nil {
			t.Errorf("accepted invalid/ambiguous proxy %q", raw)
		}
	}
}

func TestProxyImportTextJSONAndDeduplication(t *testing.T) {
	for _, text := range []string{
		"http://user:secret@proxy.example:8080\nproxy.example:8080:user:secret\nwrong-line\nsocks5h://us.a:secret@proxy.example:1080",
		`{"data":{"proxies":[{"host":"proxy.example","port":8080,"username":"user","password":"secret","type":"http"},"http://user:secret@proxy.example:8080","wrong-line",{"host":"proxy.example","port":1080,"user":"us.a","pass":"secret","protocol":"socks5h"}]}}`,
	} {
		urls, problems := ExtractURLs(text)
		if len(urls) != 2 || len(problems) != 1 {
			t.Fatalf("urls=%v problems=%v", urls, problems)
		}
		if strings.Contains(strings.Join(problems, " "), "secret") {
			t.Fatal("diagnostic exposes proxy credentials")
		}
	}
	urls, problems := ExtractURLs(`[代理](socks5h://group.a:secret@proxy.example:1080)`)
	if len(problems) > 0 || len(urls) != 1 || strings.HasSuffix(urls[0], ")") {
		t.Fatalf("Markdown import failed: %v %v", urls, problems)
	}
}

func TestProxyAccountGenerationKeepsCustomCountryGroup(t *testing.T) {
	for _, group := range []string{"sg", "us", "my-custom-group"} {
		urls, err := StickyURLs("socks5h://"+group+":secret@proxy.example:1080", 3, "account")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, raw := range urls {
			u, _ := url.Parse(raw)
			password, _ := u.User.Password()
			if !strings.HasPrefix(u.User.Username(), group+".account-") || password != "secret" || seen[raw] {
				t.Fatalf("provider format changed: %s", raw)
			}
			seen[raw] = true
		}
	}
	urls, err := StickyURLs("http://group-{account}:secret@proxy.example:8080", 1, "acct")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(urls[0])
	if !strings.HasPrefix(u.User.Username(), "group-acct-") || strings.Contains(u.User.Username(), ".") {
		t.Fatal("custom account template was ignored")
	}
}
