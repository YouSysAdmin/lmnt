package netx

import (
	"net/netip"
	"testing"
)

func TestSearchRange(t *testing.T) {
	busy := map[uint16]bool{9000: true, 9002: true, 9003: true}
	probe := func(p uint16) (bool, error) { return !busy[p], nil }

	got, err := searchRange(9000, 3, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got != 9004 {
		t.Errorf("got %d, want 9004", got)
	}

	got, err = searchRange(9000, 1, probe)
	if err != nil || got != 9001 {
		t.Errorf("got %d, %v, want 9001", got, err)
	}
}

func TestSearchRangeDoesNotWrap(t *testing.T) {
	calls := 0
	probe := func(uint16) (bool, error) { calls++; return false, nil }

	_, err := searchRange(65530, 4, probe)
	if err == nil {
		t.Fatal("expected an error")
	}
	// 65530..65532 are the only valid starts for a run of 4.
	if calls > 3 {
		t.Errorf("probe called %d times, the walk wrapped around", calls)
	}
}

func TestFreePortRangeLive(t *testing.T) {
	p, err := FreePortRange(Loopback, 20000, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p < 20000 {
		t.Errorf("got %d below the requested start", p)
	}
}

func TestParseForward(t *testing.T) {
	good := map[string]Forward{
		"8080:80":              {Host: netip.MustParseAddrPort("127.0.0.1:8080"), GuestPort: 80},
		"0.0.0.0:2222:22":      {Host: netip.MustParseAddrPort("0.0.0.0:2222"), GuestPort: 22},
		"[::1]:9000:445":       {Host: netip.MustParseAddrPort("[::1]:9000"), GuestPort: 445},
		"192.168.1.10:21:2121": {Host: netip.MustParseAddrPort("192.168.1.10:21"), GuestPort: 2121},
	}
	for in, want := range good {
		got, err := ParseForward(in)
		if err != nil {
			t.Errorf("ParseForward(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseForward(%q) = %v, want %v", in, got, want)
		}
		if back, err := ParseForward(got.String()); err != nil || back != got {
			t.Errorf("round trip of %q gave %v, %v", in, back, err)
		}
	}

	bad := []string{"", "80", "a:b", "0:80", "80:0", "80:70000", "1.2.3:80:80", "::1:9000:445", "x:1:2"}
	for _, in := range bad {
		if f, err := ParseForward(in); err == nil {
			t.Errorf("ParseForward(%q) = %v, want error", in, f)
		}
	}
}
