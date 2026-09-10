package eval

import "testing"

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want string // rendered back through pathString
	}{
		{"a", "a"},
		{"status.atProvider.principalId", "status.atProvider.principalId"},
		{`metadata.annotations["crossplane.io/external-name"]`, `metadata.annotations["crossplane.io/external-name"]`},
		{`metadata.annotations['single.quoted']`, `metadata.annotations["single.quoted"]`},
		{"spec.rules[0].host", "spec.rules[0].host"},
		{"items[2]", "items[2]"},
		// Rendering is canonical: a bracketed key that is a legal identifier
		// comes back in dotted form, because the two mean the same thing.
		{`a["b"].c[0]["d.e"]`, `a.b.c[0]["d.e"]`},
	}

	for _, tc := range cases {
		segs, err := parsePath(tc.in)
		if err != nil {
			t.Errorf("parsePath(%q): %v", tc.in, err)
			continue
		}
		if got := pathString(segs); got != tc.want {
			t.Errorf("parsePath(%q) rendered as %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParsePathErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"a.",
		".a",
		"a..b",
		`a["unterminated`,
		"a[1",
		"a[notanumber]",
		`a["x"b`,
	} {
		if _, err := parsePath(in); err == nil {
			t.Errorf("parsePath(%q) should have failed", in)
		}
	}
}

func TestResolvePathReportsHowFarItGot(t *testing.T) {
	root, err := toStarlark(map[string]any{
		"status": map[string]any{
			"atProvider": map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	segs, err := parsePath("status.atProvider.id")
	if err != nil {
		t.Fatal(err)
	}
	_, ok, resolved := resolvePath(root, segs)
	if ok {
		t.Fatal("path should not resolve")
	}
	// status and atProvider resolved; id did not. Naming the failing link is
	// what makes the Waiting condition point at the field being waited on.
	if resolved != 2 {
		t.Errorf("resolved %d segments, want 2", resolved)
	}
	if got := pathString(segs[:resolved+1]); got != "status.atProvider.id" {
		t.Errorf("failing path = %q", got)
	}
}

func TestResolvePathNegativeIndex(t *testing.T) {
	root, err := toStarlark(map[string]any{"items": []any{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	segs, err := parsePath("items[-1]")
	if err != nil {
		t.Fatal(err)
	}
	v, ok, _ := resolvePath(root, segs)
	if !ok {
		t.Fatal("negative index should resolve from the end")
	}
	if v.String() != `"c"` {
		t.Errorf("got %s, want c", v.String())
	}
}
