package main

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// A saved password or token is never shown again, and a request may ask to
// keep it by leaving the field empty. It is kept only for the address it was
// saved for. Otherwise a person who may configure the extension, and never
// saw the secret, could name a server of their own and have it sent there.

var (
	errPasswordNeedsRetype = errors.New("The broker address changed. Enter the password again: a saved password is used only with the broker it was saved for.")
	errTokenNeedsRetype    = errors.New("The Home Assistant address changed. Enter the token again: a saved token is used only with the server it was saved for.")
	errPlugPasswordRetype  = errors.New("The plug's address changed. Enter its password again: a saved password is used only with the plug it was saved for.")
)

// sameBroker answers whether next would use the MQTT password where saved
// uses it. A switch between the built-in broker and an external one counts
// as a change either way: the password of one is never given to the other.
// Turning TLS off counts as a change too.
func sameBroker(saved, next Settings) bool {
	if saved.MQTTBuiltin || next.MQTTBuiltin {
		return saved.MQTTBuiltin && next.MQTTBuiltin
	}
	return strings.EqualFold(saved.brokerURL(), next.brokerURL())
}

// haOrigin is the scheme, host and port of a Home Assistant URL, or "". A
// default port is left out, as the browser leaves it out.
func haOrigin(raw string) string {
	u, err := url.Parse(normalizeBaseURL(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && !(p == "443" && scheme == "https") && !(p == "80" && scheme == "http") {
		host = net.JoinHostPort(u.Hostname(), p)
	}
	return strings.ToLower(scheme + "://" + host)
}

// keepPassword fills next's MQTT password with the saved one when the request
// asked for that and the broker is the same.
func keepPassword(asked, saved Settings, next *Settings) error {
	if asked.MQTTPassword != "" || !asked.KeepPassword || saved.MQTTPassword == "" {
		return nil
	}
	if !sameBroker(saved, *next) {
		return errPasswordNeedsRetype
	}
	next.MQTTPassword = saved.MQTTPassword
	next.HasPassword = true
	return nil
}

// keepHAToken does for the Home Assistant token what keepPassword does for
// the MQTT password.
func keepHAToken(asked, saved Settings, next *Settings) error {
	if strings.TrimSpace(asked.HAToken) != "" || !asked.KeepHAToken || saved.HAToken == "" {
		return nil
	}
	if haOrigin(saved.HAURL) != haOrigin(next.HAURL) {
		return errTokenNeedsRetype
	}
	next.HAToken = saved.HAToken
	next.HasHAToken = true
	return nil
}

// withoutURL is the cause of a failed HTTP request without the address it
// went to. The address of a Tasmota plug carries its password in the query,
// and the error of net/http names the whole address.
func withoutURL(err error) error {
	if failed, ok := errors.AsType[*url.Error](err); ok && failed.Err != nil {
		return failed.Err
	}
	return err
}

// --- Text that leaves for the event stream or the diagnostics bundle ---
//
// An error text of a plug or a broker may name an address, and one kept
// from an earlier version a password. The pages of the extension show it to
// the people who set that plug up. An event and the diagnostics file go further, so
// their text is filtered first.

const (
	scrubbedSecret  = "[secret]"
	scrubbedAddress = "[address]"
	// maxScrubbedBytes is the most a filtered text holds.
	maxScrubbedBytes = 200
	// minScrubbedWord is the shortest secret or host that is looked for. A
	// shorter one would take ordinary words apart.
	minScrubbedWord = 3
)

var (
	scrubKeyValue = regexp.MustCompile(`(?i)\b(password|passwd|pass|user|token|access_token|auth)=[^&\s"']*`)
	scrubURL      = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']+`)
	scrubIPv6     = regexp.MustCompile(`\[[0-9A-Fa-f:.]*:[0-9A-Za-z:.%]*\](?::[0-9]{1,5})?`)
	scrubMAC      = regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){5}\b`)
	scrubIPv4     = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?::[0-9]{1,5})?\b`)
	// A name the resolver or a certificate check names, a known host or not.
	scrubLookup    = regexp.MustCompile(`\blookup [^\s:]+`)
	scrubCertNames = regexp.MustCompile(`\bvalid for [^,]+(?:, [^,]+)*, not \S+`)
)

