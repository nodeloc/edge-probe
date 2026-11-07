package agent

import "testing"

func TestDefaultInterfacePicksLowestMetricDefaultRoute(t *testing.T) {
	var routes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	0101A8C0	0003	0	0	600	00000000	0	0	0
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
down0	00000000	0101A8C0	0002	0	0	1	00000000	0	0	0
`
	if name := defaultInterface(routes); name != "eth0" {
		t.Fatalf("got %q", name)
	}
	if name := defaultInterface("Iface\tDestination\n"); name != "" {
		t.Fatalf("no default route, got %q", name)
	}
}
