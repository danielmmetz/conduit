package client

import "testing"

func TestWsURLForValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"http://localhost:8080", "ws://localhost:8080/ws"},
		{"https://example.com", "wss://example.com/ws"},
		{"ws://example.com/", "ws://example.com/ws"},
		{"wss://example.com/base", "wss://example.com/base/ws"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := wsURLFor(c.in)
			if err != nil {
				t.Fatalf("wsURLFor(%q) err = %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("wsURLFor(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestWsURLForInvalid(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"example.com", "ftp://example.com"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := wsURLFor(in)
			if err == nil {
				t.Fatalf("wsURLFor(%q) = %q, want error", in, got)
			}
		})
	}
}
