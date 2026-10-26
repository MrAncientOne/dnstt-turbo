package main

import (
	"fmt"
	"testing"

	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

func TestMTUAdmissionAndSeparateListeners(t *testing.T) {
	for _, tc := range []struct {
		server, advertised int
		accept             bool
	}{
		{16384, 512, false}, {16384, 1232, false}, {16384, 4096, true},
		{1232, 1232, true}, {512, 512, true},
	} {
		t.Run(fmt.Sprintf("listener%d/client%d", tc.server, tc.advertised), func(t *testing.T) {
			q := testQuery(t, testDomain(t), 1, clientPayload(turbotunnel.ClientID{}, []byte("upstream")), uint16(tc.advertised))
			resp, payload, _ := responseFor(q, testDomain(t), tc.server)
			if (resp.Rcode() == dns.RcodeNoError) != tc.accept {
				t.Fatalf("rcode=%d", resp.Rcode())
			}
			if !tc.accept {
				if payload != nil {
					t.Fatal("rejected query admitted upstream data")
				}
				return
			}
			mtu := min(kcpMTUCap, computeMaxEncodedPayload(tc.server)-2)
			// Model the worst allowed name rather than depending on the much
			// shorter test domain's spare space.
			if computeMaxEncodedPayload(tc.advertised) < mtu+2 {
				t.Fatal("accepted cap cannot carry segment")
			}
		})
	}
}
