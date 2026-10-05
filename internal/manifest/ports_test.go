package manifest

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func raw(binds ...Bind) Port { return Port{Kind: PortRaw, Binds: binds} }

func TestParsePort(t *testing.T) {
	tests := []struct {
		in   string
		want Port
	}{
		{"internal", Port{Kind: PortInternal}},
		{"metrics", Port{Kind: PortMetrics}},
		{"errors.example.com", Port{Kind: PortDomain, Domain: "errors.example.com"}},
		{"Errors.Example.COM", Port{Kind: PortDomain, Domain: "errors.example.com"}},
		{"INTERNAL", Port{Kind: PortInternal}},
		{"tcp", raw(Bind{Proto: "tcp"})},
		{"udp", raw(Bind{Proto: "udp"})},
		{"tcp:7777", raw(Bind{Proto: "tcp", Host: 7777})},
		{"udp:7778", raw(Bind{Proto: "udp", Host: 7778})},
		{"TCP:7777", raw(Bind{Proto: "tcp", Host: 7777})},
	}
	for _, tc := range tests {
		got, err := ParsePort(tc.in)
		if err != nil {
			t.Errorf("ParsePort(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.want.String() || got.Kind != tc.want.Kind {
			t.Errorf("ParsePort(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParsePortErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"tcpp:7777",    // typo in the protocol
		"tcp:0",        // port out of range
		"tcp:70000",    // port out of range
		"tcp:notaport", // not a number
		"http",         // the generated hostname is gone; name one
		"http:8080",    // nor does it take a port
		"internal:80",  // internal takes no port either
	} {
		if got, err := ParsePort(in); err == nil {
			t.Errorf("ParsePort(%q) = %+v, want an error", in, got)
		}
	}
}

// A hostname is the fallback branch, so a mistyped protocol must not be
// quietly accepted as a domain name and then fail at cert issuance.
func TestMistypedProtocolIsNotAHostname(t *testing.T) {
	if _, err := ParsePort("tcp:7777x"); err == nil {
		t.Fatal("expected an error, not a hostname")
	}
}

func TestRawHostPortDefaultsToContainerPort(t *testing.T) {
	p, _ := ParsePort("tcp")
	if got := p.Binds[0].HostPort(7777); got != 7777 {
		t.Errorf("host port = %d, want the container port", got)
	}
	p, _ = ParsePort("tcp:8888")
	if got := p.Binds[0].HostPort(7777); got != 8888 {
		t.Errorf("host port = %d, want the declared one", got)
	}
}

// A list exists for exactly one case: both protocols on one container port.
// YAML has no way to write the same key twice, which is why a list is needed
// at all.
func TestListOfProtocols(t *testing.T) {
	var s Service
	if err := yaml.Unmarshal([]byte("name: api\nimage: i:1\nports:\n  7777: [tcp, udp]\n"), &s); err != nil {
		t.Fatal(err)
	}
	p := s.Ports[7777]
	if p.Kind != PortRaw {
		t.Fatalf("kind = %q, want raw", p.Kind)
	}
	got := strings.Join(p.Protocols(), ",")
	if got != "tcp,udp" {
		t.Errorf("protocols = %q, want tcp,udp", got)
	}
	for _, b := range p.Binds {
		if b.HostPort(7777) != 7777 {
			t.Errorf("%s host port = %d, want 7777", b.Proto, b.HostPort(7777))
		}
	}
}

func TestListWithDistinctHostPorts(t *testing.T) {
	var s Service
	if err := yaml.Unmarshal([]byte("name: api\nimage: i:1\nports:\n  7777: [tcp:7777, udp:7778]\n"), &s); err != nil {
		t.Fatal(err)
	}
	p := s.Ports[7777]
	for _, b := range p.Binds {
		want := map[string]int{"tcp": 7777, "udp": 7778}[b.Proto]
		if b.HostPort(7777) != want {
			t.Errorf("%s host port = %d, want %d", b.Proto, b.HostPort(7777), want)
		}
	}
}

func TestListErrors(t *testing.T) {
	for _, body := range []string{
		"name: x\nimage: i:1\nports:\n  80: [tcp, tcp]\n",   // repeated protocol
		"name: x\nimage: i:1\nports:\n  80: [http, tcp]\n",  // only protocols belong in a list
		"name: x\nimage: i:1\nports:\n  80: [internal]\n",   // ditto
		"name: x\nimage: i:1\nports:\n  80: []\n",           // says nothing
		"name: x\nimage: i:1\nports:\n  80: {proto: tcp}\n", // not a mapping
	} {
		var s Service
		if err := yaml.Unmarshal([]byte(body), &s); err == nil {
			t.Errorf("expected an error for:\n%s", body)
		}
	}
}

// internal is the one reach that is not public, and the whole security model
// leans on that distinction.
func TestPublicAndIngress(t *testing.T) {
	cases := map[string]struct{ public, ingress bool }{
		"internal":           {false, false},
		"metrics":            {false, false},
		"errors.example.com": {true, true},
		"tcp":                {true, false},
	}
	for in, want := range cases {
		p, err := ParsePort(in)
		if err != nil {
			t.Fatal(err)
		}
		if p.Public() != want.public || p.UsesIngress() != want.ingress {
			t.Errorf("%q: public=%v ingress=%v, want %v/%v",
				in, p.Public(), p.UsesIngress(), want.public, want.ingress)
		}
	}
}

func TestUnsetPortKindIsNotPublic(t *testing.T) {
	if (Port{}).Public() {
		t.Error("a port with no kind must fail closed")
	}
}

// A metrics port is a side door: the service is found and health-checked on
// the port it actually serves, and on its metrics port only when that is all
// it listens on.
func TestPrimaryPortSkipsMetrics(t *testing.T) {
	cases := []struct {
		ports string
		want  int
	}{
		{"{2113: metrics, 9000: internal}", 9000},
		{"{2112: metrics, 8080: web.example.com}", 8080},
		{"{2112: metrics}", 2112},
	}
	for _, c := range cases {
		var s Service
		if err := yaml.Unmarshal([]byte("{name: g, image: i:1, ports: "+c.ports+"}"), &s); err != nil {
			t.Fatal(err)
		}
		if got := s.PrimaryPort(); got != c.want {
			t.Errorf("%s: primary port = %d, want %d", c.ports, got, c.want)
		}
		if got := s.MetricsPort(); got == 0 {
			t.Errorf("%s: metrics port not found", c.ports)
		}
	}
}
