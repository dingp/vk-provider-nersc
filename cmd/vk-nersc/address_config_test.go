package main

import "testing"

func TestResolveNodeAddress(t *testing.T) {
	for _, tc := range []struct {
		name, mode, explicit, pod, want string
		hostname, wantErr               bool
	}{
		{name: "default Pod IP", pod: "10.42.1.2", want: "10.42.1.2"},
		{name: "default loopback", want: "127.0.0.1"},
		{name: "InternalIP explicit priority", mode: "InternalIP", explicit: "10.43.1.2", pod: "10.42.1.2", want: "10.43.1.2"},
		{name: "InternalIP Pod fallback", mode: "InternalIP", pod: "10.42.1.2", want: "10.42.1.2"},
		{name: "Hostname missing explicit with numeric Pod IP", mode: "Hostname", pod: "10.42.1.2", wantErr: true},
		{name: "Hostname missing both", mode: "Hostname", wantErr: true},
		{name: "Hostname whitespace explicit", mode: "Hostname", explicit: " \t", pod: "10.42.1.2", wantErr: true},
		{name: "Hostname DNS name", mode: "Hostname", explicit: "provider.example", pod: "10.42.1.2", wantErr: true},
		{name: "Hostname malformed IP", mode: "Hostname", explicit: "999.0.0.1", wantErr: true},
		{name: "Hostname IPv4", mode: "Hostname", explicit: "10.43.1.2", pod: "10.42.1.2", want: "10.43.1.2", hostname: true},
		{name: "Hostname IPv6", mode: "Hostname", explicit: "2001:db8::1", want: "2001:db8::1", hostname: true},
		{name: "Hostname trimmed explicit", mode: "Hostname", explicit: " 10.43.1.2 ", want: "10.43.1.2", hostname: true},
		{name: "unknown mode", mode: "ExternalIP", explicit: "10.43.1.2", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, hostname, err := resolveNodeAddress(tc.mode, tc.explicit, tc.pod)
			if (err != nil) != tc.wantErr || address != tc.want || hostname != tc.hostname {
				t.Fatalf("address=%q hostname=%v error=%v", address, hostname, err)
			}
		})
	}
}
