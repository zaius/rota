package proxy

import (
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"

	"github.com/zaius/rota/core/internal/models"
)

// A gateway is one upstream endpoint fronting a pool of exit IPs, where the
// username picks the exit, e.g. "user-residential-{country}-{session}".
//   - Rota fills {session} with an exit ID per binding and {country} with the
//     request's country, so each binding is its own exit and needs no
//     reservation.
//   - A burned or failed exit is dropped for a new one instead of cooling or
//     failing the gateway, which would take every exit out with it.
func IsGateway(p *models.Proxy) bool {
	return p.Username != nil && strings.Contains(*p.Username, "{session}")
}

func fillsCountry(p *models.Proxy) bool {
	return IsGateway(p) && strings.Contains(*p.Username, "{country}")
}

// newExit returns a random session ID. Webshare accepts only decimal IDs that
// fit in an int64.
func newExit() string { return strconv.FormatInt(rand.Int64(), 10) }

// withExit returns a copy of gateway p that dials exit in country. Without a
// country, "-{country}" disappears, which is Webshare's any-country form.
func withExit(p *models.Proxy, exit, country string) *models.Proxy {
	u := *p.Username
	if country == "" {
		u = strings.ReplaceAll(u, "-{country}", "")
	}
	u = strings.NewReplacer("{session}", exit, "{country}", country).Replace(u)
	c := *p
	c.Username = &u
	return &c
}

// isGatewayFailure reports whether err blames the upstream proxy itself:
// unreachable, rejecting our credentials or account, or misconfigured. Any
// other failure through a gateway blames the exit behind it.
func isGatewayFailure(err error) bool {
	if failure, ok := errors.AsType[*upstreamFailure](err); ok {
		switch failure.reason {
		case "proxy_connect_failed", "proxy_configuration_error", "upstream_proxy_auth_failed", "upstream_proxy_payment_required":
			return true
		}
	}
	// net/http reports a failed dial to the proxy as a "proxyconnect" error.
	op, ok := errors.AsType[*net.OpError](err)
	return ok && op.Op == "proxyconnect"
}
