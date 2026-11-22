package main

import "testing"

func TestLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:6060", "127.0.0.1:0", "[::1]:6060", "127.5.4.3:80"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{
		":6060", "0.0.0.0:6060", "[::]:6060", "localhost:6060", "10.0.0.5:6060",
		"192.168.1.2:6060", "example.com:6060", "127.0.0.1", "6060", "", "127.0.0.1:",
	} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