// scrubber holds what this install knows to be secret or an address.
type scrubber struct {
	secrets []string
	hosts   []string
}

// hostForms is a host as an error text may name it: as written, without its
// scheme and path, and without its port.
func hostForms(raw string) []string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return nil
	}
	if _, rest, found := strings.Cut(h, "://"); found {
		h = rest
	}
	h, _, _ = strings.Cut(h, "/")
	if at := strings.LastIndex(h, "@"); at >= 0 {
		h = h[at+1:]
	}
	forms := []string{h}
	if name, _, err := net.SplitHostPort(h); err == nil {
		forms = append(forms, name)
	} else if bare := strings.Trim(h, "[]"); bare != h {
		forms = append(forms, bare)
	}
	return forms
}

// scrubber collects the saved secrets and the addresses in use.
func (m *module) scrubber() scrubber {
	s := m.settings()
	out := scrubber{secrets: []string{s.MQTTPassword, s.HAToken}}
	out.hosts = append(out.hosts, hostForms(s.MQTTHost)...)
	out.hosts = append(out.hosts, hostForms(s.brokerURL())...)
	out.hosts = append(out.hosts, hostForms(s.HAURL)...)

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, cfg := range m.configs {
		out.secrets = append(out.secrets, cfg.HTTPPassword)
		out.hosts = append(out.hosts, hostForms(cfg.HTTPHost)...)
	}
	out.hosts = append(out.hosts, hostForms(m.pinnedMQTTHost)...)
	out.hosts = append(out.hosts, hostForms(m.pinnedHAURL)...)
	out.hosts = append(out.hosts, hostForms(m.suggestedHTTPHost)...)
	return out
}

// scrubText is s with no secret and no address in it, on one line, at most
// maxScrubbedBytes long.
func (m *module) scrubText(s string) string {
	if s == "" {
		return ""
	}
	sc := m.scrubber()
	return scrubWith(s, sc.secrets, sc.hosts)
}

func (sc scrubber) text(s string) string {
	if s == "" {
		return ""
	}
	return scrubWith(s, sc.secrets, sc.hosts)
}

// longestFirst is the words of at least minScrubbedWord bytes, each once,
// the longest first, so that a word inside another one is replaced whole.
func longestFirst(words []string) []string {
	seen := make(map[string]bool, len(words))
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < minScrubbedWord || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// lowerASCII lowers A to Z only, so every byte stays where it is.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// replaceFold replaces word in s whatever its case.
func replaceFold(s, word, with string) string {
	low, lowWord := lowerASCII(s), lowerASCII(word)
	if !strings.Contains(low, lowWord) {
		return s
	}
	var out strings.Builder
	for {
		i := strings.Index(low, lowWord)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])
		out.WriteString(with)
		s, low = s[i+len(word):], low[i+len(word):]
	}
}

// scrubWith takes the secrets and the hosts out of s, then everything that
// looks like a credential, an address or a hardware address.
func scrubWith(s string, secrets, hosts []string) string {
	var forms []string
	for _, secret := range secrets {
		forms = append(forms, secret, url.QueryEscape(secret))
	}
	for _, secret := range longestFirst(forms) {
		s = strings.ReplaceAll(s, secret, scrubbedSecret)
	}
	s = scrubKeyValue.ReplaceAllString(s, "${1}="+scrubbedSecret)
	s = scrubURL.ReplaceAllString(s, scrubbedAddress)
	s = scrubLookup.ReplaceAllString(s, "lookup "+scrubbedAddress)
	s = scrubCertNames.ReplaceAllString(s, "valid for "+scrubbedAddress)
	for _, host := range longestFirst(hosts) {
		s = replaceFold(s, host, scrubbedAddress)
	}
	s = scrubIPv6.ReplaceAllString(s, scrubbedAddress)
	s = scrubMAC.ReplaceAllString(s, scrubbedAddress)
	s = scrubIPv4.ReplaceAllString(s, scrubbedAddress)
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	if len(s) > maxScrubbedBytes {
		cut := maxScrubbedBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
