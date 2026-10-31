//go:build !harness && !vspd

package dcr

import "testing"

func TestParseSPVPeers(t *testing.T) {
	got, err := parseSPVPeers(" 89.40.0.147:19560, example.org ,[::1]:1 ", "18555")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"89.40.0.147:19560", "example.org:18555", "[::1]:1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got, _ := parseSPVPeers("", "1"); len(got) != 0 {
		t.Fatal("empty not empty")
	}
	for _, bad := range []string{"host:0", "host:99999", "host:x", ":19560"} {
		if _, err := parseSPVPeers(bad, "1"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
