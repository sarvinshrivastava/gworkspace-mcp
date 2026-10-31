package remote

import "testing"

func TestHost(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:5":        "1.2.3.4",
		"172.17.0.1:3015":  "172.17.0.1",
		"[::1]:5":          "::1",
		"[fe80::1%eth0]:9": "fe80::1%eth0",
		"no-port":          "no-port",
		"":                 "",
	}
	for in, want := range cases {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}
