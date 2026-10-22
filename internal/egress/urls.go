// Package egress manages persistent account-to-proxy bindings.
package egress

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const MaxProxies = 500

// ParseURL accepts forward HTTP(S) and remote-DNS SOCKS proxy URLs. The stored
// URL retains the provider's username and password exactly after URL decoding.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 4096 {
		return nil, errors.New("代理链接为空或过长")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("代理链接格式无效；特殊字符可用 URL 编码")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	defaultPort := ""
	switch u.Scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	case "socks5", "socks5h":
		defaultPort = "1080"
	default:
		return nil, errors.New("仅支持 http、https、socks5 和 socks5h 代理")
	}
	if u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("请填写完整代理地址：协议://用户名:密码@主机:端口")
	}
	if strings.ContainsAny(u.Hostname(), " \t\r\n\\") {
		return nil, errors.New("代理主机名无效")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("代理端口为空")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("代理端口必须在 1–65535 之间")
	}
	if u.User != nil {
		if strings.ContainsAny(u.User.Username(), "\r\n") {
			return nil, errors.New("代理用户名含无效字符")
		}
		if password, _ := u.User.Password(); strings.ContainsAny(password, "\r\n") {
			return nil, errors.New("代理密码含无效字符")
		}
	}
	u.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), port)
	u.Path, u.RawPath = "", ""
	return u, nil
}

// TransportURL maps socks5h to Go 1.22's SOCKS5 implementation. That
// implementation sends the destination hostname to the proxy without a local
// DNS lookup, which is the socks5h behavior.
func TransportURL(u *url.URL) *url.URL {
	cp := *u
	if cp.Scheme == "socks5h" {
		cp.Scheme = "socks5"
	}
	return &cp
}

func proxyID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "pxy-" + hex.EncodeToString(sum[:8])
}

func maskedURL(raw string) string {
	u, err := ParseURL(raw)
	if err != nil {
		return "无效代理地址"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return u.Scheme + "://***:***@" + u.Host
		} else {
			return u.Scheme + "://***@" + u.Host
		}
	}
	return u.String()
}

// Normalize accepts unambiguous market formats. An omitted protocol uses the
// selection in the import form; ambiguous numeric user/password fields require
// a full URL rather than guessing which value is the port.
func Normalize(raw, defaultScheme string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		if u, err := ParseURL(raw); err == nil {
			return u, nil
		}
		defaultScheme, raw = raw[:i], raw[i+3:]
	}
	if defaultScheme == "" {
		defaultScheme = "http"
	}
	if strings.Contains(raw, "@") {
		left, right, _ := strings.Cut(raw, "@")
		if host, port, err := net.SplitHostPort(left); err == nil && validProxyHost(host) && validPort(port) {
			if _, rightPort, err := net.SplitHostPort(right); err != nil || !validPort(rightPort) {
				if username, password, ok := strings.Cut(right, ":"); ok {
					u := &url.URL{Scheme: defaultScheme, Host: left, User: url.UserPassword(username, password)}
					return ParseURL(u.String())
				}
			}
		}
		return ParseURL(defaultScheme + "://" + raw)
	}
	parts := strings.Split(raw, ":")
	if _, port, err := net.SplitHostPort(raw); err == nil && validPort(port) {
		return ParseURL(defaultScheme + "://" + raw)
	}
	if len(parts) == 4 {
		front := validPort(parts[1]) && validProxyHost(parts[0])
		back := validPort(parts[3]) && validProxyHost(parts[2])
		if front && back {
			return nil, errors.New("代理字段顺序不明确，请改用 协议://用户名:密码@主机:端口")
		}
		var host, port, user, password string
		if front {
			host, port, user, password = parts[0], parts[1], parts[2], parts[3]
		} else if back {
			user, password, host, port = parts[0], parts[1], parts[2], parts[3]
		} else {
			return nil, errors.New("代理字段无效，请使用完整 URL 或 主机:端口:用户名:密码")
		}
		u := &url.URL{Scheme: defaultScheme, Host: net.JoinHostPort(host, port), User: url.UserPassword(user, password)}
		return ParseURL(u.String())
	}
	fields := strings.FieldsFunc(raw, func(ch rune) bool { return ch == '|' || ch == ',' || ch == '\t' })
	if len(fields) == 4 {
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		front := validPort(fields[1]) && validProxyHost(fields[0])
		back := validPort(fields[3]) && validProxyHost(fields[2])
		if front && back {
			return nil, errors.New("代理字段顺序不明确，请改用完整代理 URL")
		}
		if front {
			u := &url.URL{Scheme: defaultScheme, Host: net.JoinHostPort(fields[0], fields[1]), User: url.UserPassword(fields[2], fields[3])}
			return ParseURL(u.String())
		}
		if back {
			u := &url.URL{Scheme: defaultScheme, Host: net.JoinHostPort(fields[2], fields[3]), User: url.UserPassword(fields[0], fields[1])}
			return ParseURL(u.String())
		}
	}
	return nil, errors.New("未识别代理格式；请使用完整 URL，或 主机:端口:用户名:密码")
}

