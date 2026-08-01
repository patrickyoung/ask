package auth

import (
	"fmt"
	"net"
	"net/url"
)

// CheckTokenURL rejects a token endpoint that would carry credentials in
// the clear.
//
// The body of a token request holds a client secret or a refresh token —
// the long-lived halves, the ones worth stealing. Over http they are on the
// wire in plaintext for every hop in between to read, and a gateway inside
// a corporate network is still reached across a network. This is the same
// hole the redirect rule closes (see tokenClient), approached from the
// front: there is no point refusing to be redirected onto a cleartext hop
// if the first hop was already one.
//
// Loopback is the exception, and only loopback. A token endpoint on this
// machine never reaches a wire, which is what lets a developer stand one up
// — and what lets this repository's tests run against httptest without
// anybody minting a certificate.
//
// The check is worth making where the URL is configured rather than only
// where it is used: a mistake in a deploy script should be an error the
// first time it runs, not the first time a token happens to expire.
func CheckTokenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("token endpoint %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if loopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("token endpoint %s is http: a client secret or refresh "+
			"token would travel in the clear. Use https (http is allowed only on loopback)", raw)
	case "":
		return fmt.Errorf("token endpoint %q has no scheme; use https://", raw)
	default:
		return fmt.Errorf("token endpoint %q uses scheme %q; use https://", raw, u.Scheme)
	}
}

// loopback reports whether a host names this machine. The literal name and
// the address families are all there is to accept: a hostname that merely
// resolves to 127.0.0.1 today is a DNS answer, not a property of the URL,
// and it can resolve elsewhere tomorrow.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
