package proxy

import (
	"bytes"
	"encoding/base64"
	"net"
	"net/http"

	acr "github.com/coyove/goflyway/pkg/aclrouter"
)

const (
	ruleProxy = iota
	rulePass
	ruleBlock
)

type Rule struct {
	IP     string
	Ans    byte
	OldAns byte
	R      byte
}

func (proxy *ProxyClient) canDirectConnect(host string) (r byte, ext string) {
	host, _ = splitHostPort(host)

	if c, ok := proxy.DNSCache.Get(host); ok && c.(*Rule) != nil {
		switch c.(*Rule).Ans {
		case ruleProxy:
			return ruleProxy, "Proxy (cached)"
		case rulePass:
			return rulePass, "Pass (cached)"
		case ruleBlock:
			return ruleBlock, "Block (cached)"
		default:
			panic("?")
		}
	}

	rule, ipstr, err := proxy.ACL.Check(host, !proxy.ACL.RemoteDNS)
	proxy.Logger.If(err != nil).Errorf("ACL check error: %v", err)

	priv := false
	defer func() {
		if proxy.Policy.IsSet(PolicyGlobal) && !priv {
			r = ruleProxy
			ext = "Global"
		} else {
			proxy.DNSCache.Add(host, &Rule{ipstr, r, r, rule})
		}
	}()

	switch rule {
	case acr.RuleIPv6:
		return ruleProxy, "Proxy (IPv6)" // By default we proxy IPv6 destination
	case acr.RuleMatchedPass:
		return rulePass, "Pass"
	case acr.RuleProxy, acr.RuleMatchedProxy:
		return ruleProxy, "Proxy"
	case acr.RuleBlock:
		return ruleBlock, "Block"
	case acr.RulePrivate:
		priv = true
		return rulePass, "Private IP"
	case acr.RulePass:
		if !proxy.ACL.RemoteDNS {
			return rulePass, "Pass (trust local DNS)"
		}
		r = rulePass
	default:
		r = ruleProxy
	}

	if proxy.Policy.IsSet(PolicyGlobal) {
		return
	}

	// We have doubts, so query the upstream
	cr := proxy.Cipher.newRequest()
	cr.Opt.Set(doDNS)
	cr.Real = "dns"
	cr.Auth = proxy.UserAuth
	cr.Query = host

	dnsloc := "http://" + proxy.Upstream
	trueloc := "http://" + proxy.genHost() + "/" + proxy.encryptClientRequest(cr)

	if !proxy.Policy.IsSet(PolicyForward) {
		dnsloc = trueloc
	}

	req, _ := http.NewRequest("GET", dnsloc, nil)
	trans := proxy.tpq

	if proxy.Policy.IsSet(PolicyAgent) {
		req, _ = http.NewRequest("GET", proxy.agentUpstream(), nil)
		req.Header.Add(fwdURLHeader, host+",dns")
		trans = proxy.tpd
	}

	if proxy.Policy.IsSet(PolicyForward) {
		req.Header.Add(fwdURLHeader, trueloc)
	}

	resp, err := trans.RoundTrip(req)
	if err != nil {
		if e, _ := err.(net.Error); e != nil && e.Timeout() {
			// proxy.tpq.Dial = (&net.Dialer{Timeout: 2 * time.Second}).Dial
			return r, "Timeout"
		}
		// 	proxy.Logger.Errorf("ACL", err)
		// }
		return r, "Network error: " + err.Error()
	}

	tryClose(resp.Body)
	ip, err := base64.StdEncoding.DecodeString(resp.Header.Get(dnsRespHeader))
	if parts := bytes.Split(ip, []byte(".")); len(parts) == 4 {
		ipstr = string(ip)
		goto ACL_CHECK
	}

	if err != nil || ip == nil || len(ip) != net.IPv4len {
		return r, "Bad response"
	}

	ipstr = net.IP(ip).String()

ACL_CHECK:
	switch rule, _, _ = proxy.ACL.Check(ipstr, true); rule {
	case acr.RulePass, acr.RuleMatchedPass:
		return rulePass, "Pass (by remote)"
	case acr.RuleProxy, acr.RuleMatchedProxy:
		return ruleProxy, "Proxy (by remote)"
	case acr.RuleBlock:
		return ruleBlock, "Block (by remote)"
	case acr.RulePrivate:
		return ruleProxy, "Private IP (by remote)"
	default:
		return ruleProxy, "Unknown"
	}
}

func (proxy *ProxyServer) isBlocked(host string) (bk bool) {
	if proxy.ACL == nil {
		return false
	}

	host, _ = splitHostPort(host)
	if c, ok := proxy.ACLCache.Get(host); ok {
		return c.(bool)
	}

	rule, _, err := proxy.ACL.Check(host, true)
	proxy.Logger.If(err != nil).Errorf("ACL check error: %v", err)

	bk = rule == acr.RuleBlock
	proxy.ACLCache.Add(host, bk)
	return
}
