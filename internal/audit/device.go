package audit

import (
	"net"
	"strings"
)

// Device turns a User-Agent header into the few words a person recognises:
// "Chrome บน Windows", "Safari บน iPhone". The raw header stays in the row —
// this is only how it is read back — so nothing is lost by getting an unusual
// browser slightly wrong.
//
// Order matters throughout: every Chromium browser also says "Chrome" and
// "Safari", and every iOS browser says "Mac OS X", so the specific names are
// tested before the general ones.
func Device(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	l := strings.ToLower(ua)

	// Things that are not a person's browser at all.
	for _, p := range []string{"curl/", "wget/", "go-http-client", "python-requests", "python-urllib",
		"postmanruntime", "okhttp", "node-fetch", "axios/", "undici", "java/"} {
		if strings.Contains(l, p) {
			return "โปรแกรมอัตโนมัติ (ไม่ใช่เบราว์เซอร์)"
		}
	}

	browser := ""
	switch {
	case strings.Contains(l, "edg/"), strings.Contains(l, "edga/"), strings.Contains(l, "edgios/"):
		browser = "Edge"
	case strings.Contains(l, "opr/"), strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "line/"):
		browser = "LINE"
	case strings.Contains(l, "fban"), strings.Contains(l, "fbav"):
		browser = "Facebook"
	case strings.Contains(l, "firefox/"), strings.Contains(l, "fxios/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/"), strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	}

	os := ""
	switch {
	case strings.Contains(l, "iphone"):
		os = "iPhone"
	case strings.Contains(l, "ipad"):
		os = "iPad"
	case strings.Contains(l, "android"):
		os = "Android"
	case strings.Contains(l, "windows"):
		os = "Windows"
	case strings.Contains(l, "mac os x"), strings.Contains(l, "macintosh"):
		os = "macOS"
	case strings.Contains(l, "cros"):
		os = "ChromeOS"
	case strings.Contains(l, "linux"):
		os = "Linux"
	}

	switch {
	case browser != "" && os != "":
		return browser + " บน " + os
	case browser != "":
		return browser
	case os != "":
		return "เบราว์เซอร์บน " + os
	}
	return "อุปกรณ์ที่ไม่รู้จัก"
}

// Network says where an address sits relative to the server, in the three
// buckets that matter to somebody deciding whether a row is odd: the server
// itself, the organisation's own network, or the open internet.
func Network(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	switch {
	case p.IsLoopback():
		return "local"
	case p.IsPrivate(), p.IsLinkLocalUnicast():
		return "private"
	}
	return "public"
}
