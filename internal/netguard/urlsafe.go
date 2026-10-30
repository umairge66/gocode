// This file holds the URL-parsing front end of the SSRF check. It keeps its own
// WHATWG-ish URL parsing (ParseURL over net/url) and an http(s) scheme gate,
// then hands the IP/host *safety verdict* — loopback, unspecified,
// link-local-metadata or otherwise-blocked destinations — to IsSafeUpstreamURL,
// the gateway's SSRF guard.

package netguard
