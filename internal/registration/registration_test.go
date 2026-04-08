package registration

import "testing"

func TestProxyNumFromSubnet(t *testing.T) {
	t.Parallel()

	got, err := ProxyNumFromSubnet("10.42.0.0/16")
	if err != nil {
		t.Fatalf("ProxyNumFromSubnet() error = %v", err)
	}
	if got != 42 {
		t.Fatalf("ProxyNumFromSubnet() = %d, want 42", got)
	}
}

func TestProxyNumFromSubnetRejectsInvalid(t *testing.T) {
	t.Parallel()

	if _, err := ProxyNumFromSubnet("10.42.1.0/24"); err == nil {
		t.Fatal("ProxyNumFromSubnet() error = nil, want error")
	}
}
