package tsig

import (
	"encoding/hex"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/miekg/dns"
	"net"
	"strings"
	"time"
)

const (
	// GSS is the RFC 3645 defined algorithm name
	GSS = "gss-tsig."
)

const (
	_ uint16 = iota // Reserved, RFC 2930, section 2.5
	// TkeyModeServer is used for server assigned keying
	TkeyModeServer
	// TkeyModeDH is used for Diffie-Hellman exchanged keying
	TkeyModeDH
	// TkeyModeGSS is used for GSS-API establishment
	TkeyModeGSS
	// TkeyModeResolver is used for resolver assigned keying
	TkeyModeResolver
	// TkeyModeDelete is used for key deletion
	TkeyModeDelete
)

// ExchangeTKEY exchanges TKEY records with the given host using the given
// key name, algorithm, mode, and lifetime with the provided input payload.
// Any additional DNS records are also sent and the exchange can be secured
// with TSIG if a key name, algorithm and MAC are provided.
// The TKEY record is returned along with any other DNS records in the
// response along with any error that occurred.
func ExchangeTKEY(host, keyname, algorithm string, mode uint16, lifetime uint32, input []byte, extra []dns.RR, tsigname, tsigalgo, tsigmac *string) (*dns.TKEY, []dns.RR, error) {

	client := &dns.Client{
		Net: "tcp",
	}

	// nsupdate(1) intentionally ignores the TSIG on the TKEY response for GSS
	if strings.ToLower(algorithm) == GSS {
		client.TsigAlgorithm = map[string]*dns.TsigAlgorithm{GSS: {nil, nil}}
		client.TsigSecret = map[string]string{keyname: ""}
	}

	msg := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			RecursionDesired: false,
		},
		Question: make([]dns.Question, 1),
		Extra:    make([]dns.RR, 1),
	}

	msg.Question[0] = dns.Question{
		Name:   keyname,
		Qtype:  dns.TypeTKEY,
		Qclass: dns.ClassANY,
	}

	msg.Id = dns.Id()

	now := time.Now().Unix()

	var inception, expiration uint32
	switch mode {
	case TkeyModeDH:
		fallthrough
	case TkeyModeGSS:
		inception = uint32(now)
		expiration = uint32(now) + lifetime
	case TkeyModeDelete:
		inception = 0
		expiration = 0
	default:
		return nil, nil, fmt.Errorf("Unsupported TKEY mode %d", mode)
	}

	msg.Extra[0] = &dns.TKEY{
		Hdr: dns.RR_Header{
			Name:   keyname,
			Rrtype: dns.TypeTKEY,
			Class:  dns.ClassANY,
			Ttl:    0,
		},
		Algorithm:  algorithm,
		Mode:       mode,
		Inception:  inception,
		Expiration: expiration,
		KeySize:    uint16(len(input)),
		Key:        hex.EncodeToString(input),
	}

	msg.Extra = append(msg.Extra, extra...)

	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, nil, err
	}

	if strings.ToLower(algorithm) != GSS && tsigname != nil && tsigalgo != nil && tsigmac != nil {
		client.TsigSecret = map[string]string{*tsigname: *tsigmac}
		msg.SetTsig(*tsigname, *tsigalgo, 300, time.Now().Unix())
	}

	var rr *dns.Msg
	var errs error
	for _, addr := range addrs {

		// Every time we send the message the TSIG RR gets dropped
		copied := msg

		rr, _, err = client.Exchange(copied, net.JoinHostPort(addr, "53"))
		if err == nil || rr != nil {
			break
		}
		errs = multierror.Append(errs, err)
	}

	if rr == nil {
		return nil, nil, errs
	}

	if rr.Rcode != dns.RcodeSuccess {
		return nil, nil, fmt.Errorf("DNS error: %s (%d)", dns.RcodeToString[rr.Rcode], rr.Rcode)
	}

	additional := []dns.RR{}

	var tkey *dns.TKEY

	for _, ans := range rr.Answer {
		switch t := ans.(type) {
		case *dns.TKEY:
			// There mustn't be more than one TKEY answer RR
			if tkey != nil {
				return nil, nil, fmt.Errorf("Multiple TKEY responses")
			}
			tkey = t
		default:
			additional = append(additional, ans)
		}
	}

	// There should always be at least a TKEY answer RR
	if tkey == nil {
		return nil, nil, fmt.Errorf("Received no TKEY response")
	}

	if tkey.Error != 0 {
		return nil, nil, fmt.Errorf("TKEY error: %s (%d)", dns.RcodeToString[int(tkey.Error)], tkey.Error)
	}

	return tkey, additional, nil
}