func validPort(value string) bool {
	n, err := strconv.Atoi(value)
	return err == nil && n > 0 && n <= 65535
}
func validProxyHost(value string) bool {
	if net.ParseIP(value) != nil || value == "localhost" {
		return true
	}
	return strings.Contains(value, ".") && !strings.ContainsAny(value, " \t\r\n/@:")
}

var proxyLinkRE = regexp.MustCompile(`(?i)(?:https?|socks5h?)://[^\s<>"` + "`" + `]+`)

// ExtractURLs supports line lists, copied text and JSON wrappers. Invalid lines
// are reported by position; credentials are never included in the diagnostics.
func ExtractURLs(text string, scheme ...string) (urls []string, problems []string) {
	defaultScheme := "http"
	if len(scheme) > 0 && scheme[0] != "" {
		defaultScheme = scheme[0]
	}
	var values []string
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case string:
				values = append(values, v)
			case []any:
				for _, child := range v {
					walk(child)
				}
			case map[string]any:
				host, _ := v["host"].(string)
				if host == "" {
					host, _ = v["server"].(string)
				}
				port := v["port"]
				if port == nil {
					port = v["server_port"]
				}
				if host != "" && port != nil {
					s := defaultScheme
					for _, key := range []string{"scheme", "protocol", "type"} {
						if value, ok := v[key].(string); ok && value != "" {
							s = value
							break
						}
					}
					user, _ := v["username"].(string)
					if user == "" {
						user, _ = v["user"].(string)
					}
					password, _ := v["password"].(string)
					if password == "" {
						password, _ = v["pass"].(string)
					}
					u := &url.URL{Scheme: s, Host: net.JoinHostPort(host, fmt.Sprint(port))}
					if user != "" {
						u.User = url.UserPassword(user, password)
					}
					values = append(values, u.String())
					return
				}
				for _, key := range []string{"url", "proxy", "proxy_url", "server", "proxies", "links", "data", "items"} {
					if child, ok := v[key]; ok {
						walk(child)
					}
				}
			}
		}
		walk(decoded)
	} else {
		values = strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	}
	seen := make(map[string]bool)
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		links := proxyLinkRE.FindAllString(value, -1)
		if len(links) == 0 || (strings.Contains(strings.TrimSpace(value), "://") && !strings.Contains(value, "](") && !strings.ContainsAny(value, " \t")) {
			links = []string{strings.TrimSpace(value)}
		}
		for _, link := range links {
			// JSON strings have already been decoded. For copied Markdown, strip
			// only the closing link delimiter; punctuation inside credentials stays.
			if strings.Contains(value, "](") && strings.HasSuffix(link, ")") {
				link = strings.TrimSuffix(link, ")")
			}
			u, err := Normalize(link, defaultScheme)
			if err != nil {
				problems = append(problems, fmt.Sprintf("第 %d 项：%s", i+1, err))
				continue
			}
			canonical := u.String()
			if !seen[canonical] {
				urls = append(urls, canonical)
				seen[canonical] = true
			}
			if len(urls) > MaxProxies {
				return nil, []string{fmt.Sprintf("一次最多导入 %d 条代理", MaxProxies)}
			}
		}
	}
	return urls, problems
}

// StickyURLs expands a proxy-account template. Without a placeholder, preserve
// the chosen country/group username and append a dot plus a proxy account ID.
func StickyURLs(base string, count int, prefix string) ([]string, error) {
	if count < 1 || count > MaxProxies {
		return nil, fmt.Errorf("粘性链接数量必须在 1–%d 之间", MaxProxies)
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "wb2a"
	}
	if len(prefix) > 24 {
		return nil, errors.New("代理账号前缀最多 24 个字符")
	}
	for _, ch := range prefix {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return nil, errors.New("代理账号前缀仅允许字母、数字、下划线和连字符")
		}
	}
	base = strings.TrimSpace(base)
	base = strings.ReplaceAll(base, "{account}", "{session}")
	template := strings.Contains(base, "{session}")
	parsed, err := ParseURL(strings.ReplaceAll(base, "{session}", "account"))
	if err != nil {
		return nil, err
	}
	if !template && (parsed.User == nil || parsed.User.Username() == "") {
		return nil, errors.New("基础链接需包含代理用户名，或在链接中指定 {account}")
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var random [6]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, errors.New("无法生成代理账号标识")
		}
		session := prefix + "-" + hex.EncodeToString(random[:])
		var raw string
		if template {
			u, err := ParseURL(strings.ReplaceAll(base, "{session}", session))
			if err != nil {
				return nil, err
			}
			raw = u.String()
		} else {
			u := *parsed
			username := parsed.User.Username() + "." + session
			if password, has := parsed.User.Password(); has {
				u.User = url.UserPassword(username, password)
			} else {
				u.User = url.User(username)
			}
			raw = u.String()
		}
		out = append(out, raw)
	}
	return out, nil
}
