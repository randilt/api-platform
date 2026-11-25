package memlimit

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]struct {
		n  int64
		ok bool
	}{
		"1073741824\n":        {1 << 30, true},
		"max\n":               {0, false},
		"":                    {0, false},
		"9223372036854771712": {0, false}, // cgroup v1 "unlimited"
		"-1":                  {0, false},
		"abc":                 {0, false},
		"  536870912  ":       {512 << 20, true},
	}
	for in, want := range cases {
		n, ok := parse(in)
		if n != want.n || ok != want.ok {
			t.Errorf("parse(%q) = %d, %v; want %d, %v", in, n, ok, want.n, want.ok)
		}
	}
}

func TestCheck(t *testing.T) {
	if err := Check(1<<30, 8, 64<<20); err != nil {
		t.Errorf("8 × 64 MiB + 128 MiB fits in 1 GiB: %v", err)
	}
	err := Check(1<<30, 16, 64<<20)
	if err == nil || !strings.Contains(err.Error(), "1152 MiB") || !strings.Contains(err.Error(), "1024 MiB") {
		t.Errorf("16 × 64 MiB + 128 MiB does not fit in 1 GiB: %v", err)
	}
	if err := Check(Base, 0, 64<<20); err != nil {
		t.Errorf("no runs needs only the base: %v", err)
	}
}

func TestGoLimit(t *testing.T) {
	if got := GoLimit(1 << 30); got != (1<<30)/100*85 {
		t.Errorf("GoLimit = %d", got)
	}
}
